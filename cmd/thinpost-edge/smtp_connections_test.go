package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type testListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *testListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *testListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *testListener) Addr() net.Addr { return &net.TCPAddr{} }

type remoteConn struct {
	net.Conn
	addr net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.addr }
func queueConn(l *testListener, ip string) net.Conn {
	server, client := net.Pipe()
	l.connections <- remoteConn{Conn: server, addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 1234}}
	return client
}
func TestSMTPConnectionLimitIsPerSourceAndReleasesOnClose(t *testing.T) {
	raw := &testListener{connections: make(chan net.Conn, 8), closed: make(chan struct{})}
	stats := &smtpAdmissionStats{}
	listener, err := limitSMTPConnections(raw, 4, 1, stats)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	firstClient := queueConn(raw, "192.0.2.1")
	defer firstClient.Close()
	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// IPv4-mapped addresses must share the same limit as their IPv4 form.
	rejected := queueConn(raw, "::ffff:192.0.2.1")
	defer rejected.Close()
	otherClient := queueConn(raw, "192.0.2.2")
	defer otherClient.Close()
	other, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = rejected.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal("excess source connection was not closed", err)
	}
	if stats.concurrentLimit.Load() != 1 {
		t.Fatal("source connection rejection was not counted")
	}
	if stats.activeConnections.Load() != 2 {
		t.Fatal("rejected connection retained an occupancy slot")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = first.Close() }()
	}
	wg.Wait()
	nextClient := queueConn(raw, "192.0.2.1")
	defer nextClient.Close()
	next, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
}
func TestSMTPConnectionLimitRejectsInvalidConfiguration(t *testing.T) {
	raw := &testListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	defer raw.Close()
	for _, limits := range [][2]int{{0, 1}, {4, 0}, {1, 2}} {
		if _, err := limitSMTPConnections(raw, limits[0], limits[1], nil); err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
}

func TestSMTPConnectionLimitOnTCPListener(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := limitSMTPConnections(raw, 4, 1, nil)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	defer listener.Close()
	firstClient, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer firstClient.Close()
	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	secondClient, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer secondClient.Close()
	type result struct {
		conn net.Conn
		err  error
	}
	next := make(chan result, 1)
	go func() { conn, err := listener.Accept(); next <- result{conn, err} }()
	if err = secondClient.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = secondClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("excess TCP connection remained open")
	} else if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
		t.Fatal("excess TCP connection was not closed")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	thirdClient, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer thirdClient.Close()
	select {
	case accepted := <-next:
		if accepted.err != nil {
			t.Fatal(accepted.err)
		}
		_ = accepted.conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("source slot was not released")
	}
}

func TestSMTPConnectionOccupancyReportsSaturationAndRelease(t *testing.T) {
	raw := &testListener{connections: make(chan net.Conn, 4), closed: make(chan struct{})}
	stats := &smtpAdmissionStats{}
	listener, err := limitSMTPConnections(raw, 2, 2, stats)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client1 := queueConn(raw, "192.0.2.1")
	defer client1.Close()
	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	client2 := queueConn(raw, "192.0.2.2")
	defer client2.Close()
	second, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if stats.activeConnections.Load() != 2 || stats.connectionLimit.Load() != 2 || stats.saturationEntries.Load() != 1 {
		t.Fatal("did not observe the full connection limit")
	}
	client3 := queueConn(raw, "192.0.2.3")
	defer client3.Close()
	next := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			next <- conn
		}
	}()
	select {
	case conn := <-next:
		_ = conn.Close()
		t.Fatal("accepted beyond the global limit")
	case <-time.After(30 * time.Millisecond):
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case third := <-next:
		if stats.activeConnections.Load() != 2 || stats.saturationEntries.Load() != 2 {
			t.Fatal("did not observe re-entry into saturation")
		}
		_ = third.Close()
		_ = third.Close()
	case <-time.After(time.Second):
		t.Fatal("connection slot did not reopen")
	}
	_ = second.Close()
	if stats.activeConnections.Load() != 0 {
		t.Fatal("connection count did not return to zero")
	}
}
