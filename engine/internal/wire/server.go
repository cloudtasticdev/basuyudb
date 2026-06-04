package wire

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
)

// pgNotification is an async notification delivered via NOTIFY.
type pgNotification struct {
	channel string
	payload string
	pid     int32
}

// listenerEntry associates a buffered notification channel with the conn
// that issued LISTEN.
type listenerEntry struct {
	ch   chan pgNotification
	conn *conn
}

// Server is the PG wire v3 listener.
type Server struct {
	addr      string
	exec      executor.Executor
	jwks      *auth.JWKSCache // nil in dev mode
	devMode   bool
	logger    *slog.Logger
	tlsConfig *tls.Config

	// roles holds locally provisioned SCRAM-SHA-256 credentials created via
	// CREATE/ALTER ROLE ... PASSWORD. When Config.RolesPath is set these are
	// persisted across restarts (optionally encrypted at rest); see
	// internal/auth/roles.go and rolestore_persist.go.
	roles *auth.RoleStore

	ln net.Listener

	// listenerMu guards listeners. A channel name maps to the set of conns
	// currently LISTENing on it.
	listenerMu sync.RWMutex
	listeners  map[string][]*listenerEntry
}

// Config configures the wire Server.
type Config struct {
	Addr string // e.g. ":5432"
	Executor executor.Executor
	JWKS *auth.JWKSCache // required unless DevMode
	DevMode bool // BASUYUDB_DEV_MODE
	Logger *slog.Logger

	// RolesPath, if non-empty, makes the local SCRAM role store durable: roles
	// are loaded from and flushed to this file (conventionally
	// <dataDir>/roles.json). When empty, the store is in-memory only.
	RolesPath string
	// EncryptionKey, when non-empty alongside RolesPath, encrypts the roles file
	// at rest with AES-256-GCM (same key as BadgerDB's BASUYUDB_ENCRYPTION_KEY).
	EncryptionKey []byte

	// BootstrapRole / BootstrapPassword, when BOTH non-empty, seed a single role
	// into the (loaded) store at construction if it is not already present —
	// making SCRAM usable from a clean install. The password is never logged.
	BootstrapRole     string
	BootstrapPassword string
}

// NewServer constructs a wire Server. It returns an error if a persistent role
// store is requested but cannot be loaded (e.g. a corrupt/undecryptable roles
// file), or if bootstrap role seeding fails — we fail fast rather than start
// with missing or wrong credentials.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	var roles *auth.RoleStore
	if cfg.RolesPath != "" {
		rs, err := auth.NewPersistentRoleStore(cfg.RolesPath, cfg.EncryptionKey)
		if err != nil {
			return nil, fmt.Errorf("wire: load persistent role store: %w", err)
		}
		roles = rs
	} else {
		roles = auth.NewRoleStore()
	}
	srv := &Server{
		addr:    cfg.Addr,
		exec:    cfg.Executor,
		jwks:    cfg.JWKS,
		devMode: cfg.DevMode,
		logger:  cfg.Logger,
		roles:   roles,
	}

	// Bootstrap role: seed only if both creds provided AND the role is absent.
	if cfg.BootstrapRole != "" && cfg.BootstrapPassword != "" {
		if srv.roles.Has(cfg.BootstrapRole) {
			srv.logger.Info("bootstrap role already present; not reseeding", "role", cfg.BootstrapRole)
		} else {
			if err := srv.SeedRole(cfg.BootstrapRole, cfg.BootstrapPassword); err != nil {
				return nil, fmt.Errorf("wire: seed bootstrap role: %w", err)
			}
			srv.logger.Info("seeded bootstrap role", "role", cfg.BootstrapRole)
		}
	}

	tlsCfg, err := generateSelfSignedTLS()
	if err != nil {
		srv.logger.Warn("TLS cert generation failed, SSL disabled", "err", err)
	} else {
		srv.tlsConfig = tlsCfg
	}
	return srv, nil
}

// SeedRole provisions (or replaces) a local SCRAM role from a plaintext
// password, persisting it when the store is durable. The plaintext is used only
// to derive the verifier and is never retained or logged.
func (s *Server) SeedRole(user, pass string) error {
	return s.roles.UpsertPassword(user, pass)
}

