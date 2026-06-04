package wire

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

const (
	sslRequestCode = 80877103 // 1234 5679
	gssEncRequest  = 80877104
	protocolV3     = 196608 // 0x00030000
	cancelRequest  = 80877102
)

var connCounter atomic.Uint64

// serverCursor holds the state of a declared server-side cursor.
type serverCursor struct {
	sql  string
	rows [][]executor.Datum
	cols []executor.Column
	pos  int
}

// conn is a single client connection.
type conn struct {
	netConn net.Conn
	mr      *msgReader
	mw      *msgWriter
	srv     *Server
	sess    *session.Session
	logger  *slog.Logger
	// Extended-query state. The Postgres extended protocol allows many named
	// prepared statements and portals to coexist on a connection (the "" name is
	// the unnamed statement/portal, reused per query). Statement-caching drivers
	// (pgx default mode, Prisma, JDBC, ...) rely on named statements.
	prepared map[string]*preparedStmt
	portals  map[string]*portal
	// explicit (multi-statement) transaction state: tx is non-nil between BEGIN
	// and COMMIT/ROLLBACK; txAborted is set when a statement errored inside it.
	tx        *transactions.Txn
	txAborted bool
	// notifyChs holds the per-channel notification channels registered by LISTEN.
	notifyChs []chan pgNotification
	// cursors holds server-side cursors declared with DECLARE ... CURSOR FOR.
	cursors map[string]*serverCursor
}

const msgNotification = 'A'

