package auth

// SCRAM-SHA-256 (RFC 5802 + RFC 7677) server-side state machine and crypto.
//
// This file is pure and unit-testable: it knows nothing about the PostgreSQL
// wire protocol or SASL message framing (that lives in internal/wire/scram.go).
// It implements the SCRAM math and a small server-side conversation driver.
//
// Channel binding is intentionally NOT supported: we advertise only
// "SCRAM-SHA-256" (never "SCRAM-SHA-256-PLUS"), and we reject any client that
// requests channel binding (gs2 flag 'p') or sends a c= value other than the
// no-binding header base64("n,,") == "biws".
//
// SECURITY: we store only the SCRAM verifier (salt, iterations, StoredKey,
// ServerKey) — never the plaintext password. Proof verification uses a
// constant-time compare. Server nonces are drawn from crypto/rand.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// DefaultSCRAMIterations is the PBKDF2 iteration count used when provisioning a
// new verifier. RFC 7677 mandates a minimum of 4096; PostgreSQL's default is
// also 4096. Higher is slower per login but more resistant to offline cracking.
const DefaultSCRAMIterations = 4096

// scramSaltLen is the length in bytes of a freshly generated per-role salt.
const scramSaltLen = 16

// scramNonceLen is the length in bytes of the server's nonce contribution
// (before base64 encoding).
const scramNonceLen = 18

// SCRAMVerifier is the stored, password-derived authentication material for a
// role. It NEVER contains the plaintext password. It is the SCRAM equivalent of
// a password hash and is safe to persist.
type SCRAMVerifier struct {
	Salt       []byte
	Iterations int
	StoredKey  []byte // SHA256(ClientKey)
	ServerKey  []byte // HMAC(SaltedPassword, "Server Key")
}

// NewSCRAMVerifier derives a SCRAMVerifier from a plaintext password using a
// freshly generated random salt and DefaultSCRAMIterations. The plaintext is
// used only transiently here and is never stored.
func NewSCRAMVerifier(password string) (SCRAMVerifier, error) {
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return SCRAMVerifier{}, fmt.Errorf("scram: salt generation: %w", err)
	}
	return deriveVerifier(password, salt, DefaultSCRAMIterations), nil
}

// deriveVerifier computes the verifier fields from a password, salt, and
// iteration count. Exposed (lowercase) for deterministic unit tests.
func deriveVerifier(password string, salt []byte, iterations int) SCRAMVerifier {
	salted := pbkdf2.Key([]byte(password), salt, iterations, sha256.Size, sha256.New)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, []byte("Server Key"))
	return SCRAMVerifier{
		Salt:       append([]byte(nil), salt...),
		Iterations: iterations,
		StoredKey:  storedKey[:],
		ServerKey:  serverKey,
	}
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// SCRAMServerConversation drives the server side of one SCRAM-SHA-256 exchange.
// Lifecycle:
//
//	sc := NewSCRAMServerConversation(verifier)
//	serverFirst, err := sc.Step1(clientFirstMessage)   // -> server-first-message
//	serverFinal, err  := sc.Step2(clientFinalMessage)  // -> server-final-message
//
// On any verification failure Step2 returns an error and the caller MUST reject
// authentication (do not leak which check failed to the client beyond a generic
// failure).
type SCRAMServerConversation struct {
	verifier SCRAMVerifier

	clientNonce      string
	serverNonce      string
	clientFirstBare  string // "n=...,r=..." (without the gs2 header)
	serverFirstMsg   string // "r=...,s=...,i=..."
	gs2HeaderB64     string // base64 of the gs2 header the client must echo in c=
	authMessageReady bool
}

// NewSCRAMServerConversation starts a conversation against a stored verifier.
func NewSCRAMServerConversation(v SCRAMVerifier) *SCRAMServerConversation {
	return &SCRAMServerConversation{verifier: v}
}

