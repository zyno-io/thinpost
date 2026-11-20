package edgesession

import (
	"context"
	"net/netip"

	"github.com/zyno-io/thinpost/internal/bounce"
)

// SMTPSubmission adapts the submission bridge to the SMTP submission
// listeners. Verdicts become fixed replies through ReplyFor and AuthReply, so
// control never sends free text, and an accepted result without a valid
// message ID is a 451, never a 250.
func (s *Server) SMTPSubmission() bounce.Submission { return smtpSubmission{c: &s.submission} }

type smtpSubmission struct{ c SubmissionClient }

func smtpReply(r SMTPReply) bounce.Reply {
	return bounce.Reply{Code: r.Code, Enhanced: r.Enhanced, Text: r.Text, Close: r.Close}
}

func (a smtpSubmission) Available() bool { return a.c.Available() }

func (a smtpSubmission) Verify(ctx context.Context, cred bounce.Credential, ip netip.Addr) (bounce.AuthResult, error) {
	verdict, err := a.c.Verify(ctx, SubmissionCredential{ID: cred.ID, Secret: cred.Secret}, ip)
	if err != nil {
		return bounce.AuthResult{}, err
	}
	reply := AuthReply(verdict)
	return bounce.AuthResult{OK: reply.Code == 235, Invalid: reply.Code == 535, Reply: smtpReply(reply)}, nil
}

func (a smtpSubmission) Submit(ctx context.Context, m bounce.SubmissionMessage) (bounce.Reply, error) {
	result, err := a.c.Submit(ctx, SubmissionMessage{Credential: SubmissionCredential{ID: m.Credential.ID, Secret: m.Credential.Secret}, MailFrom: m.MailFrom,
		Rcpts: m.Rcpts, ClientIP: m.ClientIP, TLSVersion: m.TLSVersion, Received: m.Received, Port: m.Port, Body: m.Body})
	if err != nil {
		return bounce.Reply{}, err
	}
	return smtpReply(ReplyFor(result)), nil
}