// generateSelfSignedTLS creates a self-signed TLS certificate for localhost
// so that clients that attempt SSL upgrades can proceed.
func generateSelfSignedTLS() (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"BasuyuDB"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
	cert, err := tls.X509KeyPair(certPEM, privPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
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

// addListener registers conn c as a listener on channel and returns a buffered
// channel on which notifications will be delivered.
func (s *Server) addListener(channel string, c *conn) chan pgNotification {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.listeners == nil {
		s.listeners = map[string][]*listenerEntry{}
	}
	ch := make(chan pgNotification, 16)
	s.listeners[channel] = append(s.listeners[channel], &listenerEntry{ch: ch, conn: c})
	return ch
}

// removeListener deregisters c from channel.
func (s *Server) removeListener(channel string, c *conn) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	list := s.listeners[channel]
	filtered := list[:0]
	for _, e := range list {
		if e.conn != c {
			filtered = append(filtered, e)
		}
	}
	s.listeners[channel] = filtered
}

// removeAllListeners removes c from every channel it is listening on.
func (s *Server) removeAllListeners(c *conn) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	for ch, list := range s.listeners {
		filtered := list[:0]
		for _, e := range list {
			if e.conn != c {
				filtered = append(filtered, e)
			}
		}
		s.listeners[ch] = filtered
	}
}

// removeListenerCh removes a specific notification channel from the registry.
func (s *Server) removeListenerCh(ch chan pgNotification) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	for name, list := range s.listeners {
		filtered := list[:0]
		for _, e := range list {
			if e.ch != ch {
				filtered = append(filtered, e)
			}
		}
		s.listeners[name] = filtered
	}
}

// notify delivers a notification to all listeners on channel.
func (s *Server) notify(channel, payload string, pid int32) {
	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()
	n := pgNotification{channel: channel, payload: payload, pid: pid}
	for _, e := range s.listeners[channel] {
		select {
		case e.ch <- n:
		default: // drop if buffer full
		}
	}
}

// notificationResponse encodes a PG NotificationResponse ('A') message body.
func notificationResponse(pid int32, channel, payload string) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(pid))
	out := make([]byte, 0, 4+len(channel)+1+len(payload)+1)
	out = append(out, b[:]...)
	out = append(out, []byte(channel)...)
	out = append(out, 0)
	out = append(out, []byte(payload)...)
	out = append(out, 0)
	return out
}

// ---- extended query protocol (milestone-1: single unnamed stmt/portal) ----

// execStatement unifies session-control interception and real execution. It is
// used by both the simple and extended query paths.
func (c *conn) execStatement(sql string, params []executor.Datum) (*executor.Result, error) {
	trimmed := strings.TrimSpace(sql)
	if tag, ok := sessionControlTag(trimmed); ok {
		return &executor.Result{Command: tag}, nil
	}
	res, err := c.execSQL(trimmed, params)
	if err != nil {
		// For catalog/introspection SELECT queries that use PostgreSQL-specific
		// syntax we don't support (unnest, generate_subscripts, &, etc.), return
		// an empty result instead of a hard error. Prisma schema-engine tolerates
		// "no rows" for introspection queries and will just skip those items.
		if isCatalogIntrospectionQuery(trimmed) {
			return &executor.Result{Command: "SELECT", Columns: nil, Rows: nil}, nil
		}
		return nil, err
	}
	return res, nil
}

// isCatalogIntrospectionQuery returns true when sql is a read-only catalog
// query that is safe to return empty results for when execution fails. This
// lets the schema engine proceed even for complex PG-specific introspection
// queries we don't fully support (unnest, generate_subscripts, bitwise ops, etc.).
func isCatalogIntrospectionQuery(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	// Must be a SELECT (including CTEs) — never treat DML as "safe to silence".
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return false
	}
	// Look for catalog table references that indicate this is an introspection query.
	for _, hint := range []string{
		"PG_INDEX", "PG_CLASS", "PG_NAMESPACE", "PG_ATTRIBUTE", "PG_TYPE",
		"PG_CONSTRAINT", "PG_VIEWS", "PG_ENUM", "PG_ATTRDEF", "PG_AM",
		"INFORMATION_SCHEMA",
	} {
		if strings.Contains(upper, hint) {
			return true
		}
	}
	return false
}

