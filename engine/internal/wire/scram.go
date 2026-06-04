package wire

// PostgreSQL SASL (SCRAM-SHA-256) message framing for the wire protocol.
//
// This file translates between the PG SASL authentication messages and the
// pure SCRAM state machine in internal/auth/scram.go. PG SASL flow (see
// https://www.postgresql.org/docs/current/sasl-authentication.html):
//
//	S: AuthenticationSASL          (R, int32 10)  + NUL-terminated mechanism list
//	C: SASLInitialResponse         ('p')          mechanism + int32 len + client-first
//	S: AuthenticationSASLContinue  (R, int32 11)  + server-first-message
//	C: SASLResponse                ('p')          client-final-message (raw)
//	S: AuthenticationSASLFinal     (R, int32 12)  + server-final-message (v=...)
//	S: AuthenticationOk            (R, int32 0)
//
// We advertise only "SCRAM-SHA-256" (never "-PLUS"): no channel binding.

import (
	"encoding/binary"
	"fmt"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
)

const scramMechanism = "SCRAM-SHA-256"

// localRoleSession mints an auth.Session for a SCRAM-authenticated local role.
// It reuses auth.SessionFromClaims (the same validated construction path the
// JWT flow uses) so the NamespaceID is validated identically — there is no
// weaker session-construction path. The role name becomes the session subject
// and role; the connecting database becomes the single granted namespace.
func localRoleSession(namespace, user string) (auth.Session, error) {
	s, err := auth.SessionFromClaims(&auth.SessionClaims{
		Sub:             user,
		Jti:             "local:" + user,
		Role:            "user",
		NamespaceAccess: []string{namespace},
		NamespaceID:     namespace,
	})
	if err != nil {
		return auth.Session{}, err
	}
	s.Branch = "main"
	return s, nil
}

// SASL AuthenticationRequest sub-codes (the int32 after 'R').
const (
	authSASL         = 10
	authSASLContinue = 11
	authSASLFinal    = 12
)

// authenticationSASL builds the AuthenticationSASL body: int32 10 followed by a
// NUL-terminated list of supported mechanism names, terminated by an extra NUL.
func authenticationSASL() []byte {
	var e builder
	e.int32(authSASL)
	e.str(scramMechanism) // mechanism name + NUL
	e.byte1(0)            // list terminator
	return e.b
}

// authenticationSASLContinue builds the AuthenticationSASLContinue body: int32
// 11 followed by the server-first SASL data (not NUL-terminated).
func authenticationSASLContinue(data string) []byte {
	var e builder
	e.int32(authSASLContinue)
	e.bytes([]byte(data))
	return e.b
}

// authenticationSASLFinal builds the AuthenticationSASLFinal body: int32 12
// followed by the server-final SASL data (not NUL-terminated).
func authenticationSASLFinal(data string) []byte {
	var e builder
	e.int32(authSASLFinal)
	e.bytes([]byte(data))
	return e.b
}

// runSCRAM performs the full server-side SCRAM-SHA-256 SASL exchange against
// the supplied verifier. It assumes the caller has NOT yet sent any auth
// message. On success it returns nil (the caller then sends AuthenticationOk and
// proceeds); on failure it returns an error (the caller sends a FATAL and
// closes). It does not send AuthenticationOk itself.
func (c *conn) runSCRAM(v auth.SCRAMVerifier) error {
	// 1. Advertise the mechanism.
	if err := c.mw.send(msgAuthentication, authenticationSASL()); err != nil {
		return err
	}
	if err := c.mw.flush(); err != nil {
		return err
	}

	// 2. Read SASLInitialResponse ('p'): mechanism name (CString) + int32 length
	//    + client-first-message bytes.
	typ, body, err := c.mr.readTyped()
	if err != nil {
		return err
	}
	if typ != fMsgPassword {
		return fmt.Errorf("scram: expected SASLInitialResponse, got %q", typ)
	}
	pos := 0
	mech := cString(body, &pos)
	if mech != scramMechanism {
		return fmt.Errorf("scram: unsupported SASL mechanism %q", mech)
	}
	if pos+4 > len(body) {
		return fmt.Errorf("scram: truncated SASLInitialResponse")
	}
	initLen := int32(binary.BigEndian.Uint32(body[pos:]))
	pos += 4
	// initLen == -1 means "no initial response" — but PG SCRAM always includes
	// the client-first-message, so treat absence as an error.
	if initLen < 0 || pos+int(initLen) > len(body) {
		return fmt.Errorf("scram: invalid SASLInitialResponse length %d", initLen)
	}
	clientFirst := string(body[pos : pos+int(initLen)])

	conv := auth.NewSCRAMServerConversation(v)
	serverFirst, err := conv.Step1(clientFirst)
	if err != nil {
		return err
	}

	// 3. Send AuthenticationSASLContinue with server-first-message.
	if err := c.mw.send(msgAuthentication, authenticationSASLContinue(serverFirst)); err != nil {
		return err
	}
	if err := c.mw.flush(); err != nil {
		return err
	}

	// 4. Read SASLResponse ('p'): the body is the raw client-final-message.
	typ, body, err = c.mr.readTyped()
	if err != nil {
		return err
	}
	if typ != fMsgPassword {
		return fmt.Errorf("scram: expected SASLResponse, got %q", typ)
	}
	clientFinal := string(body)

	serverFinal, err := conv.Step2(clientFinal)
	if err != nil {
		return err
	}

	// 5. Send AuthenticationSASLFinal with server-final-message (v=...).
	if err := c.mw.send(msgAuthentication, authenticationSASLFinal(serverFinal)); err != nil {
		return err
	}
	return c.mw.flush()
}