// drainNotifications reads from ch and sends NotificationResponse messages
// asynchronously. Exits when ch is closed.
func (c *conn) drainNotifications(ch chan pgNotification) {
	for n := range ch {
		var pid int32 = 1
		if c.sess != nil {
			pid = int32(c.sess.ConnID())
		}
		body := notificationResponse(pid, n.channel, n.payload)
		_ = c.mw.send(msgNotification, body)
		_ = c.mw.flush()
	}
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	c := &conn{
		netConn:  nc,
		mr:       newMsgReader(nc),
		mw:       newMsgWriter(nc),
		srv:      s,
		logger:   s.logger.With("remote", nc.RemoteAddr().String()),
		prepared: map[string]*preparedStmt{},
		portals:  map[string]*portal{},
		cursors:  map[string]*serverCursor{},
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
	// Deregister all LISTEN subscriptions so the goroutines can exit.
	for _, ch := range c.notifyChs {
		c.srv.removeListenerCh(ch)
		close(ch)
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
		case sslRequestCode:
			if c.srv.tlsConfig != nil {
				// Upgrade to TLS.
				if _, err := c.netConn.Write([]byte{'S'}); err != nil {
					return err
				}
				tlsConn := tls.Server(c.netConn, c.srv.tlsConfig)
				if err := tlsConn.Handshake(); err != nil {
					return fmt.Errorf("TLS handshake: %w", err)
				}
				c.netConn = tlsConn
				c.mr = newMsgReader(tlsConn)
				c.mw = newMsgWriter(tlsConn)
			} else {
				if _, err := c.netConn.Write([]byte{'N'}); err != nil {
					return err
				}
			}
			continue
		case gssEncRequest:
			// GSS not supported.
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
	// Send parameter statuses that real Postgres drivers (pgx, quaint/Prisma,
	// pq, JDBC) parse during startup. Missing entries cause Rust drivers to
	// panic/crash, so be generous here.
	for k, v := range map[string]string{
		"server_version":              "15.0",
		"server_encoding":             "UTF8",
		"client_encoding":             "UTF8",
		"DateStyle":                   "ISO, MDY",
		"integer_datetimes":           "on",
		"standard_conforming_strings": "on",
		"TimeZone":                    "UTC",
		"IntervalStyle":               "postgres",
		"is_superuser":                "on",
		"session_authorization":       "postgres",
		"application_name":            "",
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

	// Local SCRAM-SHA-256 path: if the connecting user has a password locally
	// provisioned via CREATE/ALTER ROLE ... PASSWORD, authenticate with SCRAM.
	// Standard PostgreSQL clients (psql, pgx, JDBC, ...) negotiate SCRAM by
	// default, so this is the modern-default path for password-authenticated
	// roles. Users NOT in the local store fall through to the JWT path below,
	// so PassportAuth JWT-as-password auth continues to work unchanged.
	user := params["user"]
	if c.srv.roles != nil {
		if verifier, ok := c.srv.roles.Lookup(user); ok {
			if err := c.runSCRAM(verifier); err != nil {
				return auth.Session{}, err
			}
			return localRoleSession(namespace, user)
		}
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
			c.logger.Debug("loop read error (client disconnect or io error)", "err", err)
			return
		}
		c.logger.Debug("loop recv msg", "type", string([]byte{typ}), "len", len(body))
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
			c.handleClose(body)
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

	// COPY drives its own sub-protocol (CopyIn/CopyOut), so intercept it here.
	if strings.HasPrefix(strings.ToUpper(trimmed), "COPY") {
		if stmt, perr := parser.Parse(trimmed); perr == nil {
			if cp, ok := stmt.(*ast.CopyStmt); ok {
				c.handleCopy(cp)
				c.endQuery()
				return
			}
		}
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
	// CREATE/ALTER ROLE|USER ... PASSWORD 'secret' — provision a local SCRAM
	// credential. Statements WITHOUT a PASSWORD clause are left to fall through
	// to the existing no-op-sentinel handling in normalizeSQLForParsing.
	case (strings.HasPrefix(upper, "CREATE ROLE ") || strings.HasPrefix(upper, "CREATE USER ") ||
		strings.HasPrefix(upper, "ALTER ROLE ") || strings.HasPrefix(upper, "ALTER USER ")) &&
		strings.Contains(upper, "PASSWORD"):
		if name, pw, ok := parseRolePassword(sql); ok {
			if err := c.srv.roles.UpsertPassword(name, pw); err != nil {
				c.sendExecError(&executor.ExecError{SQLSTATE: "42601", Msg: "could not provision role password: " + err.Error()})
				return true
			}
			tag := "CREATE ROLE"
			if strings.HasPrefix(upper, "ALTER") {
				tag = "ALTER ROLE"
			}
			c.mw.send(msgCommandComplete, commandComplete(tag))
			return true
		}
		// PASSWORD present but unparseable (e.g. PASSWORD NULL): fall through to
		// the standard no-op handling so the statement is still accepted.
		return false
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
				err = c.srv.exec.CommitExplicit(context.Background(), c.tx, c.sess)
			}
			c.tx, c.txAborted = nil, false
			if err != nil {
				c.sendExecError(err)
				return true
			}
		}
		c.mw.send(msgCommandComplete, commandComplete("COMMIT"))
		return true
	// ROLLBACK TO SAVEPOINT must be checked before the plain ROLLBACK case.
	case strings.HasPrefix(upper, "ROLLBACK TO SAVEPOINT "), strings.HasPrefix(upper, "ROLLBACK TO "):
		c.mw.send(msgCommandComplete, commandComplete("ROLLBACK"))
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
		// Persist a GUC value for SET name = value / SET name TO value /
		// SET LOCAL name = value, so current_setting() reads it back. SET ROLE,
		// SET TRANSACTION, SET SESSION CHARACTERISTICS, SET CONSTRAINTS, and the
		// SET SCHEMA / TIME ZONE multi-word forms are acknowledged without
		// persisting a custom GUC.
		if c.sess != nil {
			applySetGUC(c.sess, sql)
		}
		c.mw.send(msgCommandComplete, commandComplete("SET"))
		return true
	case strings.HasPrefix(upper, "RESET "):
		if c.sess != nil {
			name := strings.TrimSpace(strings.TrimRight(sql[len("RESET "):], "; "))
			if strings.EqualFold(name, "ALL") {
				c.sess.ResetAllSettings()
			} else if name != "" {
				c.sess.ResetSetting(unquoteGUCName(name))
			}
		}
		c.mw.send(msgCommandComplete, commandComplete("RESET"))
		return true
	case strings.HasPrefix(upper, "DISCARD"):
		c.mw.send(msgCommandComplete, commandComplete("DISCARD ALL"))
		return true
	case strings.HasPrefix(upper, "DEALLOCATE"):
		// Prepared-statement deallocation is a client-side concern here; drivers
		// (pgx, JDBC) send DEALLOCATE ALL on reset. Acknowledge it.
		c.mw.send(msgCommandComplete, commandComplete("DEALLOCATE ALL"))
		return true
	case strings.HasPrefix(upper, "LISTEN "):
		channel := strings.ToLower(strings.Trim(strings.TrimSpace(strings.TrimPrefix(upper, "LISTEN ")), `"'; `))
		ch := c.srv.addListener(channel, c)
		c.notifyChs = append(c.notifyChs, ch)
		go c.drainNotifications(ch)
		c.mw.send(msgCommandComplete, commandComplete("LISTEN"))
		return true
	case strings.HasPrefix(upper, "NOTIFY "):
		rest := strings.TrimSpace(sql[7:])
		parts := strings.SplitN(strings.TrimRight(rest, "; "), ",", 2)
		channel := strings.ToLower(strings.Trim(parts[0], `"' `))
		payload := ""
		if len(parts) > 1 {
			payload = strings.Trim(parts[1], `"' `)
		}
		var pid int32 = 1
		if c.sess != nil {
			pid = int32(c.sess.ConnID())
		}
		c.srv.notify(channel, payload, pid)
		c.mw.send(msgCommandComplete, commandComplete("NOTIFY"))
		return true
	case strings.TrimSpace(upper) == "UNLISTEN *":
		c.srv.removeAllListeners(c)
		c.mw.send(msgCommandComplete, commandComplete("UNLISTEN"))
		return true
	case strings.HasPrefix(upper, "UNLISTEN "):
		channel := strings.ToLower(strings.Trim(strings.TrimSpace(strings.TrimPrefix(upper, "UNLISTEN ")), `"'; `))
		c.srv.removeListener(channel, c)
		c.mw.send(msgCommandComplete, commandComplete("UNLISTEN"))
		return true

	// SAVEPOINT / RELEASE — no nested savepoint storage yet; ack.
	case strings.HasPrefix(upper, "SAVEPOINT "), upper == "SAVEPOINT":
		c.mw.send(msgCommandComplete, commandComplete("SAVEPOINT"))
		return true
	case strings.HasPrefix(upper, "RELEASE SAVEPOINT "), strings.HasPrefix(upper, "RELEASE "):
		c.mw.send(msgCommandComplete, commandComplete("RELEASE"))
		return true

	// LOCK TABLE — advisory; acknowledge without locking.
	case strings.HasPrefix(upper, "LOCK TABLE "), strings.HasPrefix(upper, "LOCK "):
		c.mw.send(msgCommandComplete, commandComplete("LOCK TABLE"))
		return true

	// VACUUM / ANALYZE / REINDEX — maintenance stubs.
	case upper == "VACUUM", strings.HasPrefix(upper, "VACUUM "):
		c.mw.send(msgCommandComplete, commandComplete("VACUUM"))
		return true
	case upper == "ANALYZE", strings.HasPrefix(upper, "ANALYZE "):
		c.mw.send(msgCommandComplete, commandComplete("VACUUM"))
		return true
	case upper == "REINDEX", strings.HasPrefix(upper, "REINDEX "):
		c.mw.send(msgCommandComplete, commandComplete("REINDEX"))
		return true

	// SET CONSTRAINTS { ALL | name [, ...] } { DEFERRED | IMMEDIATE } — control
	// deferred checking of DEFERRABLE constraints for the current transaction.
	case strings.HasPrefix(upper, "SET CONSTRAINTS"):
		all, names, deferred, ok := parseSetConstraints(sql)
		if !ok {
			c.sendExecError(&executor.ExecError{SQLSTATE: "42601", Msg: "syntax error in SET CONSTRAINTS"})
			return true
		}
		// Outside an explicit transaction (c.tx == nil) this is an accepted no-op.
		if err := c.srv.exec.SetConstraints(context.Background(), c.tx, c.sess, all, names, deferred); err != nil {
			c.sendExecError(err)
			return true
		}
		c.mw.send(msgCommandComplete, commandComplete("SET CONSTRAINTS"))
		return true

	// SECURITY LABEL — no-op.
	case strings.HasPrefix(upper, "SECURITY LABEL"):
		c.mw.send(msgCommandComplete, commandComplete("SECURITY LABEL"))
		return true

	// LOAD — extension loading; no-op.
	case upper == "LOAD", strings.HasPrefix(upper, "LOAD '"):
		c.mw.send(msgCommandComplete, commandComplete("LOAD"))
		return true

	// DO — anonymous PL/pgSQL block; no-op.
	case strings.HasPrefix(upper, "DO "), strings.HasPrefix(upper, "DO\n"), strings.HasPrefix(upper, "DO\t"):
		c.mw.send(msgCommandComplete, commandComplete("DO"))
		return true

	// DECLARE cursor.
	case strings.HasPrefix(upper, "DECLARE "):
		rest := strings.TrimSpace(sql[len("DECLARE "):])
		restUpper := strings.ToUpper(rest)
		cursorEnd := strings.Index(restUpper, " CURSOR")
		if cursorEnd > 0 {
			name := strings.Fields(rest[:cursorEnd])[0]
			forIdx := strings.Index(restUpper, " FOR ")
			if forIdx > 0 {
				query := strings.TrimSpace(rest[forIdx+5:])
				if c.cursors == nil {
					c.cursors = map[string]*serverCursor{}
				}
				c.cursors[name] = &serverCursor{sql: query}
			}
		}
		c.mw.send(msgCommandComplete, commandComplete("DECLARE CURSOR"))
		return true

	// FETCH from cursor.
	case strings.HasPrefix(upper, "FETCH "):
		if err := c.handleFetch(upper, sql); err != nil {
			c.sendExecError(err)
		}
		return true

	// CLOSE cursor.
	case strings.HasPrefix(upper, "CLOSE "):
		name := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(sql), "close "))
		name = strings.Trim(name, `"'; `)
		if c.cursors != nil {
			delete(c.cursors, name)
		}
		c.mw.send(msgCommandComplete, commandComplete("CLOSE CURSOR"))
		return true

	// MOVE cursor — acknowledge without moving (single-node, non-scrollable stub).
	case strings.HasPrefix(upper, "MOVE "):
		c.mw.send(msgCommandComplete, commandComplete("MOVE"))
		return true
	}
	return false
}

