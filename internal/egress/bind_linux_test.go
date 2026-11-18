//go:build linux

package egress

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// Linux assigns all of 127.0.0.0/8 to the loopback interface, so a connection
// bound to 127.0.0.2 shows that address to the peer. Other platforms do not.
func TestDialTCPBindsRequestedSource(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("loopback listener unavailable")
	}
	defer listener.Close()
	peer := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			peer <- ""
			return
		}
		defer conn.Close()
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		peer <- host
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialTCP(ctx, netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1"), port)
	if err != nil {
		t.Skip("127.0.0.2 cannot be bound on this host")
	}
	defer conn.Close()
	if got := <-peer; got != "127.0.0.2" {
		t.Fatalf("peer saw source %q, want 127.0.0.2", got)
	}
	// An address the host does not own fails the dial; nothing falls back.
	if c, err := dialTCP(ctx, netip.MustParseAddr("192.0.2.77"), netip.MustParseAddr("127.0.0.1"), port); err == nil {
		c.Close()
		t.Fatal("dial succeeded from an address the host does not own")
	}
}
