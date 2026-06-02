package wire

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
	"github.com/cloudtasticdev/basuyudb/engine/internal/version"
)

const (
	sslRequestCode = 80877103 // 1234 5679
	gssEncRequest = 80877104
	protocolV3 = 196608 // 0x00030000
	cancelRequest = 80877102
)

var connCounter atomic.Uint64

// conn is a single client connection.
type conn struct {
	netConn net.Conn
	mr *msgReader
	mw *msgWriter
	srv *Server
	sess *session.Session
	logger *slog.Logger
	// extended-query state (one unnamed statement/portal, milestone-1)
	parsedSQL string
	boundParams []executor.Datum
	paramCount int // number of $N parameters in the parsed statement
	resultFormats []int16 // per-column result format codes from Bind (0 text, 1 binary)
	// explicit (multi-statement) transaction state: tx is non-nil between BEGIN
	// and COMMIT/ROLLBACK; txAborted is set when a statement errored inside it.
	tx *transactions.Txn
	txAborted bool
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	c := &conn{
		netConn: nc,
		mr: newMsgReader(nc),
		mw: newMsgWriter(nc),
		srv: s,
		logger: s.logger.With("remote", nc.RemoteAddr().String()),
	}
	if err := c.startup(); err != nil {
		if !errors.Is(err, io.EOF) {
			c.logger.Debug("startup failed", "err", err)
		}
		return
	}
	c.loop()
	// Roll back any transaction left open when the client disconnects.
	if c.tx != nil {
		_ = c.srv.exec.RollbackExplicit(context.Background(), c.tx)
		c.tx = nil
	}
}

// startup performs SSL negotiation, the startup packet, authentication, and the
// initial parameter/ready handshake.
func (c *conn) startup() error {
	// There may be an SSLRequest/GSSENCRequest before the real startup packet.
	for {
		code, body, err := c.mr.readStartup()
		if err != nil {
			return err
		}
		switch code {
		case sslRequestCode, gssEncRequest:
			// Milestone-1: decline encryption with 'N'; client retries plain.
			if _, err := c.netConn.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		case cancelRequest:
			return io.EOF // ignore cancel on a fresh conn
		case protocolV3:
			return c.handleStartupV3(body)
		default:
			return fmt.Errorf("wire: unsupported startup code %d", code)
		}
	}
}

func (c *conn) handleStartupV3(body []byte) error {
	// body: int32 protocol + (key\0 value\0)* \0
	params := map[string]string{}
	pos := 4
	for pos < len(body) && body[pos] != 0 {
		k := cString(body, &pos)
		v := cString(body, &pos)
		params[k] = v
	}

	namespace := params["database"]
	if namespace == "" {
		namespace = "defaultdb"
	}

	// Authentication.
	authSess, err := c.authenticate(params, namespace)
	if err != nil {
		c.sendError("FATAL", "28000", err.Error())
		return err
	}
	c.sess = session.New(authSess, connCounter.Add(1), params)

	// Post-auth handshake.
	if err := c.mw.send(msgAuthentication, authenticationOk()); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"server_version": version.PGWireServerVersion,
		"server_encoding": "UTF8",
		"client_encoding": "UTF8",
		"DateStyle": "ISO, MDY",
		"integer_datetimes": "on",
		"standard_conforming_strings": "on",
	} {
		if err := c.mw.send(msgParameterStatus, parameterStatus(k, v)); err != nil {
			return err
		}
	}
	if err := c.mw.send(msgBackendKeyData, backendKeyData(int32(c.sess.ConnID()), 0)); err != nil {
		return err
	}
	if err := c.mw.send(msgReadyForQuery, readyForQuery('I')); err != nil {
		return err
	}
	c.logger.Info("connection established",
		"namespace", c.sess.Namespace().String(),
		"branch", c.sess.Branch(),
		"user", params["user"],
	)
	return c.mw.flush()
}

