package wire

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
)

// Server is the PG wire v3 listener.
type Server struct {
	addr string
	exec executor.Executor
	jwks *auth.JWKSCache // nil in dev mode
	devMode bool
	logger *slog.Logger

	ln net.Listener
}

// Config configures the wire Server.
type Config struct {
	Addr string // e.g. ":5432"
	Executor executor.Executor
	JWKS *auth.JWKSCache // required unless DevMode
	DevMode bool // BASUYUDB_DEV_MODE
	Logger *slog.Logger
}

// NewServer constructs a wire Server.
func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{
		addr: cfg.Addr,
		exec: cfg.Executor,
		jwks: cfg.JWKS,
		devMode: cfg.DevMode,
		logger: cfg.Logger,
	}
}

// Listen binds the TCP listener (so the bound address is known before Serve).
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Addr returns the actual bound address (useful when addr was ":0").
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Serve accepts connections until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
	s.logger.Info("PG wire server listening", "addr", s.Addr(), "dev_mode", s.devMode)
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go s.handleConn(nc)
	}
}

// Close stops the listener.
func (s *Server) Close() error {
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// ---- extended query protocol (milestone-1: single unnamed stmt/portal) ----

// execStatement unifies session-control interception and real execution. It is
// used by both the simple and extended query paths.
func (c *conn) execStatement(sql string, params []executor.Datum) (*executor.Result, error) {
	trimmed := strings.TrimSpace(sql)
	if tag, ok := sessionControlTag(trimmed); ok {
		return &executor.Result{Command: tag}, nil
	}
	return c.execSQL(trimmed, params)
}

func (c *conn) handleParse(body []byte) {
	pos := 0
	_ = cString(body, &pos) // destination prepared-statement name (unnamed)
	c.parsedSQL = cString(body, &pos) // query string
	// remaining: int16 numParamTypes + OIDs. The client may declare 0 and let us
	// infer; either way the authoritative count is the highest $N in the SQL.
	declared := 0
	if pos+2 <= len(body) {
		declared = int(int16(binary.BigEndian.Uint16(body[pos:])))
	}
	c.paramCount = countParams(c.parsedSQL)
	if declared > c.paramCount {
		c.paramCount = declared
	}
	c.boundParams = nil
	c.mw.send(msgParseComplete, nil)
}

// countParams returns the highest $N parameter index in sql, ignoring $-runs
// inside single-quoted string literals.
func countParams(sql string) int {
	max := 0
	inStr := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch == '\'' {
			inStr = !inStr
			continue
		}
		if inStr || ch != '$' {
			continue
		}
		j := i + 1
		n := 0
		for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
			n = n*10 + int(sql[j]-'0')
			j++
		}
		if j > i+1 && n > max {
			max = n
		}
		i = j - 1
	}
	return max
}

func (c *conn) handleBind(body []byte) {
	pos := 0
	_ = cString(body, &pos) // portal name
	_ = cString(body, &pos) // source statement name

	// Parameter format codes.
	nFmt := int(int16(binary.BigEndian.Uint16(body[pos:])))
	pos += 2
	pos += nFmt * 2 // skip format codes (treat all as text in milestone-1)

	// Parameter values.
	nParams := int(int16(binary.BigEndian.Uint16(body[pos:])))
	pos += 2
	params := make([]executor.Datum, 0, nParams)
	for i := 0; i < nParams; i++ {
		l := int32(binary.BigEndian.Uint32(body[pos:]))
		pos += 4
		if l == -1 {
			params = append(params, executor.Datum{Null: true})
			continue
		}
		val := string(body[pos : pos+int(l)])
		pos += int(l)
		params = append(params, executor.Datum{Text: val})
	}
	c.boundParams = params

	// Result format codes: int16 count, then a code per result column (or a
	// single code applied to all). Honoured so binary-requesting drivers (pgx,
	// and ORMs built on it) get binary-encoded results.
	c.resultFormats = nil
	if pos+2 <= len(body) {
		nRes := int(int16(binary.BigEndian.Uint16(body[pos:])))
		pos += 2
		formats := make([]int16, 0, nRes)
		for i := 0; i < nRes && pos+2 <= len(body); i++ {
			formats = append(formats, int16(binary.BigEndian.Uint16(body[pos:])))
			pos += 2
		}
		c.resultFormats = formats
	}
	c.mw.send(msgBindComplete, nil)
}

func (c *conn) handleDescribe(body []byte) {
	pos := 0
	kind := body[pos]
	pos++
	_ = cString(body, &pos) // name

	if kind == 'S' {
		// Statement describe: the driver needs the parameter count before Bind, and
		// (for statement-caching drivers like pgx) the result row shape too. Send
		// ParameterDescription, then RowDescription for a row-returning statement
		// or NoData otherwise.
		c.mw.send(msgParameterDesc, parameterDescription(c.paramCount))
		if cols := c.describeColumns(); len(cols) > 0 {
			c.mw.send(msgRowDescription, rowDescription(cols))
		} else {
			c.mw.send(msgNoData, nil)
		}
		return
	}
	// Portal describe: execute to learn the row shape.
	res, err := c.execStatement(c.parsedSQL, c.boundParams)
	if err != nil {
		c.sendExecError(err)
		return
	}
	if len(res.Columns) == 0 {
		c.mw.send(msgNoData, nil)
	} else {
		c.mw.send(msgRowDescription, rowDescription(res.Columns))
	}
}

// describeColumns returns the result columns of a row-returning prepared
// statement, probed by executing it with NULL parameters (read-only — only the
// column shape is used). Non-SELECT statements return nil so the caller sends
// NoData and never mutates at describe time.
func (c *conn) describeColumns() []executor.Column {
	stmt, err := parser.Parse(c.parsedSQL)
	if err != nil {
		return nil
	}
	if _, ok := stmt.(*ast.SelectStmt); !ok {
		return nil
	}
	nullParams := make([]executor.Datum, c.paramCount)
	for i := range nullParams {
		nullParams[i] = executor.Datum{Null: true}
	}
	res, err := c.srv.exec.Execute(context.Background(), stmt, c.sess, nullParams)
	if err != nil {
		return nil
	}
	return res.Columns
}

func (c *conn) handleExecuteExtended(body []byte) {
	pos := 0
	_ = cString(body, &pos) // portal name
	// int32 max rows — ignored (return all) in milestone-1.

	res, err := c.execStatement(c.parsedSQL, c.boundParams)
	if err != nil {
		c.sendExecError(err)
		return
	}
	// In extended protocol, RowDescription was already sent at Describe; here we
	// emit only DataRows (in the client-requested format) + CommandComplete.
	for _, row := range res.Rows {
		c.mw.send(msgDataRow, dataRowFmt(row, res.Columns, c.resultFormats))
	}
	c.mw.send(msgCommandComplete, commandComplete(commandTag(res)))
}
