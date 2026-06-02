// Package wire implements the PostgreSQL wire protocol v3 frontend/backend
// message exchange. It depends on engine/internal/{executor,session,auth}; the
// executor does NOT import wire (by design). This is the
// milestone-1 server: startup, authentication, simple + minimal extended query,
// sufficient for Gate 1 (`psql` connects and runs `SELECT 1`).
package wire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// Backend message type bytes (server -> client).
const (
	msgAuthentication = 'R'
	msgParameterStatus = 'S'
	msgBackendKeyData = 'K'
	msgReadyForQuery = 'Z'
	msgRowDescription = 'T'
	msgDataRow = 'D'
	msgCommandComplete = 'C'
	msgErrorResponse = 'E'
	msgEmptyQuery = 'I'
	msgParseComplete = '1'
	msgBindComplete = '2'
	msgNoData = 'n'
	msgParameterDesc = 't'
)

// Frontend message type bytes (client -> server).
const (
	fMsgQuery = 'Q'
	fMsgParse = 'P'
	fMsgBind = 'B'
	fMsgDescribe = 'D'
	fMsgExecute = 'E'
	fMsgSync = 'S'
	fMsgClose = 'C'
	fMsgTerminate = 'X'
	fMsgPassword = 'p'
)

// msgReader reads length-prefixed protocol messages.
type msgReader struct {
	r *bufio.Reader
}

func newMsgReader(r io.Reader) *msgReader { return &msgReader{r: bufio.NewReader(r)} }

// readTyped reads a standard message: 1 type byte + int32 length + body.
func (mr *msgReader) readTyped() (typ byte, body []byte, err error) {
	t, err := mr.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var lenBuf [4]byte
	if _, err = io.ReadFull(mr.r, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n < 4 {
		return 0, nil, fmt.Errorf("wire: invalid message length %d", n)
	}
	body = make([]byte, n-4)
	if _, err = io.ReadFull(mr.r, body); err != nil {
		return 0, nil, err
	}
	return t, body, nil
}

// readStartup reads the untyped startup packet (int32 length + body). It also
// transparently handles the SSLRequest / GSSENCRequest preludes by signalling
// via the returned code.
func (mr *msgReader) readStartup() (code uint32, body []byte, err error) {
	var lenBuf [4]byte
	if _, err = io.ReadFull(mr.r, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n < 8 || n > 1<<20 {
		return 0, nil, fmt.Errorf("wire: invalid startup length %d", n)
	}
	body = make([]byte, n-4)
	if _, err = io.ReadFull(mr.r, body); err != nil {
		return 0, nil, err
	}
	code = binary.BigEndian.Uint32(body[:4])
	return code, body, nil
}

// msgWriter builds and flushes backend messages.
type msgWriter struct {
	w *bufio.Writer
}

func newMsgWriter(w io.Writer) *msgWriter { return &msgWriter{w: bufio.NewWriter(w)} }

func (mw *msgWriter) flush() error { return mw.w.Flush() }

// send writes a typed backend message with the given body.
func (mw *msgWriter) send(typ byte, body []byte) error {
	if err := mw.w.WriteByte(typ); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)+4))
	if _, err := mw.w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := mw.w.Write(body)
	return err
}

// ---- message body builders ----

// builder is a small append-only encoder for message bodies.
type builder struct{ b []byte }

func (e *builder) int16(v int16) { e.b = binary.BigEndian.AppendUint16(e.b, uint16(v)) }
func (e *builder) int32(v int32) { e.b = binary.BigEndian.AppendUint32(e.b, uint32(v)) }
func (e *builder) byte1(v byte) { e.b = append(e.b, v) }
func (e *builder) str(s string) { e.b = append(e.b, s...); e.b = append(e.b, 0) }
func (e *builder) bytes(p []byte) { e.b = append(e.b, p...) }

// authenticationOk: AuthenticationOk (R, int32 0).
func authenticationOk() []byte {
	var e builder
	e.int32(0)
	return e.b
}

// authenticationCleartext: AuthenticationCleartextPassword (R, int32 3).
func authenticationCleartext() []byte {
	var e builder
	e.int32(3)
	return e.b
}

// parameterStatus: a server parameter (name, value).
func parameterStatus(name, val string) []byte {
	var e builder
	e.str(name)
	e.str(val)
	return e.b
}

// backendKeyData: process id + secret key.
func backendKeyData(pid, secret int32) []byte {
	var e builder
	e.int32(pid)
	e.int32(secret)
	return e.b
}

// readyForQuery: transaction status ('I' idle).
func readyForQuery(status byte) []byte { return []byte{status} }

// commandComplete: command tag.
func commandComplete(tag string) []byte {
	var e builder
	e.str(tag)
	return e.b
}

// errorResponse: a PG ErrorResponse with severity, SQLSTATE, and message.
func errorResponse(severity, sqlstate, message string) []byte {
	var e builder
	e.byte1('S')
	e.str(severity)
	e.byte1('V')
	e.str(severity)
	e.byte1('C')
	e.str(sqlstate)
	e.byte1('M')
	e.str(message)
	e.byte1(0) // terminator
	return e.b
}

// cString reads a NUL-terminated string from body starting at *pos.
func cString(body []byte, pos *int) string {
	start := *pos
	for *pos < len(body) && body[*pos] != 0 {
		*pos++
	}
	s := string(body[start:*pos])
	*pos++ // skip NUL
	return s
}