// handleFetch executes a FETCH statement against a declared cursor.
// Supports: FETCH [FORWARD|BACKWARD|ALL|NEXT|PRIOR|FIRST|LAST|ABSOLUTE|RELATIVE] [count] [FROM|IN] cursor_name
func (c *conn) handleFetch(upper, sql string) error {
	// Parse cursor name: appears after FROM or IN, or as the last word.
	parts := strings.Fields(sql)
	name := ""
	count := 1
	allRows := false
	for i, p := range parts {
		pu := strings.ToUpper(p)
		if pu == "FROM" || pu == "IN" {
			if i+1 < len(parts) {
				name = strings.Trim(parts[i+1], `"'; `)
			}
			break
		}
		if pu == "ALL" {
			allRows = true
		}
		if n, err := strconv.Atoi(p); err == nil && i > 0 {
			count = n
		}
	}
	if name == "" && len(parts) > 1 {
		name = strings.Trim(parts[len(parts)-1], `"'; `)
	}

	cursor, ok := c.cursors[name]
	if !ok {
		return fmt.Errorf("cursor %q does not exist", name)
	}

	// Execute query lazily on first FETCH.
	if cursor.rows == nil && cursor.sql != "" {
		normalized := normalizeSQLForParsing(cursor.sql)
		if normalized == noopSentinel {
			cursor.rows = [][]executor.Datum{}
		} else {
			stmt, err := parser.Parse(normalized)
			if err != nil {
				return err
			}
			ctx := context.Background()
			if c.tx != nil {
				ctx = executor.CtxWithTxn(ctx, c.tx)
			}
			result, err := c.srv.exec.Execute(ctx, stmt, c.sess, nil)
			if err != nil {
				return err
			}
			cursor.rows = result.Rows
			cursor.cols = result.Columns
		}
	}

	// Determine the slice of rows to return.
	var rows [][]executor.Datum
	if allRows {
		rows = cursor.rows[cursor.pos:]
		cursor.pos = len(cursor.rows)
	} else {
		end := cursor.pos + count
		if end > len(cursor.rows) {
			end = len(cursor.rows)
		}
		rows = cursor.rows[cursor.pos:end]
		cursor.pos = end
	}

	// Emit RowDescription + DataRows + CommandComplete.
	if len(cursor.cols) > 0 {
		c.mw.send(msgRowDescription, rowDescription(cursor.cols))
	}
	for _, row := range rows {
		c.mw.send(msgDataRow, dataRow(row))
	}
	c.mw.send(msgCommandComplete, commandComplete(fmt.Sprintf("FETCH %d", len(rows))))
	return nil
}

