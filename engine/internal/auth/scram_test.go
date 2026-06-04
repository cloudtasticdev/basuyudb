package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

// fakeClient runs the client side of a SCRAM-SHA-256 exchange in-process so we
// can exercise the full server state machine without a network or PG driver.
type fakeClient struct {
	username    string
	password    string
	clientNonce string
}

func (c *fakeClient) clientFirst() (full, bare string) {
	bare = "n=" + saslPrepName(c.username) + ",r=" + c.clientNonce
	full = "n,," + bare
	return full, bare
}

// clientFinal computes the client-final-message given the server-first-message
// and the bare client-first-message.
func (c *fakeClient) clientFinal(t *testing.T, clientFirstBare, serverFirst string) string {
	t.Helper()
	attrs, err := parseAttrs(serverFirst)
	if err != nil {
		t.Fatalf("parse server-first: %v", err)
	}
	salt, err := base64.StdEncoding.DecodeString(attrs["s"])
	if err != nil {
		t.Fatalf("bad salt: %v", err)
	}
	var iters int
	if _, err := fmtSscan(attrs["i"], &iters); err != nil {
		t.Fatalf("bad iters: %v", err)
	}
	combinedNonce := attrs["r"]

	salted := pbkdf2.Key([]byte(c.password), salt, iters, sha256.Size, sha256.New)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)

	gs2 := base64.StdEncoding.EncodeToString([]byte("n,,"))
	cfwp := "c=" + gs2 + ",r=" + combinedNonce
	authMessage := clientFirstBare + "," + serverFirst + "," + cfwp

	clientSig := hmacSHA256(storedKey[:], []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSig[i]
	}
	return cfwp + ",p=" + base64.StdEncoding.EncodeToString(proof)
}

// verifyServerSignature checks the server-final-message v= value.
func (c *fakeClient) verifyServerSignature(t *testing.T, serverFirst, clientFirstBare, clientFinal, serverFinal string) {
	t.Helper()
	attrs, _ := parseAttrs(serverFirst)
	salt, _ := base64.StdEncoding.DecodeString(attrs["s"])
	var iters int
	fmtSscan(attrs["i"], &iters)
	salted := pbkdf2.Key([]byte(c.password), salt, iters, sha256.Size, sha256.New)
	serverKey := hmacSHA256(salted, []byte("Server Key"))
	cfwp := clientFinalWithoutProof(clientFinal)
	authMessage := clientFirstBare + "," + serverFirst + "," + cfwp
	want := "v=" + base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, []byte(authMessage)))
	if serverFinal != want {
		t.Fatalf("server signature mismatch: got %q want %q", serverFinal, want)
	}
}

func saslPrepName(s string) string {
	// We do not implement SASLprep; PG clients send the bare name and so do we.
	return s
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errNonDigit
		}
		n = n*10 + int(r-'0')
	}
	*out = n
	return 1, nil
}

var errNonDigit = &scramTestErr{"non-digit in integer"}

type scramTestErr struct{ s string }

func (e *scramTestErr) Error() string { return e.s }

func runExchange(t *testing.T, v SCRAMVerifier, cl *fakeClient) error {
	t.Helper()
	sc := NewSCRAMServerConversation(v)
	full, bare := cl.clientFirst()
	serverFirst, err := sc.Step1(full)
	if err != nil {
		return err
	}
	cf := cl.clientFinal(t, bare, serverFirst)
	serverFinal, err := sc.Step2(cf)
	if err != nil {
		return err
	}
	cl.verifyServerSignature(t, serverFirst, bare, cf, serverFinal)
	return nil
}

func TestSCRAMHappyPath(t *testing.T) {
	v, err := NewSCRAMVerifier("s3cr3t-pässword")
	if err != nil {
		t.Fatal(err)
	}
	cl := &fakeClient{username: "alice", password: "s3cr3t-pässword", clientNonce: "rOprNGfwEbeRWgbNEkqO"}
	if err := runExchange(t, v, cl); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestSCRAMWrongPassword(t *testing.T) {
	v, err := NewSCRAMVerifier("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	cl := &fakeClient{username: "alice", password: "wrong-horse", clientNonce: "abcdefghijklmnop"}
	err = runExchange(t, v, cl)
	if err == nil {
		t.Fatal("expected authentication failure for wrong password")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("expected auth-failed error, got %v", err)
	}
}

func TestSCRAMRejectsChannelBinding(t *testing.T) {
	v, _ := NewSCRAMVerifier("pw")
	sc := NewSCRAMServerConversation(v)
	// gs2 flag "p=tls-server-end-point" requests channel binding -> must reject.
	_, err := sc.Step1("p=tls-server-end-point,,n=alice,r=nonce123")
	if err == nil {
		t.Fatal("expected channel-binding rejection")
	}
}

func TestSCRAMRejectsTamperedChannelBinding(t *testing.T) {
	v, _ := NewSCRAMVerifier("pw")
	cl := &fakeClient{username: "bob", password: "pw", clientNonce: "nonceNONCEnonce123"}
	sc := NewSCRAMServerConversation(v)
	full, bare := cl.clientFirst()
	serverFirst, err := sc.Step1(full)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper: send a c= value claiming channel binding ("eSws" = base64("y,,")).
	attrs, _ := parseAttrs(serverFirst)
	tampered := "c=" + base64.StdEncoding.EncodeToString([]byte("y,,")) + ",r=" + attrs["r"] + ",p=" +
		base64.StdEncoding.EncodeToString([]byte("ignored"))
	_ = bare
	if _, err := sc.Step2(tampered); err == nil {
		t.Fatal("expected channel-binding mismatch rejection")
	}
}

func TestDeriveVerifierDeterministic(t *testing.T) {
	salt := []byte("0123456789abcdef")
	v1 := deriveVerifier("hunter2", salt, 4096)
	v2 := deriveVerifier("hunter2", salt, 4096)
	if !hmac.Equal(v1.StoredKey, v2.StoredKey) || !hmac.Equal(v1.ServerKey, v2.ServerKey) {
		t.Fatal("deriveVerifier not deterministic")
	}
	if hmac.Equal(v1.StoredKey, v1.ServerKey) {
		t.Fatal("StoredKey and ServerKey should differ")
	}
}

func TestRoleStoreUpsertLookup(t *testing.T) {
	rs := NewRoleStore()
	if rs.Has("nobody") {
		t.Fatal("empty store should not have role")
	}
	if err := rs.UpsertPassword("carol", "p@ss"); err != nil {
		t.Fatal(err)
	}
	v, ok := rs.Lookup("carol")
	if !ok {
		t.Fatal("expected carol to exist")
	}
	cl := &fakeClient{username: "carol", password: "p@ss", clientNonce: "qqqqwwwweeeerrrr"}
	if err := runExchange(t, v, cl); err != nil {
		t.Fatalf("round-trip via store failed: %v", err)
	}
	if err := rs.Delete("carol"); err != nil {
		t.Fatal(err)
	}
	if rs.Has("carol") {
		t.Fatal("expected carol deleted")
	}
}
