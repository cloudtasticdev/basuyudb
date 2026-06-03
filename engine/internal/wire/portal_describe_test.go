package wire

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"testing"
)

// rawConn is a minimal client-side PG wire helper for exercising the exact
// message sequence node-postgres uses: it describes the PORTAL (not the
// statement) after Bind. That path must NOT execute the statement, or a
// mutating INSERT ... RETURNING runs twice (once at Describe, once at Execute).
type rawConn struct {
	c net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *rawConn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	rc := &rawConn{c: c, r: bufio.NewReader(c)}
	// StartupMessage: int32 len, int32 proto(196608), "user\0postgres\0database\0defaultdb\0\0".
	params := []byte("user\x00postgres\x00database\x00defaultdb\x00\x00")
	body := make([]byte, 4+len(params))
	binary.BigEndian.PutUint32(body[0:4], 196608)
	copy(body[4:], params)
	pkt := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(pkt[0:4], uint32(len(body)+4))
	copy(pkt[4:], body)
	if _, err := c.Write(pkt); err != nil {
		t.Fatalf("write startup: %v", err)
	}
	rc.readUntilReady(t) // consume Auth/ParamStatus/BackendKeyData/ReadyForQuery
	return rc
}

func (rc *rawConn) send(t *testing.T, typ byte, body []byte) {
	t.Helper()
	h := make([]byte, 5)
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:], uint32(len(body)+4))
	if _, err := rc.c.Write(append(h, body...)); err != nil {
		t.Fatalf("write %c: %v", typ, err)
	}
}

// readUntilReady consumes messages until ReadyForQuery ('Z'), returning the set
// of message-type bytes seen.
func (rc *rawConn) readUntilReady(t *testing.T) map[byte]int {
	t.Helper()
	seen := map[byte]int{}
	for {
		typ, err := rc.r.ReadByte()
		if err != nil {
			t.Fatalf("read type: %v", err)
		}
		var ln [4]byte
		if _, err := readFull(rc.r, ln[:]); err != nil {
			t.Fatalf("read len: %v", err)
		}
		n := binary.BigEndian.Uint32(ln[:]) - 4
		buf := make([]byte, n)
		if _, err := readFull(rc.r, buf); err != nil {
			t.Fatalf("read body: %v", err)
		}
		seen[typ]++
		if typ == 'E' { // ErrorResponse — surface for debugging
			t.Fatalf("server error response: %q", string(buf))
		}
		if typ == 'Z' { // ReadyForQuery
			return seen
		}
	}
}

func readFull(r *bufio.Reader, p []byte) (int, error) {
	got := 0
	for got < len(p) {
		n, err := r.Read(p[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

func cstr(s string) []byte { return append([]byte(s), 0) }

// TestPortalDescribeDoesNotDoubleExecute guards the node-postgres pattern:
// Parse / Bind / Describe('P') / Execute on an INSERT ... RETURNING must insert
// exactly one row (the portal Describe must be side-effect-free).
func TestPortalDescribeDoesNotDoubleExecute(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()

	// Create the table with pgx (simple, convenient).
	setup := pgxConnect(t, addr, true)
	if _, err := setup.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	setup.Close(ctx)

	rc := dialRaw(t, addr)

	// Parse: unnamed stmt, "INSERT ... RETURNING id", 0 declared param types.
	var parse []byte
	parse = append(parse, cstr("")...)                                          // stmt name
	parse = append(parse, cstr("INSERT INTO t (id, v) VALUES ($1, $2) RETURNING id")...) // query
	parse = append(parse, 0, 0)                                                 // int16 param count = 0
	rc.send(t, 'P', parse)

	// Bind: unnamed portal/stmt, 0 param format codes, 2 text params ("5","9"),
	// 0 result format codes.
	var bind []byte
	bind = append(bind, cstr("")...) // portal
	bind = append(bind, cstr("")...) // stmt
	bind = append(bind, 0, 0)        // int16 param format code count = 0
	bind = append(bind, 0, 2)        // int16 param value count = 2
	for _, v := range []string{"5", "9"} {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(v)))
		bind = append(bind, l[:]...)
		bind = append(bind, []byte(v)...)
	}
	bind = append(bind, 0, 0) // int16 result format code count = 0
	rc.send(t, 'B', bind)

	// Describe the PORTAL ('P'), then Execute, then Sync.
	rc.send(t, 'D', append([]byte{'P'}, cstr("")...))
	exec := append(cstr(""), 0, 0, 0, 0) // portal + int32 max rows = 0
	rc.send(t, 'E', exec)
	rc.send(t, 'S', nil)
	rc.readUntilReady(t)

	// Exactly one row must exist — Describe must not have inserted a phantom row.
	check := pgxConnect(t, addr, true)
	defer check.Close(ctx)
	var n int
	if err := check.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("portal Describe double-executed: want 1 row, got %d", n)
	}
}