// parseRolePassword extracts the role name and plaintext password from a
//
//	CREATE ROLE  name [WITH] [LOGIN ...] PASSWORD 'secret' [...]
//	CREATE USER  name [WITH] ...         PASSWORD 'secret' [...]
//	ALTER  ROLE  name [WITH]             PASSWORD 'secret' [...]
//	ALTER  USER  name [WITH]             PASSWORD 'secret' [...]
//
// statement. The password is a single-quoted SQL string literal with '' as the
// escape for an embedded quote. Returns ok=false when no parseable single-quoted
// PASSWORD literal is present (e.g. PASSWORD NULL, or a password passed as an
// identifier), so the caller can fall back to no-op handling. The leading verb
// (CREATE/ALTER) and noun (ROLE/USER) are assumed already matched by the caller.
func parseRolePassword(sql string) (name, password string, ok bool) {
	s := strings.TrimSpace(sql)
	// Skip the verb (CREATE/ALTER) and the noun (ROLE/USER): two leading words.
	fields := strings.Fields(s)
	if len(fields) < 4 {
		return "", "", false
	}
	// fields[0]=CREATE/ALTER, fields[1]=ROLE/USER, fields[2]=role name.
	name = strings.Trim(fields[2], `"`)
	if name == "" {
		return "", "", false
	}

	// Find the PASSWORD keyword (case-insensitive), then the following single-
	// quoted literal. We scan on the original string to preserve the literal's
	// case and content.
	upper := strings.ToUpper(s)
	idx := strings.Index(upper, "PASSWORD")
	if idx < 0 {
		return "", "", false
	}
	rest := s[idx+len("PASSWORD"):]
	// Allow optional ENCRYPTED keyword: PASSWORD is what we matched, but some
	// dialects write "ENCRYPTED PASSWORD" — that still ends here at 'PASSWORD'.
	// Skip whitespace up to the opening quote.
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != '\'' {
		return "", "", false // PASSWORD NULL, or not a string literal.
	}
	i++ // consume opening quote
	var sb strings.Builder
	for i < len(rest) {
		ch := rest[i]
		if ch == '\'' {
			// '' is an escaped single quote; a lone ' closes the literal.
			if i+1 < len(rest) && rest[i+1] == '\'' {
				sb.WriteByte('\'')
				i += 2
				continue
			}
			return name, sb.String(), true
		}
		sb.WriteByte(ch)
		i++
	}
	// Unterminated literal.
	return "", "", false
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

// parseSetConstraints parses
//
//	SET CONSTRAINTS { ALL | name [, name ...] } { DEFERRED | IMMEDIATE }
//
// returning (all, names, deferred, ok). all is true for the ALL form; otherwise
// names lists the (case-folded; schema qualifier stripped to the last segment)
// constraint names. ok is false on a malformed statement.
func parseSetConstraints(sql string) (all bool, names []string, deferred bool, ok bool) {
	s := strings.TrimRight(strings.TrimSpace(sql), "; \t\r\n")
	const prefix = "SET CONSTRAINTS"
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return false, nil, false, false
	}
	rest := strings.TrimSpace(s[len(prefix):])
	upperRest := strings.ToUpper(rest)
	switch {
	case strings.HasSuffix(upperRest, "DEFERRED"):
		deferred = true
		rest = strings.TrimSpace(rest[:len(rest)-len("DEFERRED")])
	case strings.HasSuffix(upperRest, "IMMEDIATE"):
		deferred = false
		rest = strings.TrimSpace(rest[:len(rest)-len("IMMEDIATE")])
	default:
		return false, nil, false, false
	}
	if rest == "" {
		return false, nil, false, false
	}
	if strings.EqualFold(rest, "ALL") {
		return true, nil, deferred, true
	}
	for _, part := range strings.Split(rest, ",") {
		n := strings.TrimSpace(part)
		n = strings.Trim(n, `"`)
		if n == "" {
			return false, nil, false, false
		}
		// Strip an optional schema qualifier; constraint metadata is stored by
		// the bare constraint name.
		if dot := strings.LastIndex(n, "."); dot >= 0 {
			n = n[dot+1:]
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return false, nil, false, false
	}
	return false, names, deferred, true
}

// execSQL parses and executes one statement against the executor, joining the
// active explicit transaction when one is open.
func (c *conn) execSQL(sql string, params []executor.Datum) (*executor.Result, error) {
	// Normalise SQL before parsing: replace constructs our goyacc grammar doesn't
	// support but that map trivially to equivalent supported constructs.
	sql = normalizeSQLForParsing(sql)
	// Statements that we intentionally accept but do not execute.
	if sql == noopSentinel {
		return &executor.Result{Command: "OK"}, nil
	}
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

// parameterDescription builds a 't' message body: parameter count + one OID per
// parameter. We use OID 0 ("unspecified") by default, which tells drivers to
// encode params as text. The oids slice may override specific parameter OIDs
// when the SQL context provides type information (e.g. WHERE oid = $1 → OID 26).
func parameterDescription(n int, oids []uint32) []byte {
	var e builder
	e.int16(int16(n))
	for i := 0; i < n; i++ {
		if i < len(oids) && oids[i] != 0 {
			e.int32(int32(oids[i]))
		} else {
			e.int32(0)
		}
	}
	return e.b
}

// inferParamOIDs infers PostgreSQL type OIDs for query parameters ($1, $2, …)
// using lightweight SQL pattern matching. This prevents recursive type-resolution
// loops in clients like quaint/Prisma that look up unknown-OID parameters in
// pg_type: if we say $1 has OID 26 (oid type) for a WHERE oid = $1 clause, quaint
// knows the oid type natively and skips the pg_type lookup entirely.
func inferParamOIDs(sql string, n int) []uint32 {
	if n == 0 {
		return nil
	}
	oids := make([]uint32, n)
	// Heuristic: look for patterns "column_name [op] $N" where we know the type.
	upper := strings.ToUpper(sql)
	for i := 0; i < n; i++ {
		param := fmt.Sprintf("$%d", i+1)
		idx := strings.Index(sql, param)
		if idx < 0 {
			continue
		}
		// Look backwards from $N for a known column name/keyword.
		before := strings.ToUpper(strings.TrimSpace(sql[:idx]))
		switch {
		case strings.HasSuffix(before, ".OID =") ||
			strings.HasSuffix(before, ".OID=") ||
			strings.HasSuffix(before, "OID =") ||
			strings.HasSuffix(before, "OID=") ||
			strings.HasSuffix(before, "TYPELEM =") ||
			strings.HasSuffix(before, "TYPBASETYPE =") ||
			strings.HasSuffix(before, "TYPRELID ="):
			oids[i] = executor.OIDOid // 26
		case strings.HasSuffix(before, "NSPNAME =") ||
			strings.HasSuffix(before, "RELNAME =") ||
			strings.HasSuffix(before, "TYPNAME =") ||
			strings.HasSuffix(before, "ATTNAME ="):
			oids[i] = executor.OIDText // 25 (name→text is safe)
		case strings.Contains(before[max(0, len(before)-20):], "= ANY"):
			// ANY($N) array parameter — use text[] (OID 1009) so quaint knows to
			// encode the array in PostgreSQL array format.
			oids[i] = executor.OIDTextArr // 1009 = text[]
		default:
			_ = upper   // suppress "upper declared and not used" if no upper uses added
			oids[i] = 0 // unknown — let driver infer
		}
	}
	return oids
}

// noopSentinel is returned by normalizeSQLForParsing for statements that
// BasuyuDB intentionally accepts without executing (CREATE FUNCTION, etc.).
// unquoteGUCName strips surrounding double quotes from a GUC name.
func unquoteGUCName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) >= 2 && name[0] == '"' && name[len(name)-1] == '"' {
		return name[1 : len(name)-1]
	}
	return name
}

// applySetGUC parses a SET statement and persists the resulting run-time
// configuration parameter into the session settings, so a later
// current_setting() / SHOW reads it back. It handles:
//
//	SET name = value          SET name TO value
//	SET LOCAL name = value    SET SESSION name = value
//
// Multi-word / control forms (SET ROLE, SET TRANSACTION, SET SESSION
// CHARACTERISTICS, SET CONSTRAINTS, SET TIME ZONE, SET SCHEMA) are left to their
// own handlers and ignored here. Per PostgreSQL, GUC names are case-insensitive.
func applySetGUC(sess *session.Session, sql string) {
	rest := strings.TrimSpace(sql[len("SET"):])
	// Strip the LOCAL / SESSION scope keyword (we model both as session-scoped:
	// SET LOCAL would be transaction-scoped in PG, but the single-statement
	// autocommit model makes session scope a safe superset for current_setting).
	upRest := strings.ToUpper(rest)
	switch {
	case strings.HasPrefix(upRest, "LOCAL "):
		rest = strings.TrimSpace(rest[len("LOCAL "):])
	case strings.HasPrefix(upRest, "SESSION "):
		rest = strings.TrimSpace(rest[len("SESSION "):])
	}
	// Reject the multi-word control forms that are not simple GUC assignments.
	upRest = strings.ToUpper(rest)
	for _, pfx := range []string{"ROLE", "TRANSACTION", "CONSTRAINTS", "TIME ZONE", "SCHEMA", "NAMES", "CHARACTERISTICS"} {
		if upRest == pfx || strings.HasPrefix(upRest, pfx+" ") {
			return
		}
	}
	// Split on the first '=' or ' TO '.
	rest = strings.TrimRight(rest, "; ")
	var name, val string
	if i := strings.Index(rest, "="); i >= 0 {
		name = strings.TrimSpace(rest[:i])
		val = strings.TrimSpace(rest[i+1:])
	} else if i := strings.Index(strings.ToUpper(rest), " TO "); i >= 0 {
		name = strings.TrimSpace(rest[:i])
		val = strings.TrimSpace(rest[i+4:])
	} else {
		return
	}
	if name == "" {
		return
	}
	sess.SetSetting(unquoteGUCName(name), unquoteGUCValue(val))
}