// preparedStmt is a parsed (named or unnamed) prepared statement.
type preparedStmt struct {
	sql string
	paramCount int
	paramOIDs []uint32 // declared parameter type OIDs (may be empty / 0)
	serverOIDs []uint32 // server-inferred OIDs (from inferParamOIDs); used for Bind decoding
}

// portal is a bound statement ready to Execute: its parameter values and the
// per-column result format codes negotiated at Bind.
type portal struct {
	stmt *preparedStmt
	params []executor.Datum
	resultFormats []int16
}

func (c *conn) handleParse(body []byte) {
	pos := 0
	name := cString(body, &pos) // destination prepared-statement name ("" = unnamed)
	sql := cString(body, &pos) // query string
	// int16 numParamTypes followed by that many int32 OIDs. A client may declare
	// 0 and let the server infer; the authoritative count is the highest $N.
	declared := 0
	if pos+2 <= len(body) {
		declared = int(int16(binary.BigEndian.Uint16(body[pos:])))
		pos += 2
	}
	oids := make([]uint32, 0, declared)
	for i := 0; i < declared && pos+4 <= len(body); i++ {
		oids = append(oids, binary.BigEndian.Uint32(body[pos:]))
		pos += 4
	}
	n := countParams(sql)
	if declared > n {
		n = declared
	}
	c.prepared[name] = &preparedStmt{
		sql:        sql,
		paramCount: n,
		paramOIDs:  oids,
		serverOIDs: inferParamOIDs(sql, n),
	}
	c.logger.Debug("handleParse", "name", name, "sql", sql, "paramCount", n)
	c.mw.send(msgParseComplete, nil)
}

// countParams returns the highest $N parameter index in sql. It correctly
// ignores $-tokens inside single-quoted string literals and SQL line comments
// (--). Dollar-quoting ($$..$$) is not yet supported but is rare in prepared
// statements sent by ORMs.
func countParams(sql string) int {
	max := 0
	inStr := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		// Skip line comments: -- to end-of-line.
		if !inStr && ch == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}
		// Skip block comments: /* ... */
		if !inStr && ch == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			i++ // consume closing /
			continue
		}
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
	portalName := cString(body, &pos)
	stmtName := cString(body, &pos)

	stmt := c.prepared[stmtName]
	if stmt == nil {
		c.sendError("ERROR", "26000", fmt.Sprintf("prepared statement %q does not exist", stmtName))
		return
	}

	// Parameter format codes: 0 → all text, 1 → one code for all, else one per
	// parameter. Drivers (pgx, JDBC) may send binary-encoded parameters.
	nFmt := int(int16(binary.BigEndian.Uint16(body[pos:])))
	pos += 2
	fmtCodes := make([]int16, nFmt)
	for i := 0; i < nFmt; i++ {
		fmtCodes[i] = int16(binary.BigEndian.Uint16(body[pos:]))
		pos += 2
	}

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
		raw := body[pos : pos+int(l)]
		pos += int(l)
		// Use client-declared OID (from Parse) if non-zero, else fall back to our
		// server-inferred OID (from inferParamOIDs). This ensures binary parameters
		// like text[] arrays are decoded correctly even when the client sends OID 0.
		var oid uint32
		if i < len(stmt.paramOIDs) && stmt.paramOIDs[i] != 0 {
			oid = stmt.paramOIDs[i]
		} else if i < len(stmt.serverOIDs) {
			oid = stmt.serverOIDs[i]
		}
		params = append(params, decodeBindParam(raw, paramIsBinary(fmtCodes, i), oid))
	}

	// Result format codes: int16 count, then a code per result column (or a
	// single code applied to all). Honoured so binary-requesting drivers get
	// binary-encoded results.
	var resultFormats []int16
	if pos+2 <= len(body) {
		nRes := int(int16(binary.BigEndian.Uint16(body[pos:])))
		pos += 2
		resultFormats = make([]int16, 0, nRes)
		for i := 0; i < nRes && pos+2 <= len(body); i++ {
			resultFormats = append(resultFormats, int16(binary.BigEndian.Uint16(body[pos:])))
			pos += 2
		}
	}
	c.portals[portalName] = &portal{stmt: stmt, params: params, resultFormats: resultFormats}
	c.mw.send(msgBindComplete, nil)
}

