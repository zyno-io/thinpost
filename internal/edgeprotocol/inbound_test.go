package edgeprotocol

import (
	"crypto/sha256"
	"strings"
	"testing"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

const inboundTestID = "0123456789abcdef0123456789abcdef"

func inboundStart(body []byte) *controlv1.InboundStart {
	digest := sha256.Sum256(body)
	return &controlv1.InboundStart{InboundId: inboundTestID, ExpectedSize: uint64(len(body)), Sha256: digest[:], MailFrom: "a@example.org", RcptTo: []string{"u@example.com"}, ClientIp: "192.0.2.1"}
}

func TestInboundAssemblerAssemblesAndVerifies(t *testing.T) {
	body := []byte(strings.Repeat("x", MaxChunkBytes+10))
	a, err := NewInboundAssembler(inboundStart(body))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := a.Add(&controlv1.DataChunk{Id: inboundTestID, Data: body[:MaxChunkBytes]}); err != nil || out != nil {
		t.Fatal("first chunk", err)
	}
	out, err := a.Add(&controlv1.DataChunk{Id: inboundTestID, Offset: MaxChunkBytes, Data: body[MaxChunkBytes:], Last: true})
	if err != nil || string(out) != string(body) {
		t.Fatalf("final chunk: %v", err)
	}
}

func TestInboundAssemblerRejects(t *testing.T) {
	body := []byte("hello world")
	for name, run := range map[string]func(a *InboundAssembler) error{
		"wrong id": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: "x", Data: body, Last: true})
			return e
		},
		"gap": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: inboundTestID, Offset: 1, Data: body})
			return e
		},
		"early last": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: inboundTestID, Data: body[:3], Last: true})
			return e
		},
		"missing last": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: inboundTestID, Data: body})
			return e
		},
		"too much": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: inboundTestID, Data: append(body, 'x'), Last: true})
			return e
		},
		"empty": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: inboundTestID, Last: true})
			return e
		},
		"digest": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: inboundTestID, Data: []byte("hello worlD"), Last: true})
			return e
		},
		"chunk over bound": func(a *InboundAssembler) error {
			_, e := a.Add(&controlv1.DataChunk{Id: inboundTestID, Data: make([]byte, MaxChunkBytes+1)})
			return e
		},
		"nil chunk": func(a *InboundAssembler) error { _, e := a.Add(nil); return e },
	} {
		a, err := NewInboundAssembler(inboundStart(body))
		if err != nil {
			t.Fatal(err)
		}
		if run(a) == nil {
			t.Errorf("%s accepted", name)
		}
		// A failed assembler is dead.
		if _, err = a.Add(&controlv1.DataChunk{Id: inboundTestID, Data: body, Last: true}); err == nil {
			t.Errorf("%s: assembler usable after an error", name)
		}
	}
}

func TestInboundStartValidation(t *testing.T) {
	good := inboundStart([]byte("x"))
	if !ValidInboundStart(good) {
		t.Fatal("valid start refused")
	}
	for name, mutate := range map[string]func(*controlv1.InboundStart){
		"id":           func(s *controlv1.InboundStart) { s.InboundId = "ABCDEF" },
		"zero size":    func(s *controlv1.InboundStart) { s.ExpectedSize = 0 },
		"huge size":    func(s *controlv1.InboundStart) { s.ExpectedSize = MaxInboundAssembleBytes + 1 },
		"digest":       func(s *controlv1.InboundStart) { s.Sha256 = []byte("short") },
		"no recipient": func(s *controlv1.InboundStart) { s.RcptTo = nil },
		"51 recipient": func(s *controlv1.InboundStart) {
			s.RcptTo = make([]string, 51)
			for i := range s.RcptTo {
				s.RcptTo[i] = "a@b.c"
			}
		},
		"long rcpt":   func(s *controlv1.InboundStart) { s.RcptTo = []string{strings.Repeat("a", 321)} },
		"long from":   func(s *controlv1.InboundStart) { s.MailFrom = strings.Repeat("a", 321) },
		"bad ip":      func(s *controlv1.InboundStart) { s.ClientIp = "not-an-ip" },
		"negative ms": func(s *controlv1.InboundStart) { s.ReceivedUnixMs = -1 },
	} {
		s := inboundStart([]byte("x"))
		mutate(s)
		if ValidInboundStart(s) {
			t.Errorf("%s accepted", name)
		}
	}
	if ValidInboundStart(nil) {
		t.Fatal("nil start accepted")
	}
}