// unquoteGUCValue strips one layer of single quotes (PG SET value literal) and
// unescapes doubled quotes, or returns a bareword/numeric value unchanged.
func unquoteGUCValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

const noopSentinel = "__BASUYUDB_NOOP__"

// normalizeSQLForParsing applies lightweight SQL rewrites so that common SQL
// patterns our goyacc grammar doesn't yet handle can still be parsed.
func normalizeSQLForParsing(sql string) string {
	// Statements that are too complex to parse in goyacc or that we intentionally
	// accept but do not execute.  Replace with the sentinel before any other work.
	upperTrimmed := strings.TrimSpace(strings.ToUpper(sql))
	if strings.HasPrefix(upperTrimmed, "CREATE FUNCTION ") ||
		strings.HasPrefix(upperTrimmed, "CREATE OR REPLACE FUNCTION ") ||
		strings.HasPrefix(upperTrimmed, "CREATE TRIGGER ") ||
		strings.HasPrefix(upperTrimmed, "CREATE OR REPLACE TRIGGER ") ||
		strings.HasPrefix(upperTrimmed, "CREATE CONSTRAINT TRIGGER ") ||
		strings.HasPrefix(upperTrimmed, "DROP FUNCTION ") ||
		strings.HasPrefix(upperTrimmed, "DROP TRIGGER ") ||
		strings.HasPrefix(upperTrimmed, "ALTER FUNCTION ") ||
		strings.HasPrefix(upperTrimmed, "COMMENT ON ") ||
		strings.HasPrefix(upperTrimmed, "CREATE AGGREGATE ") ||
		strings.HasPrefix(upperTrimmed, "CREATE OPERATOR ") ||
		strings.HasPrefix(upperTrimmed, "CREATE RULE ") ||
		strings.HasPrefix(upperTrimmed, "ALTER SEQUENCE ") ||
		strings.HasPrefix(upperTrimmed, "CREATE DOMAIN ") ||
		strings.HasPrefix(upperTrimmed, "CREATE COLLATION ") ||
		strings.HasPrefix(upperTrimmed, "GRANT ") ||
		strings.HasPrefix(upperTrimmed, "REVOKE ") ||
		strings.HasPrefix(upperTrimmed, "CREATE DATABASE ") ||
		strings.HasPrefix(upperTrimmed, "DROP DATABASE ") ||
		strings.HasPrefix(upperTrimmed, "CREATE USER ") ||
		strings.HasPrefix(upperTrimmed, "DROP USER ") ||
		strings.HasPrefix(upperTrimmed, "ALTER USER ") ||
		strings.HasPrefix(upperTrimmed, "CREATE ROLE ") ||
		strings.HasPrefix(upperTrimmed, "DROP ROLE ") ||
		strings.HasPrefix(upperTrimmed, "ALTER ROLE ") ||
		strings.HasPrefix(upperTrimmed, "CREATE GROUP ") ||
		strings.HasPrefix(upperTrimmed, "DROP GROUP ") ||
		strings.HasPrefix(upperTrimmed, "CREATE PUBLICATION ") ||
		strings.HasPrefix(upperTrimmed, "DROP PUBLICATION ") ||
		strings.HasPrefix(upperTrimmed, "CREATE SUBSCRIPTION ") ||
		strings.HasPrefix(upperTrimmed, "DROP SUBSCRIPTION ") ||
		strings.HasPrefix(upperTrimmed, "ALTER PUBLICATION ") ||
		strings.HasPrefix(upperTrimmed, "ALTER SUBSCRIPTION ") ||
		strings.HasPrefix(upperTrimmed, "CREATE SERVER ") ||
		strings.HasPrefix(upperTrimmed, "DROP SERVER ") ||
		strings.HasPrefix(upperTrimmed, "CREATE FOREIGN ") ||
		strings.HasPrefix(upperTrimmed, "DROP FOREIGN ") ||
		strings.HasPrefix(upperTrimmed, "CREATE TABLESPACE ") ||
		strings.HasPrefix(upperTrimmed, "DROP TABLESPACE ") ||
		strings.HasPrefix(upperTrimmed, "CREATE EVENT TRIGGER ") ||
		strings.HasPrefix(upperTrimmed, "DROP EVENT TRIGGER ") ||
		strings.HasPrefix(upperTrimmed, "ALTER EVENT TRIGGER ") ||
		strings.HasPrefix(upperTrimmed, "CREATE CAST ") ||
		strings.HasPrefix(upperTrimmed, "DROP CAST ") ||
		strings.HasPrefix(upperTrimmed, "REASSIGN OWNED ") ||
		strings.HasPrefix(upperTrimmed, "SECURITY LABEL ") ||
		strings.HasPrefix(upperTrimmed, "IMPORT FOREIGN SCHEMA") ||
		strings.HasPrefix(upperTrimmed, "ALTER SYSTEM ") ||
		upperTrimmed == "CHECKPOINT" ||
		strings.HasPrefix(upperTrimmed, "CHECKPOINT ") ||
		strings.HasPrefix(upperTrimmed, "PREPARE TRANSACTION ") ||
		strings.HasPrefix(upperTrimmed, "COMMIT PREPARED ") ||
		strings.HasPrefix(upperTrimmed, "ROLLBACK PREPARED ") ||
		strings.HasPrefix(upperTrimmed, "MERGE ") ||
		strings.HasPrefix(upperTrimmed, "SELECT INTO ") ||
		upperTrimmed == "CLUSTER" ||
		strings.HasPrefix(upperTrimmed, "CLUSTER ") ||
		strings.HasPrefix(upperTrimmed, "CREATE TEXT SEARCH ") ||
		strings.HasPrefix(upperTrimmed, "DROP TEXT SEARCH ") ||
		strings.HasPrefix(upperTrimmed, "ALTER TEXT SEARCH ") {
		return noopSentinel
	}
	sql = strings.NewReplacer(
		"LEFT OUTER JOIN", "LEFT JOIN",
		"left outer join", "left join",
		"RIGHT OUTER JOIN", "RIGHT JOIN",
		"right outer join", "right join",
		"FULL OUTER JOIN", "FULL JOIN",
		"full outer join", "full join",
	).Replace(sql)

	// Strip schema-qualifiers from INSERT/UPDATE/DELETE table references and from
	// RETURNING column references. Prisma client emits "public"."User" for table
	// names and "public"."User"."id" for RETURNING columns — our parser only handles
	// the unqualified form. We strip the qualifier BEFORE unquoting so the double-
	// quote removal works cleanly.
	sql = stripSchemaQualifiers(sql)

	// Unquote double-quoted identifiers in DDL (Prisma uses "TableName", "colName").
	// Our lexer doesn't recognise "..." as an identifier token, so we strip quotes.
	// This is safe for identifiers that don't contain spaces or special chars.
	sql = unquoteIdentifiers(sql)

	// NOTE: table-level PRIMARY KEY / UNIQUE / FOREIGN KEY / CHECK constraints are
	// now parsed natively by the CREATE TABLE grammar (wave 8), including composite
	// (multi-column) keys. The legacy rewriteTableConstraints() shim — which lifted
	// them into column-level qualifiers — corrupted composite keys (it produced two
	// inline PRIMARY KEY columns → 42P16), so it is intentionally no longer called.
	// See ADR-023 addendum (wave 8).

	// Rewrite AT TIME ZONE into the timezone() function that eval.go understands.
	// e.g.  now() AT TIME ZONE 'UTC'  →  timezone('UTC', now())
	// We use a simple replace since AT TIME ZONE always has the form: expr AT TIME ZONE literal.
	// This is done case-insensitively.
	if upper := strings.ToUpper(sql); strings.Contains(upper, " AT TIME ZONE ") {
		sql = rewriteAtTimeZone(sql)
	}

	// Rewrite SIMILAR TO / NOT SIMILAR TO to LIKE / NOT LIKE for parser compat.
	// SIMILAR TO has slightly different regex syntax than POSIX but LIKE is close
	// enough for the common compatibility use-cases.
	if strings.Contains(strings.ToUpper(sql), " SIMILAR TO ") {
		sql = strings.NewReplacer(
			" SIMILAR TO ", " LIKE ",
			" similar to ", " LIKE ",
		).Replace(sql)
	}
	if strings.Contains(strings.ToUpper(sql), " NOT SIMILAR TO ") {
		sql = strings.NewReplacer(
			" NOT SIMILAR TO ", " NOT LIKE ",
			" not similar to ", " NOT LIKE ",
		).Replace(sql)
	}

	// Normalize isolation levels BasuyuDB doesn't support to READ COMMITTED.
	// Only rewrite when the string appears in an isolation-level context to
	// avoid clobbering column names or string literals.
	{
		upper2 := strings.ToUpper(sql)
		if (strings.Contains(upper2, "SERIALIZABLE") || strings.Contains(upper2, "REPEATABLE READ")) &&
			(strings.Contains(upper2, "ISOLATION LEVEL") ||
				strings.Contains(upper2, "TRANSACTION_ISOLATION") ||
				strings.Contains(upper2, "DEFAULT_TRANSACTION_ISOLATION")) {
			if strings.Contains(upper2, "SERIALIZABLE") {
				sql = strings.ReplaceAll(sql, "SERIALIZABLE", "READ COMMITTED")
				sql = strings.ReplaceAll(sql, "serializable", "read committed")
			}
			if strings.Contains(strings.ToUpper(sql), "REPEATABLE READ") {
				sql = strings.ReplaceAll(sql, "REPEATABLE READ", "READ COMMITTED")
				sql = strings.ReplaceAll(sql, "repeatable read", "read committed")
			}
		}
	}

	// Strip pg_catalog. prefix from function/type calls — all pg_catalog
	// built-ins are available unqualified in BasuyuDB.
	if strings.Contains(sql, "pg_catalog.") {
		sql = strings.ReplaceAll(sql, "pg_catalog.", "")
	}

	// Strip row-level locking clauses appended to SELECT statements.
	// The parser doesn't support FOR UPDATE / FOR SHARE / etc.; strip them to
	// avoid parse errors. We only strip when the clause appears at or near the
	// very end of the SQL (within 50 chars), so subquery FORs are left alone.
	upperSQL := strings.ToUpper(sql)
	for _, suffix := range []string{
		" FOR UPDATE SKIP LOCKED",
		" FOR UPDATE NOWAIT",
		" FOR UPDATE",
		" FOR NO KEY UPDATE SKIP LOCKED",
		" FOR NO KEY UPDATE NOWAIT",
		" FOR NO KEY UPDATE",
		" FOR SHARE SKIP LOCKED",
		" FOR SHARE NOWAIT",
		" FOR SHARE",
		" FOR KEY SHARE SKIP LOCKED",
		" FOR KEY SHARE NOWAIT",
		" FOR KEY SHARE",
	} {
		if idx := strings.LastIndex(upperSQL, suffix); idx >= 0 {
			// Only strip when the clause is near the end (≤50 trailing chars).
			if len(upperSQL)-idx-len(suffix) <= 50 {
				sql = strings.TrimSpace(sql[:idx])
				upperSQL = strings.ToUpper(sql)
				break
			}
		}
	}

	return sql
}

