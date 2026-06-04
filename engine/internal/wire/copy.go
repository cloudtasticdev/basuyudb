package wire

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
)

// handleCopy drives the COPY sub-protocol for a parsed COPY statement. It writes
// CommandComplete but not ReadyForQuery — the caller (handleSimpleQuery) ends
// the query.
func (c *conn) handleCopy(cp *ast.CopyStmt) {
	// Binary COPY is supported: the executor's FormatCopyData/ParseCopyData
	// handle the full PGCOPY binary byte stream (signature + rows + trailer) when
	// format == "binary". Here we only handle the wire framing and advertise the
	// correct per-column format codes.
	if cp.IsFrom {
		c.handleCopyIn(cp)
		return
	}
	c.handleCopyOut(cp)
}

// copyResponseBody builds a CopyInResponse/CopyOutResponse body: an overall
// format code (0 = textual, 1 = binary) followed by one per-column format code.
// All columns share the overall format (PG never mixes text/binary columns in a
// single COPY).
func copyResponseBody(ncols int, binary bool) []byte {
	var fmtCode byte
	var colCode int16
	if binary {
		fmtCode = 1
		colCode = 1
	}
	var e builder
	e.b = append(e.b, fmtCode)
	e.int16(int16(ncols))
	for i := 0; i < ncols; i++ {
		e.int16(colCode)
	}
	return e.b
}

func (c *conn) handleCopyOut(cp *ast.CopyStmt) {
	isBinary := strings.EqualFold(cp.Format, "binary")
	res, err := c.srv.exec.CopyTo(context.Background(), c.sess, cp)
	if err != nil {
		c.sendExecError(err)
		return
	}
	c.mw.send(msgCopyOutResponse, copyResponseBody(len(res.Columns), isBinary))
	// For binary COPY OUT the executor produces the COMPLETE stream (PGCOPY
	// signature + rows + trailer); we send it as a single CopyData message. For
	// text/csv it produces the row bytes the same way.
	data := executor.FormatCopyData(res, cp.Format, cp.Delimiter, cp.Header)
	if len(data) > 0 {
		c.mw.send(msgCopyData, data)
	}
	c.mw.send(msgCopyDone, nil)
	c.mw.send(msgCommandComplete, commandComplete(fmt.Sprintf("COPY %d", len(res.Rows))))
}

func (c *conn) handleCopyIn(cp *ast.CopyStmt) {
	isBinary := strings.EqualFold(cp.Format, "binary")
	c.mw.send(msgCopyInResponse, copyResponseBody(len(cp.Columns), isBinary))
	c.mw.flush()

	var buf bytes.Buffer
	for {
		typ, body, err := c.mr.readTyped()
		if err != nil {
			return
		}
		switch typ {
		case fMsgCopyData:
			buf.Write(body)
		case fMsgCopyDone:
			var rows [][]executor.Datum
			var perr error
			if isBinary {
				// Binary COPY carries no per-field type OIDs in the stream, so
				// decode using the destination column OIDs for a fully typed
				// result (int4/float8/timestamp/uuid/etc.).
				oids, oerr := c.srv.exec.CopyTargetOIDs(context.Background(), c.sess, cp.Table, cp.Columns)
				if oerr != nil {
					c.sendExecError(oerr)
					return
				}
				rows, perr = executor.ParseCopyDataBinary(buf.Bytes(), oids)
			} else {
				rows, perr = executor.ParseCopyData(buf.Bytes(), cp.Format, cp.Delimiter, cp.Header, len(cp.Columns))
			}
			if perr != nil {
				c.sendExecError(perr)
				return
			}
			n, ierr := c.srv.exec.CopyFrom(context.Background(), c.sess, cp.Table, cp.Columns, rows)
			if ierr != nil {
				c.sendExecError(ierr)
				return
			}
			c.mw.send(msgCommandComplete, commandComplete(fmt.Sprintf("COPY %d", n)))
			return
		case fMsgCopyFail:
			pos := 0
			c.sendError("ERROR", "57014", "COPY from stdin failed: "+cString(body, &pos))
			return
		default:
			// Anything else aborts the COPY.
			c.sendError("ERROR", "08P01", fmt.Sprintf("unexpected message %q during COPY", typ))
			return
		}
	}
}