func TestCapabilities(t *testing.T) {
	if !ValidCapabilities([]string{CapInbound, "future/2"}) || !ValidCapabilities(nil) {
		t.Fatal("valid capabilities refused")
	}
	if ValidCapabilities([]string{""}) || ValidCapabilities([]string{"Has Space"}) || ValidCapabilities(make([]string, MaxCapabilities+1)) || ValidCapabilities([]string{strings.Repeat("a", 33)}) {
		t.Fatal("malformed capabilities accepted")
	}
	if !HasCapability([]string{"x", CapInbound}, CapInbound) || HasCapability(nil, CapInbound) {
		t.Fatal("HasCapability")
	}
}

func rt(domain string, group uint32, state controlv1.InboundRouteState) *controlv1.InboundRoute {
	return &controlv1.InboundRoute{Domain: domain, Group: group, State: state}
}

const (
	stAccept   = controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT
	stTempfail = controlv1.InboundRouteState_INBOUND_ROUTE_STATE_TEMPFAIL
)

func TestRoutesAssemblerPaging(t *testing.T) {
	var a RoutesAssembler
	if _, done, err := a.Add(&controlv1.InboundRoutes{Offset: 0, Routes: []*controlv1.InboundRoute{rt("a.example", 1, stAccept)}}); err != nil || done {
		t.Fatal("first page", err)
	}
	// A new offset-zero page restarts the snapshot.
	if _, done, err := a.Add(&controlv1.InboundRoutes{Offset: 0, Routes: []*controlv1.InboundRoute{rt("b.example", 1, stAccept)}}); err != nil || done {
		t.Fatal("restart", err)
	}
	routes, done, err := a.Add(&controlv1.InboundRoutes{Offset: 1, Last: true, Routes: []*controlv1.InboundRoute{rt("c.example", 2, stTempfail)}})
	if err != nil || !done || len(routes) != 2 || routes[0].Domain != "b.example" {
		t.Fatalf("complete: %v %v %v", routes, done, err)
	}
	// An empty snapshot is valid and clears every route.
	routes, done, err = a.Add(&controlv1.InboundRoutes{Last: true})
	if err != nil || !done || len(routes) != 0 {
		t.Fatalf("empty: %v %v %v", routes, done, err)
	}
}

func TestRoutesAssemblerRejects(t *testing.T) {
	good := rt("a.example", 1, stAccept)
	for name, pages := range map[string][]*controlv1.InboundRoutes{
		"continuation without start": {{Offset: 1, Routes: []*controlv1.InboundRoute{good}}},
		"gap":                        {{Routes: []*controlv1.InboundRoute{good}}, {Offset: 2, Last: true, Routes: []*controlv1.InboundRoute{rt("b.example", 1, stAccept)}}},
		"duplicate in page":          {{Last: true, Routes: []*controlv1.InboundRoute{good, good}}},
		"duplicate across pages":     {{Routes: []*controlv1.InboundRoute{good}}, {Offset: 1, Last: true, Routes: []*controlv1.InboundRoute{good}}},
		"uppercase":                  {{Last: true, Routes: []*controlv1.InboundRoute{rt("A.example", 1, stAccept)}}},
		"no group":                   {{Last: true, Routes: []*controlv1.InboundRoute{rt("a.example", 0, stAccept)}}},
		"unknown state":              {{Last: true, Routes: []*controlv1.InboundRoute{rt("a.example", 1, controlv1.InboundRouteState_INBOUND_ROUTE_STATE_UNSPECIFIED)}}},
		"bad name":                   {{Last: true, Routes: []*controlv1.InboundRoute{rt("a..example", 1, stAccept)}}},
		"nil page":                   {nil},
	} {
		var a RoutesAssembler
		failed := false
		for _, page := range pages {
			if _, _, err := a.Add(page); err != nil {
				failed = true
				break
			}
		}
		if !failed {
			t.Errorf("%s accepted", name)
		}
	}
}

// A bad envelope is not a framing violation: the assembler still collects the
// message so control can answer it, and ValidInboundStart flags it.
func TestEnvelopeFaultsAreNotFramingFaults(t *testing.T) {
	s := inboundStart([]byte("x"))
	s.MailFrom = strings.Repeat("a", 411)
	s.Helo = strings.Repeat("h", 601)
	if ValidInboundStart(s) || !ValidInboundFraming(s) {
		t.Fatal("classification")
	}
	if _, err := NewInboundAssembler(s); err != nil {
		t.Fatal(err)
	}
	s.Sha256 = nil
	if ValidInboundFraming(s) {
		t.Fatal("a frame with no digest is a framing fault")
	}
}
