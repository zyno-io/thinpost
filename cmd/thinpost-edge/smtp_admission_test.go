package main

import (
	"net"
	"testing"
	"time"
)

// limitSMTPConnections wraps one listener in its own admission.
func limitSMTPConnections(listener net.Listener, total, perSource int, stats *smtpAdmissionStats) (net.Listener, error) {
	admission, err := newSMTPAdmission(total, perSource, stats)
	if err != nil {
		return nil, err
	}
	return admission.Wrap(listener), nil
}

// Two listeners share one admission: the total and the per-source limits hold
// across them, and a source cannot double its allowance with a second address.
func TestSMTPAdmissionLimitsHoldAcrossListeners(t *testing.T) {
	first := &testListener{connections: make(chan net.Conn, 8), closed: make(chan struct{})}
	second := &testListener{connections: make(chan net.Conn, 8), closed: make(chan struct{})}
	admission, err := newSMTPAdmission(3, 1, &smtpAdmissionStats{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := admission.Wrap(first), admission.Wrap(second)
	defer a.Close()
	defer b.Close()

	c1 := queueConn(first, "192.0.2.1")
	defer c1.Close()
	conn1, err := a.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// The same source on the other listener is over the per-source limit and is
	// dropped; a different source is admitted.
	dup := queueConn(second, "192.0.2.1")
	defer dup.Close()
	c2 := queueConn(second, "192.0.2.2")
	defer c2.Close()
	conn2, err := b.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if got := conn2.RemoteAddr().(*net.TCPAddr).IP.String(); got != "192.0.2.2" {
		t.Fatalf("admitted %s, want the second source", got)
	}
	c3 := queueConn(first, "192.0.2.3")
	defer c3.Close()
	conn3, err := a.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// The three-connection total is now used up across both listeners.
	next := make(chan struct{}, 1)
	c4 := queueConn(second, "192.0.2.4")
	defer c4.Close()
	go func() {
		if conn, acceptErr := b.Accept(); acceptErr == nil {
			defer conn.Close()
			next <- struct{}{}
		}
	}()
	select {
	case <-next:
		t.Fatal("accepted beyond the total across listeners")
	case <-time.After(30 * time.Millisecond):
	}
	_ = conn1.Close()
	select {
	case <-next:
	case <-time.After(2 * time.Second):
		t.Fatal("slot was not released across listeners")
	}
	_ = conn2.Close()
	_ = conn3.Close()
}
