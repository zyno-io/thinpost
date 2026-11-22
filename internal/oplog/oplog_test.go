package oplog

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const (
	app = "0123456789abcdef0123456789abcdef"
	msg = "fedcba9876543210fedcba9876543210"
	del = "00112233445566778899aabbccddeeff"
)

func capture(t *testing.T) *[]string {
	var lines []string
	t.Cleanup(SetOutput(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }))
	return &lines
}

func TestFormats(t *testing.T) {
	lines := capture(t)
	Message(app, msg, StatusAccepted)
	Message(app, msg, StatusPrepared)
	PreparationFailed(app, msg, "mime_build")
	PreparationLeaseLost(app, msg)
	Delivery(app, msg, del, OutcomeRouteRejected, 550, "hosting")
	PreparedInvalid(app, msg, del)
	want := []string{
		"message app=" + app + " message=" + msg + " status=accepted",
		"message app=" + app + " message=" + msg + " status=prepared",
		"message app=" + app + " message=" + msg + " status=preparation_failed reason=mime_build",
		"message app=" + app + " message=" + msg + " status=preparation_lease_lost",
		"delivery app=" + app + " message=" + msg + " delivery=" + del + " status=route_rejected smtp=550 ip=hosting",
		"prepared app=" + app + " message=" + msg + " delivery=" + del + " status=invalid",
	}
	if strings.Join(*lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s", strings.Join(*lines, "\n"))
	}
}

func TestInvalidInputIsReplaced(t *testing.T) {
	lines := capture(t)
	for _, bad := range []string{"user@example.com", "", strings.ToUpper(app), app + "0", app[:31], "Subject: hi", strings.Repeat("g", 32)} {
		Message(bad, bad, StatusAccepted)
		Delivery(bad, bad, bad, OutcomeAccepted, 250, bad)
	}
	Message(app, msg, Status("victim@example.com"))
	Message(app, msg, StatusPreparationFailed)
	PreparationFailed(app, msg, "victim@example.com")
	Delivery(app, msg, del, Status("victim@example.com"), -5, "45.0.1.22")
	Delivery(app, msg, del, OutcomeAccepted, 1000, "")
	joined := strings.Join(*lines, "\n")
	for _, forbidden := range []string{"@", "example", "Subject", "GG", strings.ToUpper(app)} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("log contains %q:\n%s", forbidden, joined)
		}
	}
	for _, line := range *lines {
		if strings.Contains(line, "app="+app) {
			continue
		}
		if !strings.Contains(line, "app=invalid message=invalid") {
			t.Fatalf("unexpected line %q", line)
		}
	}
	for _, want := range []string{"status=other\n", "reason=other", "status=other smtp=0", "status=accepted smtp=0"} {
		if !strings.Contains(joined+"\n", want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestDefaultOutputIsStdout(t *testing.T) {
	if stdout.Writer() != os.Stdout {
		t.Fatal("lifecycle lines must be written to stdout")
	}
}

func TestSubmissionFormat(t *testing.T) {
	lines := capture(t)
	Submission("accepted", app, msg)
	Submission("rate_limited", app, msg)
	Submission("made up", "not an id", "x\ny")
	want := []string{
		"submission result=accepted app=" + app + " transfer=" + msg,
		"submission result=rate_limited app=" + app + " transfer=" + msg,
		"submission result=other app=invalid transfer=invalid",
	}
	if strings.Join(*lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", *lines)
	}
}

// A domain check line carries the app, the kind, the state and a fixed result
// code: never the domain, what DNS answered, or anything free-form.
func TestDomainCheckLine(t *testing.T) {
	lines := capture(t)
	DomainCheck(app, "bounce", "failing", "wrong_target")
	DomainCheck("bounces.customer.example", "customer.example", "bounces.customer.example", "cdn.other.example")
	want := []string{
		"domain_check app=" + app + " kind=bounce state=failing result=wrong_target",
		"domain_check app=invalid kind=other state=other result=other",
	}
	if strings.Join(*lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s", strings.Join(*lines, "\n"))
	}
}

func TestUnsubscribeForwardLine(t *testing.T) {
	lines := capture(t)
	UnsubscribeForward(app, app, app, "queued")
	UnsubscribeForward(app, app, app, "delivered")
	UnsubscribeForward("https://unsub.example.com/u?t=1", "a@example.com", app, "https://unsub.example.com/u?t=1")
	want := []string{
		"unsubscribe_forward app=" + app + " message=" + app + " delivery=" + app + " state=queued",
		"unsubscribe_forward app=" + app + " message=" + app + " delivery=" + app + " state=delivered",
		"unsubscribe_forward app=invalid message=invalid delivery=" + app + " state=other",
	}
	if strings.Join(*lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s", strings.Join(*lines, "\n"))
	}
}