// rewriteAtTimeZone rewrites `expr AT TIME ZONE 'tz'` to `timezone('tz', expr)`.
// The rewrite is safe because AT TIME ZONE is not valid SQL in any other context.
func rewriteAtTimeZone(sql string) string {
	upper := strings.ToUpper(sql)
	const marker = " AT TIME ZONE "
	for {
		idx := strings.Index(strings.ToUpper(sql), marker)
		if idx < 0 {
			break
		}
		_ = upper
		// Find the timezone argument that follows (a quoted string or identifier).
		rest := sql[idx+len(marker):]
		var tz, after string
		rest = strings.TrimSpace(rest)
		if len(rest) > 0 && rest[0] == '\'' {
			end := strings.Index(rest[1:], "'")
			if end < 0 {
				break
			}
			tz = rest[1 : end+1]
			after = rest[end+2:]
		} else {
			// Unquoted identifier (e.g. UTC)
			end := strings.IndexAny(rest, " ,);\t\n")
			if end < 0 {
				tz = rest
				after = ""
			} else {
				tz = rest[:end]
				after = rest[end:]
			}
		}
		expr := strings.TrimSpace(sql[:idx])
		sql = "timezone('" + tz + "', " + expr + ")" + after
	}
	return sql
}

// stripSchemaQualifiers removes schema and table prefixes from quoted identifiers
// to simplify what the parser needs to handle. Specifically:
//   "schema"."table"           → "table"   (in INSERT/UPDATE/DELETE table refs)
//   "schema"."table"."column"  → "column"  (in SELECT/RETURNING column refs)
// We also handle mixed-case and unquoted schema prefixes like public.User.
func stripSchemaQualifiers(sql string) string {
	// Use a simple regex-free approach: scan for patterns "x"."y"."z" and "x"."y"
	// where x looks like a schema/table qualifier.
	// Strategy: collapse runs of "ident"."ident"."ident" to just "ident" (last part).
	var out strings.Builder
	i := 0
	for i < len(sql) {
		// Skip single-quoted strings unchanged.
		if sql[i] == '\'' {
			out.WriteByte(sql[i])
			i++
			for i < len(sql) && sql[i] != '\'' {
				out.WriteByte(sql[i])
				i++
			}
			if i < len(sql) {
				out.WriteByte(sql[i])
				i++
			}
			continue
		}
		// Look for a double-quoted identifier.
		if sql[i] == '"' {
			// Read the quoted identifier.
			j := i + 1
			for j < len(sql) && sql[j] != '"' {
				j++
			}
			firstIdent := sql[i : j+1] // including quotes
			if j+1 < len(sql) && sql[j+1] == '.' {
				// There's a dot after — could be schema.table or table.column.
				// Peek ahead: is there another quoted or plain identifier?
				k := j + 2 // after the dot
				if k < len(sql) && sql[k] == '"' {
					// "ident"."ident" pattern.
					m := k + 1
					for m < len(sql) && sql[m] != '"' {
						m++
					}
					secondIdent := sql[k : m+1]
					if m+1 < len(sql) && sql[m+1] == '.' && m+2 < len(sql) && sql[m+2] == '"' {
						// Three-part: "schema"."table"."col" → "col".
						n := m + 3
						for n < len(sql) && sql[n] != '"' {
							n++
						}
						out.WriteString(sql[m+2 : n+1])
						i = n + 1
					} else {
						// Two-part: "schema"."table" → "table".
						_ = firstIdent
						out.WriteString(secondIdent)
						i = m + 1
					}
					continue
				}
			}
			// Regular quoted identifier — keep as-is.
			out.WriteString(firstIdent)
			i = j + 1
			continue
		}
		out.WriteByte(sql[i])
		i++
	}
	return out.String()
}