// authenticate runs trust auth in dev mode, otherwise cleartext-password where
// the password is a PassportAuth JWT validated against JWKS.
func (c *conn) authenticate(params map[string]string, namespace string) (auth.Session, error) {
	if c.srv.devMode {
		// Trust auth: no password round-trip. (Gated on BASUYUDB_DEV_MODE.)
		branch := params["options"] // not parsed for branch in milestone-1
		_ = branch
		return auth.DevSession(namespace, "main")
	}

	// Production: request a cleartext password carrying the JWT.
	if err := c.mw.send(msgAuthentication, authenticationCleartext()); err != nil {
		return auth.Session{}, err
	}
	if err := c.mw.flush(); err != nil {
		return auth.Session{}, err
	}
	typ, pbody, err := c.mr.readTyped()
	if err != nil {
		return auth.Session{}, err
	}
	if typ != fMsgPassword {
		return auth.Session{}, fmt.Errorf("expected password message, got %q", typ)
	}
	pos := 0
	jwtTok := cString(pbody, &pos)
	if c.srv.jwks == nil {
		return auth.Session{}, errors.New("authentication unavailable: no JWKS configured")
	}
	return c.srv.jwks.ValidateNamespace(jwtTok)
}

// loop is the main query loop after startup.
func (c *conn) loop() {
	for {
		typ, body, err := c.mr.readTyped()
		if err != nil {
			return
		}
		switch typ {
		case fMsgQuery:
			c.handleSimpleQuery(body)
		case fMsgParse:
			c.handleParse(body)
		case fMsgBind:
			c.handleBind(body)
		case fMsgDescribe:
			c.handleDescribe(body)
		case fMsgExecute:
			c.handleExecuteExtended(body)
		case fMsgSync:
			c.mw.send(msgReadyForQuery, readyForQuery('I'))
			c.mw.flush()
		case fMsgClose:
			c.mw.send('3', nil) // CloseComplete
			c.mw.flush()
		case fMsgTerminate:
			return
		default:
			// Unknown message: report and resync.
			c.sendError("ERROR", "08P01", fmt.Sprintf("unsupported message type %q", typ))
			c.mw.send(msgReadyForQuery, readyForQuery('I'))
			c.mw.flush()
		}
	}
}

// handleSimpleQuery processes a 'Q' message: it may contain a single statement.
func (c *conn) handleSimpleQuery(body []byte) {
	pos := 0
	sql := cString(body, &pos)
	trimmed := strings.TrimSpace(sql)

	if trimmed == "" {
		c.mw.send(msgEmptyQuery, nil)
		c.endQuery()
		return
	}
	if c.handleControl(trimmed) {
		c.endQuery()
		return
	}
	// In an aborted transaction, every command except COMMIT/ROLLBACK is rejected.
	if c.tx != nil && c.txAborted {
		c.sendExecError(&executor.ExecError{SQLSTATE: "25P02", Msg: "current transaction is aborted, commands ignored until end of transaction block"})
		c.endQuery()
		return
	}

	res, err := c.execSQL(trimmed, nil)
	if err != nil {
		if c.tx != nil {
			c.txAborted = true // PG: the transaction block fails as a whole
		}
		c.sendExecError(err)
		c.endQuery()
		return
	}
	c.writeResult(res)
	c.endQuery()
}

// endQuery flushes a ReadyForQuery whose status reflects the transaction state.
func (c *conn) endQuery() {
	c.mw.send(msgReadyForQuery, readyForQuery(c.txStatus()))
	c.mw.flush()
}

// txStatus is the PG ReadyForQuery status byte: 'I' idle, 'T' in a transaction,
// 'E' in a failed transaction block.
func (c *conn) txStatus() byte {
	switch {
	case c.tx == nil:
		return 'I'
	case c.txAborted:
		return 'E'
	default:
		return 'T'
	}
}

