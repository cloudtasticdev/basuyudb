package wire

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
)

// handleCopy drives the COPY sub-protocol for a parsed COPY statement. It writes
// CommandComplete but not ReadyForQuery — the caller (handleSimpleQuery) ends
// the query.
func (c *conn) handleCopy(cp *ast.CopyStmt) {
	if cp.IsFrom {
		c.handleCopyIn(cp)
		return
	}
	c.handleCopyOut(cp)
}

// copyResponseBody builds a CopyInResponse/CopyOutResponse body: overall format
// (0 = textual) followed by one per-column format code (all 0/text).
func copyResponseBody(ncols int) []byte {
	var e builder
	e.b = append(e.b, 0) // text format
	e.int16(int16(ncols))
	for i := 0; i < ncols; i++ {
		e.int16(0)
	}
	return e.b
}

func (c *conn) handleCopyOut(cp *ast.CopyStmt) {
	res, err := c.srv.exec.CopyTo(context.Background(), c.sess, cp)
	if err != nil {
		c.sendExecError(err)
		return
	}
	c.mw.send(msgCopyOutResponse, copyResponseBody(len(res.Columns)))
	data := executor.FormatCopyData(res, cp.Format, cp.Delimiter, cp.Header)
	if len(data) > 0 {
		c.mw.send(msgCopyData, data)
	}
	c.mw.send(msgCopyDone, nil)
	c.mw.send(msgCommandComplete, commandComplete(fmt.Sprintf("COPY %d", len(res.Rows))))
}

func (c *conn) handleCopyIn(cp *ast.CopyStmt) {
	c.mw.send(msgCopyInResponse, copyResponseBody(len(cp.Columns)))
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
			rows, perr := executor.ParseCopyData(buf.Bytes(), cp.Format, cp.Delimiter, cp.Header, len(cp.Columns))
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
