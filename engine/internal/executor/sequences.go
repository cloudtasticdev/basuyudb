package executor

import (
	"fmt"
	"strconv"
	"sync"
)

var (
	seqMu   sync.Mutex
	seqVals = map[string]int64{}
)

// SeqNextVal advances the named sequence and returns the new value.
func SeqNextVal(name string) int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqVals[name]++
	return seqVals[name]
}

// SeqCurrVal returns the current value of a sequence without advancing it.
// Returns an error if the sequence has never been called with SeqNextVal.
func SeqCurrVal(name string) (int64, error) {
	seqMu.Lock()
	defer seqMu.Unlock()
	v, ok := seqVals[name]
	if !ok {
		return 0, fmt.Errorf("sequence %q does not exist or currval not yet called", name)
	}
	return v, nil
}

// SeqSetVal sets the sequence to val and returns val.
func SeqSetVal(name string, val int64) int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqVals[name] = val
	return val
}

// SeqInit initialises a sequence to start-1 if it has not been set before.
func SeqInit(name string, start int64) {
	seqMu.Lock()
	defer seqMu.Unlock()
	if _, ok := seqVals[name]; !ok {
		seqVals[name] = start - 1
	}
}

func seqValStr(v int64) string { return strconv.FormatInt(v, 10) }