// handleControl handles transaction control and no-op session statements,
// returning true when the statement was a control command. BEGIN/COMMIT/
// ROLLBACK drive a real multi-statement transaction.
func (c *conn) handleControl(sql string) bool {
	upper := strings.ToUpper(sql)
	switch {
	case strings.HasPrefix(upper, "BEGIN"), strings.HasPrefix(upper, "START TRANSACTION"):
		if c.tx == nil {
			tx, err := c.srv.exec.BeginExplicit(context.Background(), c.sess)
			if err != nil {
				c.sendExecError(err)
				return true
			}
			c.tx, c.txAborted = tx, false
		}
		c.mw.send(msgCommandComplete, commandComplete("BEGIN"))
		return true
	case strings.HasPrefix(upper, "COMMIT"), strings.HasPrefix(upper, "END"):
		if c.tx != nil {
			var err error
			if c.txAborted {
				err = c.srv.exec.RollbackExplicit(context.Background(), c.tx)
			} else {
				err = c.srv.exec.CommitExplicit(context.Background(), c.tx)
			}
			c.tx, c.txAborted = nil, false
			if err != nil {
				c.sendExecError(err)
				return true
			}
		}
		c.mw.send(msgCommandComplete, commandComplete("COMMIT"))
		return true
	case strings.HasPrefix(upper, "ROLLBACK"), strings.HasPrefix(upper, "ABORT"):
		if c.tx != nil {
			_ = c.srv.exec.RollbackExplicit(context.Background(), c.tx)
			c.tx, c.txAborted = nil, false
		}
		c.mw.send(msgCommandComplete, commandComplete("ROLLBACK"))
		return true
	case strings.HasPrefix(upper, "SET BRANCH"):
		if c.tx != nil {
			c.sendExecError(&executor.ExecError{SQLSTATE: "25001", Msg: "SET branch cannot run inside a transaction block"})
			return true
		}
		if br, ok := parseSetBranch(sql); ok {
			if err := c.sess.SetBranch(br); err != nil {
				c.sendExecError(&executor.ExecError{SQLSTATE: "42501", Msg: err.Error()})
				return true
			}
		}
		c.mw.send(msgCommandComplete, commandComplete("SET"))
		return true
	case strings.HasPrefix(upper, "SET "):
		c.mw.send(msgCommandComplete, commandComplete("SET"))
		return true
	case strings.HasPrefix(upper, "RESET "):
		c.mw.send(msgCommandComplete, commandComplete("RESET"))
		return true
	case strings.HasPrefix(upper, "DISCARD"):
		c.mw.send(msgCommandComplete, commandComplete("DISCARD ALL"))
		return true
	}
	return false
}

// parseSetBranch extracts the branch name from `SET branch [=|TO] <value>`,
// tolerating quotes and a trailing semicolon.
func parseSetBranch(sql string) (string, bool) {
	lower := strings.ToLower(sql)
	idx := strings.Index(lower, "branch")
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSpace(sql[idx+len("branch"):])
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
	if len(rest) >= 3 && strings.EqualFold(rest[:3], "to ") {
		rest = strings.TrimSpace(rest[3:])
	}
	rest = strings.TrimSuffix(strings.TrimSpace(rest), ";")
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "'"), "'")
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "\""), "\"")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// execSQL parses and executes one statement against the executor, joining the
// active explicit transaction when one is open.
func (c *conn) execSQL(sql string, params []executor.Datum) (*executor.Result, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if c.tx != nil {
		ctx = executor.CtxWithTxn(ctx, c.tx)
	}
	return c.srv.exec.Execute(ctx, stmt, c.sess, params)
}

// writeResult emits RowDescription + DataRows + CommandComplete for a result.
func (c *conn) writeResult(res *executor.Result) {
	if len(res.Columns) > 0 {
		c.mw.send(msgRowDescription, rowDescription(res.Columns))
		for _, row := range res.Rows {
			c.mw.send(msgDataRow, dataRow(row))
		}
	}
	c.mw.send(msgCommandComplete, commandComplete(commandTag(res)))
}

// commandTag builds the CommandComplete tag (e.g. "SELECT 1").
func commandTag(res *executor.Result) string {
	switch res.Command {
	case "SELECT":
		return fmt.Sprintf("SELECT %d", len(res.Rows))
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", res.RowsAffected)
	case "UPDATE", "DELETE":
		return fmt.Sprintf("%s %d", res.Command, res.RowsAffected)
	default:
		return res.Command
	}
}

// rowDescription builds a 'T' message body for the given columns.
// parameterDescription builds a 't' message body: the parameter count followed
// by an OID per parameter. OID 0 means "unspecified" — the engine treats all
// bound parameters as text, so it lets the driver infer types.
func parameterDescription(n int) []byte {
	var e builder
	e.int16(int16(n))
	for i := 0; i < n; i++ {
		e.int32(0)
	}
	return e.b
}

func rowDescription(cols []executor.Column) []byte {
	var e builder
	e.int16(int16(len(cols)))
	for _, col := range cols {
		e.str(col.Name)
		e.int32(0) // table OID
		e.int16(0) // column attr number
		e.int32(int32(col.TypeOID))
		e.int16(typeLen(col.TypeOID))
		e.int32(-1) // type modifier
		e.int16(0) // format code: text
	}
	return e.b
}

