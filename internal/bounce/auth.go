package bounce

import (
	"context"
	"errors"

	"github.com/emersion/go-sasl"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	smtp "github.com/zyno-io/thinpost/internal/thirdparty/gosmtp"
)

var _ smtp.AuthSession = (*submissionSession)(nil)

// AuthMechanisms is consulted only once TLS is up (go-smtp advertises AUTH only
// when authAllowed), so AUTH is offered after STARTTLS on 587 and the internal
// listener and at once on 465.
func (s *submissionSession) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }

// Auth runs for any AUTH command once TLS is up, whatever was advertised and
// even mid-transaction, so it enforces the order itself.
func (s *submissionSession) Auth(mech string) (sasl.Server, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, tooManyErrors()
	}
	if s.txn != nil {
		s.mu.Unlock()
		return nil, &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "AUTH not permitted during a mail transaction"}
	}
	s.authAttempts++
	tooMany := s.authAttempts > maxAuthAttempts
	s.mu.Unlock()
	if tooMany {
		s.count("auth_attempts_exceeded")
		return nil, abuseClose(s)
	}
	if limiter := s.backend.cfg.AuthLimiter; limiter != nil {
		switch limiter.Check(s.clientIP()) {
		case AuthGateBlocked:
			s.count("auth_blocked")
			return nil, abuseClose(s)
		case AuthGateBusy:
			s.count("auth_busy")
			return nil, authTempfail()
		}
	}
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && identity != username {
				return s.authInvalid()
			}
			return s.authenticate(username, password)
		}), nil
	case sasl.Login:
		return &loginServer{authenticate: s.authenticate}, nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

func authTempfail() error {
	return &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Temporary authentication failure"}
}

// authInvalid counts a failure against the client address and answers 535.
func (s *submissionSession) authInvalid() error {
	s.count("auth_invalid")
	if limiter := s.backend.cfg.AuthLimiter; limiter != nil {
		limiter.Failed(s.clientIP())
	}
	return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "Authentication credentials invalid"}
}

// authenticate checks the syntax before any control call, then asks control.
func (s *submissionSession) authenticate(username, password string) error {
	if !edgeprotocol.ValidSubmissionID(username) || !edgeprotocol.ValidSubmissionSecret(password) {
		return s.authInvalid()
	}
	submission := s.backend.cfg.Submission
	if !submission.Available() {
		s.count("auth_no_session")
		return authTempfail()
	}
	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	defer cancel()
	cred := Credential{ID: username, Secret: password}
	result, err := submission.Verify(ctx, cred, s.clientIP())
	switch {
	case err != nil:
		s.count("auth_tempfail")
		return authTempfail()
	case result.OK:
		s.mu.Lock()
		s.auth = &cred
		s.mu.Unlock()
		if s.clamp != nil {
			s.clamp.authenticate()
		}
		s.count("auth_ok")
		return nil
	case result.Invalid:
		return s.authInvalid()
	case result.Reply.Close:
		s.count("auth_unavailable")
		s.closeConnection()
		return result.Reply.err()
	}
	s.count("auth_tempfail")
	return authTempfail()
}

// loginServer implements the LOGIN mechanism (go-sasl ships only a client).
type loginServer struct {
	state        int
	username     string
	authenticate func(username, password string) error
}

var errLoginDone = errors.New("authentication already finished")

func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch l.state {
	case 0:
		if response == nil {
			l.state = 1
			return []byte("Username:"), false, nil
		}
		l.username = string(response)
		l.state = 2
		return []byte("Password:"), false, nil
	case 1:
		l.username = string(response)
		l.state = 2
		return []byte("Password:"), false, nil
	case 2:
		l.state = 3
		if err = l.authenticate(l.username, string(response)); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	return nil, false, errLoginDone
}