// Step1 consumes the client-first-message and returns the server-first-message.
//
// client-first-message = gs2-header "n=" [authzid] ",r=" client-nonce
// gs2-header            = ("n" / "y" / "p=cb-name") "," [authzid] ","
//
// We only accept gs2 flags "n" (no channel binding) or "y" (client supports cb
// but server didn't advertise PLUS). We reject "p=" (channel binding required),
// since we never advertise SCRAM-SHA-256-PLUS.
func (s *SCRAMServerConversation) Step1(clientFirst string) (string, error) {
	// Split off the gs2 header: it is the first two comma-separated fields
	// (flag, authzid) — the bare message begins at the 3rd field.
	gs2Flag, rest, ok := splitFirst(clientFirst)
	if !ok {
		return "", errors.New("scram: malformed client-first-message (no gs2 flag)")
	}
	switch {
	case gs2Flag == "n" || gs2Flag == "y":
		// no channel binding requested — acceptable.
	case strings.HasPrefix(gs2Flag, "p="):
		return "", errors.New("scram: channel binding requested but not supported")
	default:
		return "", fmt.Errorf("scram: invalid gs2 channel-binding flag %q", gs2Flag)
	}
	authzid, bare, ok := splitFirst(rest)
	if !ok {
		return "", errors.New("scram: malformed client-first-message (no authzid field)")
	}
	_ = authzid // authzid (impersonation) is ignored; we authenticate as the bound role.

	s.clientFirstBare = bare
	// The gs2 header the client must echo in its c= attribute is everything up
	// to and including the second comma.
	gs2Header := clientFirst[:len(clientFirst)-len(bare)]
	s.gs2HeaderB64 = base64.StdEncoding.EncodeToString([]byte(gs2Header))

	attrs, err := parseAttrs(bare)
	if err != nil {
		return "", err
	}
	cnonce, ok := attrs["r"]
	if !ok || cnonce == "" {
		return "", errors.New("scram: client-first-message missing nonce")
	}
	s.clientNonce = cnonce

	// Generate the server nonce contribution and form combined nonce.
	srvRand := make([]byte, scramNonceLen)
	if _, err := rand.Read(srvRand); err != nil {
		return "", fmt.Errorf("scram: server nonce generation: %w", err)
	}
	s.serverNonce = cnonce + base64.StdEncoding.EncodeToString(srvRand)

	s.serverFirstMsg = "r=" + s.serverNonce +
		",s=" + base64.StdEncoding.EncodeToString(s.verifier.Salt) +
		",i=" + strconv.Itoa(s.verifier.Iterations)
	s.authMessageReady = true
	return s.serverFirstMsg, nil
}

// Step2 consumes the client-final-message, verifies the client proof, and
// returns the server-final-message ("v=<ServerSignature>"). A non-nil error
// means authentication MUST be rejected.
//
// client-final-message = "c=" base64(gs2-header) ",r=" nonce ",p=" proof
func (s *SCRAMServerConversation) Step2(clientFinal string) (string, error) {
	if !s.authMessageReady {
		return "", errors.New("scram: Step2 called before Step1")
	}
	attrs, err := parseAttrs(clientFinal)
	if err != nil {
		return "", err
	}
	// Verify channel-binding echo: must equal base64 of the original gs2 header
	// (== "biws" for "n,,"). Reject any other value (e.g. an attempted PLUS).
	cb, ok := attrs["c"]
	if !ok {
		return "", errors.New("scram: client-final-message missing channel binding (c=)")
	}
	if cb != s.gs2HeaderB64 {
		return "", errors.New("scram: channel-binding mismatch")
	}
	// Verify the full nonce echoes the combined nonce.
	rnonce, ok := attrs["r"]
	if !ok || rnonce != s.serverNonce {
		return "", errors.New("scram: nonce mismatch in client-final-message")
	}
	proofB64, ok := attrs["p"]
	if !ok {
		return "", errors.New("scram: client-final-message missing proof (p=)")
	}
	clientProof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil {
		return "", fmt.Errorf("scram: bad base64 client proof: %w", err)
	}

	// client-final-without-proof is the client-final-message with the ",p=..."
	// suffix removed.
	cfwp := clientFinalWithoutProof(clientFinal)
	authMessage := s.clientFirstBare + "," + s.serverFirstMsg + "," + cfwp

	clientSignature := hmacSHA256(s.verifier.StoredKey, []byte(authMessage))
	if len(clientProof) != len(clientSignature) {
		return "", errors.New("scram: client proof length mismatch")
	}
	// ClientKey = ClientProof XOR ClientSignature; verify SHA256(ClientKey)==StoredKey.
	clientKey := make([]byte, len(clientProof))
	for i := range clientProof {
		clientKey[i] = clientProof[i] ^ clientSignature[i]
	}
	computedStored := sha256.Sum256(clientKey)
	if subtle.ConstantTimeCompare(computedStored[:], s.verifier.StoredKey) != 1 {
		return "", errors.New("scram: authentication failed (bad password)")
	}

	// ServerSignature = HMAC(ServerKey, AuthMessage).
	serverSignature := hmacSHA256(s.verifier.ServerKey, []byte(authMessage))
	return "v=" + base64.StdEncoding.EncodeToString(serverSignature), nil
}

// splitFirst splits s on the first comma into (before, after, found).
func splitFirst(s string) (string, string, bool) {
	i := strings.IndexByte(s, ',')
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+1:], true
}

// clientFinalWithoutProof returns the client-final-message up to (but not
// including) the ",p=" proof attribute. The proof is always the last attribute.
func clientFinalWithoutProof(clientFinal string) string {
	if i := strings.LastIndex(clientFinal, ",p="); i >= 0 {
		return clientFinal[:i]
	}
	return clientFinal
}

// parseAttrs parses a SCRAM attribute list "a=v,b=v,..." into a map. Per RFC
// 5802 each attribute is a single letter followed by '=' and a value that may
// itself contain '=' (e.g. base64). Splitting only on the first '=' per field
// preserves such values.
func parseAttrs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, field := range strings.Split(s, ",") {
		if field == "" {
			continue
		}
		eq := strings.IndexByte(field, '=')
		if eq < 1 {
			return nil, fmt.Errorf("scram: malformed attribute %q", field)
		}
		out[field[:eq]] = field[eq+1:]
	}
	return out, nil
}
