package wire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"golang.org/x/crypto/pbkdf2"
)

func hmacSHA256Test(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func atoiTest(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func parseAttrsTest(s string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Split(s, ",") {
		if eq := strings.IndexByte(f, '='); eq > 0 {
			out[f[:eq]] = f[eq+1:]
		}
	}
	return out
}

func TestParseRolePassword(t *testing.T) {
	cases := []struct {
		sql      string
		wantName string
		wantPw   string
		wantOK   bool
	}{
		{"CREATE ROLE alice LOGIN PASSWORD 'secret'", "alice", "secret", true},
		{"CREATE ROLE alice WITH LOGIN PASSWORD 'secret'", "alice", "secret", true},
		{"CREATE USER bob PASSWORD 'p@ss w0rd'", "bob", "p@ss w0rd", true},
		{"ALTER ROLE carol PASSWORD 'newpw'", "carol", "newpw", true},
		{"ALTER USER dave WITH PASSWORD 'x'", "dave", "x", true},
		{"CREATE ROLE eve PASSWORD 'it''s'", "eve", "it's", true},  // '' escape
		{`CREATE ROLE "Frank" PASSWORD 'pw'`, "Frank", "pw", true}, // quoted name
		{"CREATE ROLE noauth LOGIN", "", "", false},                // no PASSWORD
		{"ALTER ROLE g PASSWORD NULL", "", "", false},              // NULL, not a literal
		{"CREATE ROLE h PASSWORD 'unterminated", "", "", false},    // unterminated
	}
	for _, tc := range cases {
		name, pw, ok := parseRolePassword(tc.sql)
		if ok != tc.wantOK || name != tc.wantName || pw != tc.wantPw {
			t.Errorf("parseRolePassword(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.sql, name, pw, ok, tc.wantName, tc.wantPw, tc.wantOK)
		}
	}
}

func TestCopyResponseBodyBinary(t *testing.T) {
	text := copyResponseBody(2, false)
	if text[0] != 0 || text[3] != 0 || text[5] != 0 {
		t.Errorf("text COPY response should use format 0, got %v", text)
	}
	bin := copyResponseBody(2, true)
	if bin[0] != 1 {
		t.Errorf("binary COPY response overall format should be 1, got %d", bin[0])
	}
	// bytes: [fmt][int16 ncols][int16 col0][int16 col1]
	if bin[2] != 2 {
		t.Errorf("expected ncols=2, got %d", bin[2])
	}
	if bin[4] != 1 || bin[6] != 1 {
		t.Errorf("binary COPY per-column format should be 1, got col0=%d col1=%d", bin[4], bin[6])
	}
}

// TestRunSCRAMFraming drives the server-side runSCRAM over a net.Pipe against an
// in-process fake client that speaks the PG SASL framing, verifying both a
// successful exchange and a wrong-password rejection.
func TestRunSCRAMFraming(t *testing.T) {
	t.Run("success", func(t *testing.T) { runSCRAMFramingCase(t, "hunter2", "hunter2", true) })
	t.Run("wrongpw", func(t *testing.T) { runSCRAMFramingCase(t, "hunter2", "nope", false) })
}

func runSCRAMFramingCase(t *testing.T, storedPw, attemptPw string, wantOK bool) {
	t.Helper()
	v, err := auth.NewSCRAMVerifier(storedPw)
	if err != nil {
		t.Fatal(err)
	}
	serverNC, clientNC := net.Pipe()
	c := &conn{
		netConn: serverNC,
		mr:      newMsgReader(serverNC),
		mw:      newMsgWriter(serverNC),
	}

	srvErrCh := make(chan error, 1)
	go func() {
		err := c.runSCRAM(v)
		// Close the server end so a client blocked reading SASLFinal (which is not
		// sent on failure) unblocks with EOF instead of deadlocking the test.
		_ = serverNC.Close()
		srvErrCh <- err
	}()

	fc := &fakeSASLClient{nc: clientNC, password: attemptPw, clientNonce: "fakeclientnonce12345"}
	clientErr := fc.run(t)

	srvErr := <-srvErrCh
	_ = clientNC.Close()

	if wantOK {
		if srvErr != nil {
			t.Fatalf("server runSCRAM failed: %v", srvErr)
		}
		if clientErr != nil {
			t.Fatalf("client exchange failed: %v", clientErr)
		}
	} else {
		if srvErr == nil {
			t.Fatal("expected server to reject wrong password")
		}
	}
}

// fakeSASLClient speaks the minimal PG SASL framing needed to drive runSCRAM.
type fakeSASLClient struct {
	nc          net.Conn
	password    string
	clientNonce string
}

func (fc *fakeSASLClient) run(t *testing.T) error {
	t.Helper()
	mr := newMsgReader(fc.nc)
	mw := newMsgWriter(fc.nc)

	// 1. Read AuthenticationSASL (R, int32 10, mechanism list).
	typ, body, err := mr.readTyped()
	if err != nil {
		return err
	}
	if typ != msgAuthentication || binary.BigEndian.Uint32(body[:4]) != authSASL {
		t.Fatalf("expected AuthenticationSASL, got typ=%q code=%d", typ, binary.BigEndian.Uint32(body[:4]))
	}

	// 2. Send SASLInitialResponse ('p'): mechanism CString + int32 len + client-first.
	clientFirstBare := "n=,r=" + fc.clientNonce
	clientFirst := "n,," + clientFirstBare
	var ib builder
	ib.str(scramMechanism)
	ib.int32(int32(len(clientFirst)))
	ib.bytes([]byte(clientFirst))
	if err := mw.send(fMsgPassword, ib.b); err != nil {
		return err
	}
	if err := mw.flush(); err != nil {
		return err
	}

	// 3. Read AuthenticationSASLContinue (R, int32 11, server-first).
	typ, body, err = mr.readTyped()
	if err != nil {
		return err
	}
	if typ != msgAuthentication || binary.BigEndian.Uint32(body[:4]) != authSASLContinue {
		// On wrong-password we won't reach here in the failure case (server still
		// sends Continue before Step2; failure happens at Step2). So treat an
		// unexpected message as the connection closing.
		return io.ErrUnexpectedEOF
	}
	serverFirst := string(body[4:])

	// 4. Compute and send client-final.
	attrs := parseAttrsTest(serverFirst)
	salt, _ := base64.StdEncoding.DecodeString(attrs["s"])
	iters := atoiTest(attrs["i"])
	combinedNonce := attrs["r"]

	salted := pbkdf2.Key([]byte(fc.password), salt, iters, sha256.Size, sha256.New)
	clientKey := hmacSHA256Test(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	gs2 := base64.StdEncoding.EncodeToString([]byte("n,,"))
	cfwp := "c=" + gs2 + ",r=" + combinedNonce
	authMessage := clientFirstBare + "," + serverFirst + "," + cfwp
	clientSig := hmacSHA256Test(storedKey[:], authMessage)
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSig[i]
	}
	clientFinal := cfwp + ",p=" + base64.StdEncoding.EncodeToString(proof)
	if err := mw.send(fMsgPassword, []byte(clientFinal)); err != nil {
		return err
	}
	if err := mw.flush(); err != nil {
		return err
	}

	// 5. Read AuthenticationSASLFinal (R, int32 12) — only present on success.
	typ, body, err = mr.readTyped()
	if err != nil {
		return err
	}
	if typ != msgAuthentication || binary.BigEndian.Uint32(body[:4]) != authSASLFinal {
		return io.ErrUnexpectedEOF
	}
	return nil
}
