package edgesession

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounce"
)

type stubClient struct {
	auth    controlv1.SubmissionAuthVerdict
	result  SubmitResult
	err     error
	gotBody []byte
}

func (s *stubClient) Available() bool { return true }
func (s *stubClient) Verify(context.Context, SubmissionCredential, netip.Addr) (controlv1.SubmissionAuthVerdict, error) {
	return s.auth, s.err
}
func (s *stubClient) Submit(_ context.Context, m SubmissionMessage) (SubmitResult, error) {
	s.gotBody = m.Body
	return s.result, s.err
}

const adapterMessageID = "0123456789abcdef0123456789abcdef"

// Every verdict maps to the bridge's fixed reply, and nothing else reaches the
// client.
func TestSMTPSubmissionVerdictTable(t *testing.T) {
	for value, name := range controlv1.SubmissionVerdict_name {
		verdict := controlv1.SubmissionVerdict(value)
		for _, held := range []bool{false, true} {
			stub := &stubClient{result: SubmitResult{Verdict: verdict, MessageID: adapterMessageID, Held: held}}
			reply, err := smtpSubmission{c: stub}.Submit(context.Background(), bounce.SubmissionMessage{Body: []byte("x")})
			if err != nil {
				t.Fatal(err)
			}
			want := ReplyFor(stub.result)
			if reply != (bounce.Reply{Code: want.Code, Enhanced: want.Enhanced, Text: want.Text, Close: want.Close}) {
				t.Fatalf("%s: %+v != %+v", name, reply, want)
			}
			if verdict == controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED && reply.Code != 451 {
				t.Fatalf("UNSPECIFIED must be a tempfail, got %d", reply.Code)
			}
		}
	}
	held, _ := smtpSubmission{c: &stubClient{result: SubmitResult{Verdict: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, MessageID: adapterMessageID, Held: true}}}.Submit(context.Background(), bounce.SubmissionMessage{})
	if held.Code != 250 || held.Enhanced != "2.0.0" || held.Text != "OK: held as "+adapterMessageID {
		t.Fatalf("held reply: %+v", held)
	}
	// An accepted result without a valid message ID is never a 250.
	bad, _ := smtpSubmission{c: &stubClient{result: SubmitResult{Verdict: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, MessageID: "short"}}}.Submit(context.Background(), bounce.SubmissionMessage{})
	if bad.Code != 451 {
		t.Fatalf("accepted without an ID: %+v", bad)
	}
	if _, err := (smtpSubmission{c: &stubClient{err: errors.New("down")}}).Submit(context.Background(), bounce.SubmissionMessage{}); err == nil {
		t.Fatal("a bridge error must reach the session as an error")
	}
}

func TestSMTPSubmissionAuthTable(t *testing.T) {
	cases := map[controlv1.SubmissionAuthVerdict]struct {
		ok, invalid bool
		code        int
		closed      bool
	}{
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK:          {ok: true, code: 235},
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID:     {invalid: true, code: 535},
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL:    {code: 454},
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_THROTTLED:   {code: 454},
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNAVAILABLE: {code: 421, closed: true},
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNSPECIFIED: {code: 454},
		controlv1.SubmissionAuthVerdict(99):                                 {code: 454},
	}
	for verdict, want := range cases {
		result, err := smtpSubmission{c: &stubClient{auth: verdict}}.Verify(context.Background(), bounce.Credential{}, netip.Addr{})
		if err != nil {
			t.Fatal(err)
		}
		if result.OK != want.ok || result.Invalid != want.invalid || result.Reply.Code != want.code || result.Reply.Close != want.closed {
			t.Fatalf("%v: %+v", verdict, result)
		}
	}
	if _, err := (smtpSubmission{c: &stubClient{err: errors.New("down")}}).Verify(context.Background(), bounce.Credential{}, netip.Addr{}); err == nil {
		t.Fatal("a bridge error must reach the session as an error")
	}
}