// unquoteIdentifiers removes PostgreSQL-style double-quote delimiters from
// SQL identifiers. "TableName" → TableName, "col name" stays quoted only if
// it contains spaces (rare in ORMs — we leave those as-is).
func unquoteIdentifiers(sql string) string {
	var out strings.Builder
	i := 0
	for i < len(sql) {
		if sql[i] == '"' {
			// Find closing quote.
			j := i + 1
			for j < len(sql) && sql[j] != '"' {
				j++
			}
			inner := sql[i+1 : j]
			// Only strip if identifier is a plain name (no spaces, no special chars).
			if !strings.ContainsAny(inner, " \t\n") {
				out.WriteString(inner)
			} else {
				out.WriteByte('"')
				out.WriteString(inner)
				out.WriteByte('"')
			}
			i = j + 1
			continue
		}
		// Keep single-quoted string literals unchanged.
		if sql[i] == '\'' {
			out.WriteByte(sql[i])
			i++
			for i < len(sql) && sql[i] != '\'' {
				out.WriteByte(sql[i])
				i++
			}
			if i < len(sql) {
				out.WriteByte(sql[i])
				i++
			}
			continue
		}
		out.WriteByte(sql[i])
		i++
	}
	return out.String()
}

func isCreateTable(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	return strings.HasPrefix(upper, "CREATE TABLE")
}

// rewriteTableConstraints rewrites table-level PRIMARY KEY / UNIQUE constraint
// declarations at the end of a CREATE TABLE column list into column-level
// qualifiers. This lets our grammar parse Prisma-generated CREATE TABLE.
//
// Handles:
//   CONSTRAINT name PRIMARY KEY (col1, col2, ...)  → col1 gets PRIMARY KEY qualifier
//   CONSTRAINT name UNIQUE (col1, col2, ...)       → col1 gets UNIQUE qualifier
//   PRIMARY KEY (col1, ...)                         → same, no name
//   UNIQUE (col1, ...)                              → same
func rewriteTableConstraints(sql string) string {
	// Find the outermost CREATE TABLE ( ... ) block.
	start := strings.Index(sql, "(")
	end := strings.LastIndex(sql, ")")
	if start < 0 || end < 0 || end <= start {
		return sql
	}
	colPart := sql[start+1 : end]
	colLines := splitColDefs(colPart)

	var realCols []string
	pkCols := []string{}
	uniqueCols := []string{}

	for _, line := range colLines {
		trimLine := strings.TrimSpace(line)
		upper := strings.ToUpper(trimLine)
		if strings.HasPrefix(upper, "CONSTRAINT ") || strings.HasPrefix(upper, "PRIMARY KEY") || strings.HasPrefix(upper, "UNIQUE ") {
			// Extract PK / UNIQUE col list.
			pkIdx := strings.Index(upper, "PRIMARY KEY")
			uqIdx := strings.Index(upper, "UNIQUE")
			if pkIdx >= 0 {
				pkCols = extractParenList(trimLine[pkIdx+len("PRIMARY KEY"):])
			} else if uqIdx >= 0 {
				uniqueCols = extractParenList(trimLine[uqIdx+len("UNIQUE"):])
			}
			// Don't add this as a column def.
		} else {
			realCols = append(realCols, line)
		}
	}

	// Add PRIMARY KEY qualifier to matching column definitions.
	for i, colLine := range realCols {
		name := extractFirstIdent(strings.TrimSpace(colLine))
		for _, pk := range pkCols {
			if strings.EqualFold(name, pk) {
				realCols[i] = strings.TrimRight(colLine, " \t") + " PRIMARY KEY"
				break
			}
		}
		for _, uq := range uniqueCols {
			if strings.EqualFold(name, uq) {
				upper := strings.ToUpper(realCols[i])
				if !strings.Contains(upper, "PRIMARY KEY") {
					realCols[i] = strings.TrimRight(realCols[i], " \t") + " UNIQUE"
				}
				break
			}
		}
	}

	return sql[:start+1] + "\n" + strings.Join(realCols, ",\n") + "\n" + sql[end:]
}

