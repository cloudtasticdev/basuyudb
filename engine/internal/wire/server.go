package wire

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
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
	// remaining: int16 numParamTypes + OIDs — ignored in milestone-1
	c.boundParams = nil
	c.mw.send(msgParseComplete, nil)
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
	// (trailing result format codes ignored — text format only in milestone-1)
	c.mw.send(msgBindComplete, nil)
}

func (c *conn) handleDescribe(body []byte) {
	pos := 0
	kind := body[pos]
	pos++
	_ = cString(body, &pos) // name

	if kind == 'S' {
		// Statement describe: milestone-1 returns NoData (clients re-Describe the
		// portal after Bind for row shape).
		c.mw.send(msgNoData, nil)
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
	// emit only DataRows + CommandComplete.
	for _, row := range res.Rows {
		c.mw.send(msgDataRow, dataRow(row))
	}
	c.mw.send(msgCommandComplete, commandComplete(commandTag(res)))
}