// paramIsBinary reports whether parameter i was sent in binary format, per the
// PG rule: 0 codes → all text; 1 code → applies to all; else per-parameter.
func paramIsBinary(fmtCodes []int16, i int) bool {
	switch len(fmtCodes) {
	case 0:
		return false
	case 1:
		return fmtCodes[0] == 1
	default:
		return i < len(fmtCodes) && fmtCodes[i] == 1
	}
}

// decodeBindParam converts a Bind parameter value to the executor's text Datum.
// Text-format params pass through; binary-format params are decoded per the
// declared type OID (big-endian, as PostgreSQL sends them).
func decodeBindParam(raw []byte, binary bool, oid uint32) executor.Datum {
	if !binary {
		return executor.Datum{Text: string(raw)}
	}
	return executor.Datum{Text: decodeBinaryParam(raw, oid)}
}

// decodeBinaryParam renders a binary-format parameter as text per its type OID
// (big-endian, PostgreSQL binary wire format). Unknown OIDs fall back to a
// width heuristic for the common integer/bool cases, else a raw string.
func decodeBinaryParam(raw []byte, oid uint32) string {
	switch oid {
	case executor.OIDBool:
		if len(raw) == 1 && raw[0] != 0 {
			return "t"
		}
		return "f"
	case executor.OIDInt2:
		if len(raw) == 2 {
			return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(raw))), 10)
		}
	case executor.OIDInt4:
		if len(raw) == 4 {
			return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(raw))), 10)
		}
	case executor.OIDInt8:
		if len(raw) == 8 {
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(raw)), 10)
		}
	case executor.OIDOid:
		// oid is stored as 4-byte unsigned integer.
		if len(raw) == 4 {
			return strconv.FormatUint(uint64(binary.BigEndian.Uint32(raw)), 10)
		}
	case executor.OIDChar:
		// "char" is a single byte — return it as the character.
		if len(raw) == 1 {
			return string(raw)
		}
	case executor.OIDTextArr:
		// Binary text[] array: decode to PostgreSQL array literal {elem1,elem2,...}
		// Format: int32 flags, int32 element_type, int32 ndim, then per-dim [length,lower],
		// then for each element: int32 len (-1=null) + bytes.
		return decodeBinaryTextArray(raw)
	case executor.OIDFloat4:
		if len(raw) == 4 {
			return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), 'g', -1, 32)
		}
	case executor.OIDFloat8:
		if len(raw) == 8 {
			return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(raw)), 'g', -1, 64)
		}
	case 0:
		// Undeclared type sent in binary: infer from width (integers/bool only).
		switch len(raw) {
		case 1:
			if raw[0] == 0 {
				return "f"
			}
			return "t"
		case 2:
			return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(raw))), 10)
		case 4:
			return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(raw))), 10)
		case 8:
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(raw)), 10)
		}
	}
	return string(raw)
}

func (c *conn) handleDescribe(body []byte) {
	pos := 0
	kind := body[pos]
	pos++
	name := cString(body, &pos)

	if kind == 'S' {
		// Statement describe: the driver needs the parameter count before Bind, and
		// (for statement-caching drivers) the result row shape too. Send
		// ParameterDescription, then RowDescription for a row-returning statement
		// or NoData otherwise.
		stmt := c.prepared[name]
		if stmt == nil {
			c.sendError("ERROR", "26000", fmt.Sprintf("prepared statement %q does not exist", name))
			return
		}
		c.mw.send(msgParameterDesc, parameterDescription(stmt.paramCount, inferParamOIDs(stmt.sql, stmt.paramCount)))
		cols := c.describeColumns(stmt.sql, stmt.paramCount)
		if len(cols) > 0 {
			colNames := make([]string, len(cols))
			colOIDs := make([]uint32, len(cols))
			for i, col := range cols {
				colNames[i] = col.Name
				colOIDs[i] = col.TypeOID
			}
			c.logger.Debug("describeStmt", "name", name, "sql", stmt.sql[:min(len(stmt.sql), 80)], "colNames", colNames, "colOIDs", colOIDs)
			c.mw.send(msgRowDescription, rowDescription(cols))
		} else {
			c.logger.Debug("describeStmt NoData", "name", name, "sql", stmt.sql[:min(len(stmt.sql), 80)])
			c.mw.send(msgNoData, nil)
		}
		return
	}
	// Portal describe: determine the row shape WITHOUT executing — executing here
	// would run mutating statements (INSERT/UPDATE/DELETE ... RETURNING) an extra
	// time before Execute. node-postgres (and other drivers) describe the portal
	// after Bind, so this path must be side-effect-free.
	p := c.portals[name]
	if p == nil {
		c.sendError("ERROR", "34000", fmt.Sprintf("portal %q does not exist", name))
		return
	}
	if cols := c.describeColumns(p.stmt.sql, p.stmt.paramCount); len(cols) > 0 {
		c.mw.send(msgRowDescription, rowDescription(cols))
	} else {
		c.mw.send(msgNoData, nil)
	}
}