// splitColDefs splits a CREATE TABLE column list by commas, respecting nested
// parentheses (e.g. DEFAULT(...)).
func splitColDefs(s string) []string {
	var parts []string
	depth := 0
	var cur strings.Builder
	for _, c := range s {
		switch c {
		case '(':
			depth++
			cur.WriteRune(c)
		case ')':
			depth--
			cur.WriteRune(c)
		case ',':
			if depth == 0 {
				parts = append(parts, cur.String())
				cur.Reset()
			} else {
				cur.WriteRune(c)
			}
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// extractParenList returns the comma-separated names inside the first (...)
// after s.
func extractParenList(s string) []string {
	start := strings.Index(s, "(")
	end := strings.Index(s, ")")
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	inner := s[start+1 : end]
	var names []string
	for _, part := range strings.Split(inner, ",") {
		names = append(names, strings.TrimSpace(part))
	}
	return names
}

// extractFirstIdent returns the first whitespace-delimited token (identifier)
// from a column definition line.
func extractFirstIdent(s string) string {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
		i++
	}
	return s[:i]
}

// decodeBinaryTextArray decodes a PostgreSQL binary-format array into the
// text-literal format {elem1,elem2,...}. Binary array format (from PostgreSQL docs):
//   int32  ndim           number of dimensions (1 for a simple 1-D array)
//   int32  flags          0 = no nulls present, 1 = nulls present
//   uint32 element_type   OID of element type (e.g. 25 = text)
//   For each dimension:
//     int32 dim_length    number of elements in this dimension
//     int32 lower_bound   lower bound index (usually 1)
//   For each element (row-major order):
//     int32 value_length  byte length of value (-1 = NULL)
//     bytes value_data    the element value
func decodeBinaryTextArray(raw []byte) string {
	if len(raw) < 12 {
		return "{}"
	}
	pos := 0
	ndim := int(int32(binary.BigEndian.Uint32(raw[pos:])))
	pos += 4
	pos += 4 // flags — skip
	pos += 4 // element OID — skip
	if ndim == 0 {
		return "{}"
	}
	// Read per-dimension metadata.
	totalElems := 1
	for d := 0; d < ndim; d++ {
		if pos+8 > len(raw) {
			return "{}"
		}
		dimLen := int(int32(binary.BigEndian.Uint32(raw[pos:])))
		pos += 4
		pos += 4 // lower bound — ignored
		totalElems *= dimLen
	}
	// Read each element.
	var elems []string
	for i := 0; i < totalElems; i++ {
		if pos+4 > len(raw) {
			break
		}
		elemLen := int(int32(binary.BigEndian.Uint32(raw[pos:])))
		pos += 4
		if elemLen < 0 {
			elems = append(elems, "NULL")
			continue
		}
		if pos+elemLen > len(raw) {
			break
		}
		elems = append(elems, string(raw[pos:pos+elemLen]))
		pos += elemLen
	}
	return "{" + strings.Join(elems, ",") + "}"
}

// simplifyForDescribe strips the WHERE / ORDER BY / LIMIT from a SELECT SQL so
// that an otherwise-unparseable query (e.g. using `= ANY($1)`) at least gives
// us the column types needed for RowDescription. Only the SELECT list and FROM
// clause are kept; the predicate/ordering are dropped.
func simplifyForDescribe(sql string) string {
	upper := strings.ToUpper(sql)
	// Only attempt for SELECT statements.
	if !strings.HasPrefix(strings.TrimSpace(upper), "SELECT") {
		return sql
	}
	// Find the position of WHERE in the top-level query (skip nested parens).
	depth := 0
	for i := 0; i < len(upper); i++ {
		switch upper[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+5 <= len(upper) && upper[i:i+5] == "WHERE" {
			return strings.TrimRight(sql[:i], " \t\r\n")
		}
		if depth == 0 && i+8 <= len(upper) && upper[i:i+8] == "ORDER BY" {
			return strings.TrimRight(sql[:i], " \t\r\n")
		}
	}
	return sql
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
		e.int16(0)  // format code: text
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
	case executor.OIDInt2:
		n, _ := strconv.ParseInt(text, 10, 64)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(int16(n)))
		return b[:]
	case executor.OIDOid:
		// oid is a 4-byte unsigned integer (same encoding as int4 but unsigned).
		n, _ := strconv.ParseUint(text, 10, 32)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n))
		return b[:]
	case executor.OIDFloat4:
		f, _ := strconv.ParseFloat(text, 32)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], math.Float32bits(float32(f)))
		return b[:]
	case executor.OIDChar:
		// "char" is a single byte — send the first byte of the text representation.
		if len(text) > 0 {
			return []byte{text[0]}
		}
		return []byte{0}
	case executor.OIDBool:
		if text == "t" || text == "true" || text == "1" {
			return []byte{1}
		}
		return []byte{0}
	case executor.OIDUUID:
		if u, ok := encodeUUID(text); ok {
			return u
		}
		return []byte(text)
	case executor.OIDDate:
		if d, ok := encodeDate(text); ok {
			return d
		}
		return []byte(text)
	case executor.OIDTimestamp, executor.OIDTimestamptz:
		if ts, ok := encodeTimestamp(text); ok {
			return ts
		}
		return []byte(text)
	default:
		// text/varchar/char/json/jsonb/numeric/bytea: binary == UTF-8 bytes.
		return []byte(text)
	}
}

// pgEpoch is PostgreSQL's date/timestamp origin: 2000-01-01 00:00:00 UTC.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var tsLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02",
}

func parseTime(text string) (time.Time, bool) {
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, strings.TrimSpace(text)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// encodeTimestamp produces the PG binary timestamp: int64 microseconds since
// the PG epoch.
func encodeTimestamp(text string) ([]byte, bool) {
	t, ok := parseTime(text)
	if !ok {
		return nil, false
	}
	micros := t.UTC().Sub(pgEpoch).Microseconds()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(micros))
	return b[:], true
}

// encodeDate produces the PG binary date: int32 days since the PG epoch.
func encodeDate(text string) ([]byte, bool) {
	t, ok := parseTime(text)
	if !ok {
		return nil, false
	}
	days := int32(t.UTC().Sub(pgEpoch).Hours() / 24)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(days))
	return b[:], true
}

// encodeUUID produces the 16-byte PG binary UUID from its text form.
func encodeUUID(text string) ([]byte, bool) {
	hexStr := strings.ReplaceAll(strings.TrimSpace(text), "-", "")
	if len(hexStr) != 32 {
		return nil, false
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, false
	}
	return b, true
}

func typeLen(oid uint32) int16 {
	switch oid {
	case executor.OIDBool, executor.OIDChar:
		return 1
	case executor.OIDInt2:
		return 2
	case executor.OIDInt4, executor.OIDFloat4, executor.OIDOid:
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

// sendCommandComplete sends a CommandComplete followed by ReadyForQuery and
// flushes the write buffer. It is used by control-statement stubs that need a
// clean round-trip without going through execSQL.
func (c *conn) sendCommandComplete(tag string) error {
	if err := c.mw.send(msgCommandComplete, commandComplete(tag)); err != nil {
		return err
	}
	if err := c.mw.send(msgReadyForQuery, readyForQuery(c.txStatus())); err != nil {
		return err
	}
	return c.mw.flush()
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
