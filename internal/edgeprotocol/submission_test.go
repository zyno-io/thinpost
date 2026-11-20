package edgeprotocol

import (
	"crypto/sha256"
	"strings"
	"testing"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

const (
	subID     = "0123456789abcdef0123456789abcdef"
	subSecret = "tps_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

func submissionStart(body []byte) *controlv1.SubmissionStart {
	digest := sha256.Sum256(body)
	return &controlv1.SubmissionStart{SubmissionId: subID, ExpectedSize: uint64(len(body)), Sha256: digest[:], CredentialId: subID, Secret: subSecret, MailFrom: "a@example.org", RcptTo: []string{"u@example.com"}, ClientIp: "192.0.2.1", Port: 587}
}

func TestSubmissionConstants(t *testing.T) {
	if len(subSecret) != 47 || !ValidSubmissionSecret(subSecret) {
		t.Fatal("test secret")
	}
	if MaxSubmissionBytes != 7864320 || SubmissionWeight(10) != 30+192<<10 {
		t.Fatal("constants")
	}
}

func TestValidSubmissionStartBounds(t *testing.T) {
	body := []byte("hello")
	if !ValidSubmissionStart(submissionStart(body)) {
		t.Fatal("valid start rejected")
	}
	mutations := map[string]func(*controlv1.SubmissionStart){
		"id":         func(s *controlv1.SubmissionStart) { s.SubmissionId = strings.ToUpper(subID) },
		"short id":   func(s *controlv1.SubmissionStart) { s.SubmissionId = "abc" },
		"zero size":  func(s *controlv1.SubmissionStart) { s.ExpectedSize = 0 },
		"big size":   func(s *controlv1.SubmissionStart) { s.ExpectedSize = MaxSubmissionBytes + 1 },
		"sha":        func(s *controlv1.SubmissionStart) { s.Sha256 = []byte{1} },
		"cred id":    func(s *controlv1.SubmissionStart) { s.CredentialId = "x" },
		"secret":     func(s *controlv1.SubmissionStart) { s.Secret = "tps_short" },
		"no rcpt":    func(s *controlv1.SubmissionStart) { s.RcptTo = nil },
		"many rcpt":  func(s *controlv1.SubmissionStart) { s.RcptTo = make([]string, MaxSubmissionRecipients+1) },
		"empty rcpt": func(s *controlv1.SubmissionStart) { s.RcptTo = []string{""} },
		"long from":  func(s *controlv1.SubmissionStart) { s.MailFrom = strings.Repeat("a", 321) },
		"ctl from":   func(s *controlv1.SubmissionStart) { s.MailFrom = "a\r\nb" },
		"ip":         func(s *controlv1.SubmissionStart) { s.ClientIp = "nope" },
		"tls":        func(s *controlv1.SubmissionStart) { s.TlsVersion = strings.Repeat("a", 65) },
		"port":       func(s *controlv1.SubmissionStart) { s.Port = 25 },
		"tls none":   func(s *controlv1.SubmissionStart) { s.TlsVersion = "none" },
		"time":       func(s *controlv1.SubmissionStart) { s.ReceivedUnixMs = -1 },
	}
	for name, mutate := range mutations {
		s := submissionStart(body)
		mutate(s)
		if ValidSubmissionStart(s) {
			t.Errorf("%s accepted", name)
		}
	}
	// Max-size recipients and null path are fine.
	s := submissionStart(body)
	s.MailFrom, s.ClientIp, s.Port, s.TlsVersion = "", "", 0, ""
	if !ValidSubmissionStart(func() *controlv1.SubmissionStart { c := submissionStart(body); c.TlsVersion = "TLS 1.3"; return c }()) {
		t.Fatal("TLS 1.3 rejected")
	}
	s.RcptTo = make([]string, MaxSubmissionRecipients)
	for i := range s.RcptTo {
		s.RcptTo[i] = "u@example.com"
	}
	if !ValidSubmissionStart(s) {
		t.Fatal("null path, 100 recipients and internal port rejected")
	}
}

func TestSubmissionStartVerdictSeparatesFramingFromEnvelope(t *testing.T) {
	body := []byte("x")
	if SubmissionStartVerdict(submissionStart(body)) != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED {
		t.Fatal("valid")
	}
	s := submissionStart(body)
	s.Secret = "bad"
	if SubmissionStartVerdict(s) != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID {
		t.Fatal("credential")
	}
	s = submissionStart(body)
	s.RcptTo = nil
	if SubmissionStartVerdict(s) != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID || !ValidSubmissionFraming(s) {
		t.Fatal("envelope only fails the envelope check")
	}
}

func TestValidSubmissionAuth(t *testing.T) {
	ok := &controlv1.SubmissionAuth{RequestId: subID, CredentialId: subID, Secret: subSecret}
	if !ValidSubmissionAuth(ok) || ValidSubmissionAuth(nil) {
		t.Fatal("basic")
	}
	for _, bad := range []*controlv1.SubmissionAuth{
		{RequestId: "x", CredentialId: subID, Secret: subSecret},
		{RequestId: subID, CredentialId: "x", Secret: subSecret},
		{RequestId: subID, CredentialId: subID, Secret: "x"},
		{RequestId: subID, CredentialId: subID, Secret: subSecret, ClientIp: "zz"},
	} {
		if ValidSubmissionAuth(bad) {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestValidSubmissionPorts(t *testing.T) {
	if !ValidSubmissionPorts(nil) || !ValidSubmissionPorts([]uint32{465, 587}) {
		t.Fatal("valid")
	}
	for _, bad := range [][]uint32{{0}, {65536}, {587, 587}, {1, 2, 3, 4, 5, 6, 7, 8, 9}} {
		if ValidSubmissionPorts(bad) {
			t.Fatalf("accepted %v", bad)
		}
	}
	if !ValidSubmissionPorts([]uint32{1, 2, 3, 4, 5, 6, 7, 65535}) {
		t.Fatal("eight ports")
	}
}

func TestChunkAssemblerAllocatesLazily(t *testing.T) {
	body := []byte(strings.Repeat("x", MaxChunkBytes+10))
	digest := sha256.Sum256(body)
	a, err := NewChunkAssembler(subID, uint64(len(body)), digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if a.Allocated() != 0 {
		t.Fatal("allocated before the first chunk")
	}
	if out, err := a.Add(&controlv1.DataChunk{Id: subID, Data: body[:MaxChunkBytes]}); err != nil || out != nil {
		t.Fatal("first chunk", err)
	}
	if a.Allocated() != len(body) {
		t.Fatalf("allocated %d want %d", a.Allocated(), len(body))
	}
	out, err := a.Add(&controlv1.DataChunk{Id: subID, Offset: MaxChunkBytes, Data: body[MaxChunkBytes:], Last: true})
	if err != nil || string(out) != string(body) {
		t.Fatalf("final chunk: %v", err)
	}
	if _, err = a.Add(&controlv1.DataChunk{Id: subID, Offset: uint64(len(body)), Data: []byte("x")}); err == nil {
		t.Fatal("accepted a chunk after completion")
	}
}

func TestChunkAssemblerRejects(t *testing.T) {
	body := []byte("hello world")
	digest := sha256.Sum256(body)
	cases := map[string]*controlv1.DataChunk{
		"wrong id":   {Id: "x", Data: body, Last: true},
		"gap":        {Id: subID, Offset: 1, Data: body},
		"early last": {Id: subID, Data: body[:3], Last: true},
		"no last":    {Id: subID, Data: body},
		"too much":   {Id: subID, Data: append(append([]byte{}, body...), 'x'), Last: true},
		"empty":      {Id: subID, Last: true},
		"oversize":   {Id: subID, Data: make([]byte, MaxChunkBytes+1)},
	}
	for name, chunk := range cases {
		a, _ := NewChunkAssembler(subID, uint64(len(body)), digest[:])
		if _, err := a.Add(chunk); err == nil {
			t.Errorf("%s accepted", name)
		}
		if a.Allocated() != 0 && name != "no last" && name != "early last" {
			t.Errorf("%s allocated %d", name, a.Allocated())
		}
	}
	a, _ := NewChunkAssembler(subID, uint64(len(body)), digest[:])
	if _, err := a.Add(&controlv1.DataChunk{Id: subID, Data: []byte("HELLO WORLD"), Last: true}); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	if _, err := NewChunkAssembler("", 1, digest[:]); err == nil {
		t.Fatal("empty id")
	}
	if _, err := NewChunkAssembler(subID, 0, digest[:]); err == nil {
		t.Fatal("zero size")
	}
	if _, err := NewChunkAssembler(subID, 1, nil); err == nil {
		t.Fatal("no digest")
	}
}