// describeColumns returns the result columns of a row-returning prepared
// statement without mutating: RETURNING columns from the schema, or a SELECT's
// shape via a NULL-parameter probe. Non-row-returning statements return nil so
// the caller sends NoData.
func (c *conn) describeColumns(sql string, paramCount int) []executor.Column {
	sql = normalizeSQLForParsing(sql)
	stmt, err := parser.Parse(sql)
	if err != nil {
		// If parsing fails (e.g. unsupported syntax like `= ANY($1)`), try a
		// simplified version so at least RowDescription is correct. We strip
		// everything from WHERE onwards (the FROM clause stays, which is enough
		// to resolve column types). Only do this for apparent SELECT statements.
		simplified := simplifyForDescribe(sql)
		if simplified != sql {
			if s2, e2 := parser.Parse(simplified); e2 == nil {
				stmt, err = s2, nil
			}
		}
		if err != nil {
			return nil
		}
	}
	if cols, ok, err := c.srv.exec.DescribeReturning(context.Background(), stmt, c.sess); err == nil && ok {
		return cols
	}
	// SELECT and SHOW are read-only and return rows; their shape is learned by a
	// null-parameter probe. Everything else returns NoData.
	switch stmt.(type) {
	case *ast.SelectStmt, *ast.ShowStmt:
	default:
		return nil
	}
	nullParams := make([]executor.Datum, paramCount)
	for i := range nullParams {
		nullParams[i] = executor.Datum{Null: true}
	}
	res, err := c.srv.exec.Execute(context.Background(), stmt, c.sess, nullParams)
	if err != nil {
		// Execution failed (e.g. unsupported function like ANY, or evaluator error).
		// Fall back to schema-only column inference from the AST — this gives the
		// correct RowDescription without running the query. This is critical for
		// queries like `WHERE col = ANY($1)` where execution fails but we still
		// need to describe the SELECT list columns for quaint/Prisma type resolution.
		if sel, ok := stmt.(*ast.SelectStmt); ok {
			return c.srv.exec.InferColumns(context.Background(), c.sess, sel)
		}
		return nil
	}
	return res.Columns
}

// handleClose drops a named prepared statement ('S') or portal ('P').
func (c *conn) handleClose(body []byte) {
	if len(body) < 1 {
		return
	}
	pos := 1
	name := cString(body, &pos)
	switch body[0] {
	case 'S':
		delete(c.prepared, name)
	case 'P':
		delete(c.portals, name)
	}
}

func (c *conn) handleExecuteExtended(body []byte) {
	pos := 0
	name := cString(body, &pos) // portal name
	// int32 max rows — ignored (return all).

	p := c.portals[name]
	if p == nil {
		c.sendError("ERROR", "34000", fmt.Sprintf("portal %q does not exist", name))
		return
	}
	res, err := c.execStatement(p.stmt.sql, p.params)
	if err != nil {
		c.sendExecError(err)
		return
	}
	// In extended protocol, RowDescription was already sent at Describe; here we
	// emit only DataRows (in the client-requested format) + CommandComplete.
	for _, row := range res.Rows {
		c.mw.send(msgDataRow, dataRowFmt(row, res.Columns, p.resultFormats))
	}
	c.mw.send(msgCommandComplete, commandComplete(commandTag(res)))
}