// dataRow builds a 'D' message body for one row.
func dataRow(row []executor.Datum) []byte {
	var e builder
	e.int16(int16(len(row)))
	for _, cell := range row {
		if cell.Null {
			e.int32(-1)
			continue
		}
		e.int32(int32(len(cell.Text)))
		e.bytes([]byte(cell.Text))
	}
	return e.b
}

// dataRowFmt encodes a row honouring per-column result format codes (text or
// binary) requested in Bind. The engine stores cells as PG text, so binary
// output is produced by converting per the column's type OID.
func dataRowFmt(row []executor.Datum, cols []executor.Column, formats []int16) []byte {
	var e builder
	e.int16(int16(len(row)))
	for i, cell := range row {
		if cell.Null {
			e.int32(-1)
			continue
		}
		if resultFormatFor(formats, i) == 1 {
			b := encodeBinary(cell.Text, colTypeOID(cols, i))
			e.int32(int32(len(b)))
			e.bytes(b)
			continue
		}
		e.int32(int32(len(cell.Text)))
		e.bytes([]byte(cell.Text))
	}
	return e.b
}

func resultFormatFor(formats []int16, i int) int16 {
	switch {
	case len(formats) == 0:
		return 0 // default text
	case len(formats) == 1:
		return formats[0] // one code applies to all columns
	case i < len(formats):
		return formats[i]
	default:
		return 0
	}
}

func colTypeOID(cols []executor.Column, i int) uint32 {
	if i < len(cols) {
		return cols[i].TypeOID
	}
	return executor.OIDText
}

// encodeBinary converts a PG text value to PostgreSQL binary wire format for the
// common scalar types; anything else falls back to its UTF-8 bytes.
func encodeBinary(text string, oid uint32) []byte {
	switch oid {
	case executor.OIDInt4:
		n, _ := strconv.ParseInt(text, 10, 64)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(int32(n)))
		return b[:]
	case executor.OIDInt8:
		n, _ := strconv.ParseInt(text, 10, 64)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		return b[:]
	case executor.OIDFloat8:
		f, _ := strconv.ParseFloat(text, 64)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
		return b[:]
	case executor.OIDBool:
		if text == "t" || text == "true" || text == "1" {
			return []byte{1}
		}
		return []byte{0}
	default:
		return []byte(text)
	}
}

func typeLen(oid uint32) int16 {
	switch oid {
	case executor.OIDBool:
		return 1
	case executor.OIDInt4:
		return 4
	case executor.OIDInt8, executor.OIDFloat8:
		return 8
	default:
		return -1 // variable length
	}
}

// sessionControlTag intercepts session-control statements that milestone-1 does
// not execute against storage but must acknowledge for client compatibility.
func sessionControlTag(sql string) (tag string, handled bool) {
	upper := strings.ToUpper(sql)
	switch {
	case strings.HasPrefix(upper, "SET "):
		return "SET", true
	case strings.HasPrefix(upper, "RESET "):
		return "RESET", true
	case strings.HasPrefix(upper, "BEGIN"), strings.HasPrefix(upper, "START TRANSACTION"):
		return "BEGIN", true
	case strings.HasPrefix(upper, "COMMIT"), strings.HasPrefix(upper, "END"):
		return "COMMIT", true
	case strings.HasPrefix(upper, "ROLLBACK"), strings.HasPrefix(upper, "ABORT"):
		return "ROLLBACK", true
	case strings.HasPrefix(upper, "DISCARD"):
		return "DISCARD ALL", true
	}
	return "", false
}

func (c *conn) sendError(severity, sqlstate, message string) {
	c.mw.send(msgErrorResponse, errorResponse(severity, sqlstate, message))
	c.mw.flush()
}

func (c *conn) sendExecError(err error) {
	sqlstate := "XX000"
	var pe *parser.ParseError
	var ee *executor.ExecError
	switch {
	case errors.As(err, &pe):
		sqlstate = pe.SQLSTATE
	case errors.As(err, &ee):
		sqlstate = ee.SQLSTATE
	}
	c.mw.send(msgErrorResponse, errorResponse("ERROR", sqlstate, err.Error()))
}
