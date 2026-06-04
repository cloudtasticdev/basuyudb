package consensus

import (
	"net"
	"testing"
)

// freeAddr returns a loopback host:port bound to an OS-assigned free port. Using
// dynamic ports (instead of fixed 6302x literals) removes a flake source: a
// dragonboat NodeHost from a prior test/run releases its listener asynchronously
// on Stop(), so a fixed port can still be held when the next bind races for it.
// OS-assigned ports avoid cross-test/cross-run collisions entirely.
//
// There is a tiny window between closing the probe listener here and dragonboat
// binding the same port; on loopback this is reliable in practice and is the
// standard Go test idiom for "give me a free port".
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
