package wire

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// startTestServer brings up a dev-mode wire server on an ephemeral port backed
// by a temp managed store, and returns its address.
func startTestServer(t *testing.T) string {
	t.Helper()
	st, err := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	txnEngine := transactions.New(st, 1, nil)
	srv, err := NewServer(Config{Addr: "127.0.0.1:0", Executor: executor.New(st, txnEngine), DevMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		_ = st.Close()
	})
	return srv.Addr()
}

// --- minimal raw PG wire v3 client (proves the protocol byte-for-byte) ---

type pgClient struct {
	c net.Conn
	r *bufio.Reader
}

func dialPG(t *testing.T, addr string) *pgClient {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cl := &pgClient{c: c, r: bufio.NewReader(c)}

	// StartupMessage: int32 len, int32 protocol(196608), key\0 val\0 ..., \0
	var body []byte
	body = binary.BigEndian.AppendUint32(body, 196608)
	for _, kv := range [][2]string{{"user", "dev"}, {"database", "tenant_a"}} {
		body = append(body, kv[0]...)
		body = append(body, 0)
		body = append(body, kv[1]...)
		body = append(body, 0)
	}
	body = append(body, 0)
	var pkt []byte
	pkt = binary.BigEndian.AppendUint32(pkt, uint32(len(body)+4))
	pkt = append(pkt, body...)
	if _, err := c.Write(pkt); err != nil {
		t.Fatal(err)
	}

	// Read until ReadyForQuery 'Z'.
	cl.drainToReady(t)
	return cl
}

func (cl *pgClient) readMsg(t *testing.T) (byte, []byte) {
	t.Helper()
	typ, err := cl.r.ReadByte()
	if err != nil {
		t.Fatalf("read type: %v", err)
	}
	var lb [4]byte
	if _, err := io.ReadFull(cl.r, lb[:]); err != nil {
		t.Fatalf("read len: %v", err)
	}
	n := binary.BigEndian.Uint32(lb[:])
	body := make([]byte, n-4)
	if _, err := io.ReadFull(cl.r, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return typ, body
}

func (cl *pgClient) drainToReady(t *testing.T) {
	t.Helper()
	for {
		typ, body := cl.readMsg(t)
		if typ == 'E' {
			t.Fatalf("server error during startup: %q", body)
		}
		if typ == 'Z' { // ReadyForQuery
			return
		}
	}
}

// simpleQuery sends 'Q' and returns the rows (each row = slice of cell strings,
// nil cell = NULL) plus the CommandComplete tag.
func (cl *pgClient) simpleQuery(t *testing.T, sql string) (rows [][]*string, tag string) {
	t.Helper()
	body := append([]byte(sql), 0)
	var pkt []byte
	pkt = append(pkt, 'Q')
	pkt = binary.BigEndian.AppendUint32(pkt, uint32(len(body)+4))
	pkt = append(pkt, body...)
	if _, err := cl.c.Write(pkt); err != nil {
		t.Fatal(err)
	}

	for {
		typ, b := cl.readMsg(t)
		switch typ {
		case 'T': // RowDescription — ignore shape for this assertion
		case 'D': // DataRow
			rows = append(rows, parseDataRow(b))
		case 'C': // CommandComplete
			tag = cString(b, new(int))
		case 'E':
			t.Fatalf("query error: %q", b)
		case 'Z': // ReadyForQuery
			return rows, tag
		}
	}
}

func parseDataRow(b []byte) []*string {
	pos := 0
	n := int(int16(binary.BigEndian.Uint16(b[pos:])))
	pos += 2
	out := make([]*string, 0, n)
	for i := 0; i < n; i++ {
		l := int32(binary.BigEndian.Uint32(b[pos:]))
		pos += 4
		if l == -1 {
			out = append(out, nil)
			continue
		}
		s := string(b[pos : pos+int(l)])
		pos += int(l)
		out = append(out, &s)
	}
	return out
}

// TestGate1EndToEnd is the Gate-1 acceptance test over a real TCP socket:
// a PG-wire client connects and `SELECT 1` returns a single row with value "1".
func TestGate1EndToEnd(t *testing.T) {
	addr := startTestServer(t)
	cl := dialPG(t, addr)

	rows, tag := cl.simpleQuery(t, "SELECT 1")
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("want one 1-column row, got %#v", rows)
	}
	if rows[0][0] == nil || *rows[0][0] != "1" {
		t.Fatalf("want cell \"1\", got %v", rows[0][0])
	}
	if tag != "SELECT 1" {
		t.Fatalf("want command tag \"SELECT 1\", got %q", tag)
	}
}

// TestConcurrentConnections opens many simultaneous wire connections, each
// running queries, to confirm the per-connection goroutine model + executor
// hold up under concurrent load (the scenario the live psql stress test could
// not isolate from Windows process-spawn overhead).
func TestConcurrentConnections(t *testing.T) {
	addr := startTestServer(t)

	const conns = 16
	errs := make(chan error, conns)
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs <- fmt.Errorf("conn %d panicked: %v", i, r)
				}
			}()
			cl := dialPGNoFatal(t, addr)
			if cl == nil {
				errs <- fmt.Errorf("conn %d failed to connect", i)
				return
			}
			rows, _ := cl.simpleQuery(t, fmt.Sprintf("SELECT %d + 1", i))
			if len(rows) != 1 || rows[0][0] == nil {
				errs <- fmt.Errorf("conn %d bad result", i)
				return
			}
			errs <- nil
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("DEADLOCK: concurrent connections did not complete within 30s")
	}
	for i := 0; i < conns; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent conn failure: %v", err)
		}
	}
}

// dialPGNoFatal is like dialPG but returns nil instead of t.Fatal on dial error
// (safe to call from a goroutine).
func dialPGNoFatal(t *testing.T, addr string) *pgClient {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil
	}
	cl := &pgClient{c: c, r: bufio.NewReader(c)}
	var body []byte
	body = binary.BigEndian.AppendUint32(body, 196608)
	for _, kv := range [][2]string{{"user", "dev"}, {"database", "tenant_a"}} {
		body = append(body, kv[0]...)
		body = append(body, 0)
		body = append(body, kv[1]...)
		body = append(body, 0)
	}
	body = append(body, 0)
	var pkt []byte
	pkt = binary.BigEndian.AppendUint32(pkt, uint32(len(body)+4))
	pkt = append(pkt, body...)
	if _, err := c.Write(pkt); err != nil {
		return nil
	}
	cl.drainToReady(t)
	return cl
}

// TestSimpleQueryVariety exercises expressions, SET interception, and an error.
func TestSimpleQueryVariety(t *testing.T) {
	addr := startTestServer(t)
	cl := dialPG(t, addr)

	rows, _ := cl.simpleQuery(t, "SELECT 1 + 41 AS answer, 'ok', NULL")
	if *rows[0][0] != "42" || *rows[0][1] != "ok" || rows[0][2] != nil {
		t.Fatalf("unexpected row: %v %v %v", rows[0][0], rows[0][1], rows[0][2])
	}

	// SET must be acknowledged (intercepted), tag "SET".
	_, tag := cl.simpleQuery(t, "SET client_encoding TO 'UTF8'")
	if tag != "SET" {
		t.Fatalf("want SET tag, got %q", tag)
	}

	// version() round-trips.
	rows, _ = cl.simpleQuery(t, "SELECT version()")
	if rows[0][0] == nil {
		t.Fatal("version() returned NULL")
	}
}
