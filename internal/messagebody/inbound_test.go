package messagebody

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

type inboundRepo struct {
	err       error
	protected bool
	audited   int
	actions   []string
}

func (r *inboundRepo) AuthorizeInboundRead(_ context.Context, actor, app, id, action string) (string, bool, format.InboundInput, error) {
	if r.err != nil {
		return "", false, format.InboundInput{}, r.err
	}
	if r.protected {
		r.audited++
	}
	r.actions = append(r.actions, action)
	return "inbound/key", r.protected, format.InboundInput{ID: "11112222333344445555666677778888", Recipient: "r@mail.example.com", Receiver: "smtp.example.com", SPF: "pass", DKIM: "none", DMARC: "pass"}, nil
}

type inboundObjectsFake struct {
	repo *inboundRepo
	data []byte
	err  error
	gets int
}

func (o *inboundObjectsFake) GetInbound(context.Context, string, int64) ([]byte, error) {
	o.gets++
	if o.repo.protected && o.repo.audited == 0 {
		return nil, errors.New("fetched before the audit")
	}
	return o.data, o.err
}

const inboundMessage = "From: =?utf-8?q?Jos=C3=A9?= <jose@sender.example>\r\nTo: r@mail.example.com\r\nSubject: Hello\r\n\r\nthe body\r\n"

func TestInboundContentUsesTheRendererAndAuditsBeforeFetching(t *testing.T) {
	repo := &inboundRepo{protected: true}
	objects := &inboundObjectsFake{repo: repo, data: []byte(inboundMessage)}
	service := NewInboundService(repo, objects)
	body, err := service.ReadInboundContent(context.Background(), "operator", "app", "id")
	if err != nil || body.From != "José <jose@sender.example>" || len(body.To) != 1 || body.Subject != "Hello" || strings.TrimSpace(body.Text) != "the body" || body.HTML != "" ||
		body.Attachments == nil || len(body.Attachments) != 0 || len(body.Omitted) != 0 || !body.EnhancedProtection {
		t.Fatalf("%+v %v", body, err)
	}
	if repo.audited != 1 || repo.actions[0] != InboundBodyReadAction {
		t.Fatal("EP read not audited once", repo.audited, repo.actions)
	}
	file, err := service.OpenInboundRaw(context.Background(), "operator", "app", "id")
	if err != nil || repo.audited != 2 || repo.actions[1] != InboundRawReadAction {
		t.Fatal("raw", err, repo.actions)
	}
	var out bytes.Buffer
	n, err := file.WriteTo(&out)
	if err != nil || n != file.Size() || !strings.HasPrefix(out.String(), "Authentication-Results: smtp.example.com;") || !strings.HasSuffix(out.String(), inboundMessage) {
		t.Fatal("raw bytes", n, err, out.String())
	}
	if len(service.slots) != 0 {
		t.Fatal("a read slot leaked")
	}
}

func TestInboundReadsRefuseWithoutFetching(t *testing.T) {
	for _, cause := range []error{admin.ErrContentExpired, admin.ErrNotFound} {
		repo := &inboundRepo{err: cause}
		objects := &inboundObjectsFake{repo: repo}
		service := NewInboundService(repo, objects)
		if _, err := service.ReadInboundContent(context.Background(), "operator", "app", "id"); !errors.Is(err, cause) {
			t.Fatal("content", err)
		}
		if _, err := service.OpenInboundRaw(context.Background(), "operator", "app", "id"); !errors.Is(err, cause) {
			t.Fatal("raw", err)
		}
		if objects.gets != 0 || len(service.slots) != 0 {
			t.Fatal("fetched or leaked a slot", objects.gets)
		}
	}
	// An object the lifecycle rule already removed comes back as expired content.
	repo := &inboundRepo{}
	service := NewInboundService(repo, &inboundObjectsFake{repo: repo, err: admin.ErrContentExpired})
	if _, err := service.ReadInboundContent(context.Background(), "operator", "app", "id"); !errors.Is(err, admin.ErrContentExpired) {
		t.Fatal("missing object", err)
	}
	if _, err := service.OpenInboundRaw(context.Background(), "operator", "app", "id"); !errors.Is(err, admin.ErrContentExpired) || len(service.slots) != 0 {
		t.Fatal("missing object (raw) or slot leaked", err)
	}
	if _, err := (*InboundService)(nil).ReadInboundContent(context.Background(), "o", "a", "i"); err == nil {
		t.Fatal("unconfigured service answered")
	}
}

func TestInboundReadsAreBoundedInConcurrency(t *testing.T) {
	repo := &inboundRepo{}
	service := NewInboundService(repo, &inboundObjectsFake{repo: repo, data: []byte(inboundMessage)})
	var held []admin.InboundRaw
	for range maxInboundReads {
		file, err := service.OpenInboundRaw(context.Background(), "o", "a", "i")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, file)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.OpenInboundRaw(ctx, "o", "a", "i"); err == nil {
		t.Fatal("a fifth read ran while four were open")
	}
	for _, file := range held {
		_, _ = file.WriteTo(&bytes.Buffer{})
	}
	if file, err := service.OpenInboundRaw(context.Background(), "o", "a", "i"); err != nil {
		t.Fatal("slots were not released", err)
	} else {
		_, _ = file.WriteTo(&bytes.Buffer{})
	}
}

func TestInboundContentReportsEnhancedProtectionOnlyWhenProtected(t *testing.T) {
	repo := &inboundRepo{}
	service := NewInboundService(repo, &inboundObjectsFake{repo: repo, data: []byte(inboundMessage)})
	body, err := service.ReadInboundContent(context.Background(), "operator", "app", "id")
	if err != nil || body.EnhancedProtection {
		t.Fatal("an unprotected message was reported as protected", err)
	}
}

type panickingObjects struct{}

func (panickingObjects) GetInbound(context.Context, string, int64) ([]byte, error) {
	panic("storage exploded")
}

func TestOpenInboundRawReleasesItsSlotOnPanic(t *testing.T) {
	repo := &inboundRepo{}
	service := NewInboundService(repo, panickingObjects{})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the panic to propagate")
			}
		}()
		_, _ = service.OpenInboundRaw(context.Background(), "o", "a", "i")
	}()
	if len(service.slots) != 0 {
		t.Fatal("a panic leaked a read slot")
	}
}

func TestMissingObjectBeforeRetentionIsLoggedByIDOnly(t *testing.T) {
	var out bytes.Buffer
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)
	repo := &inboundRepo{}
	service := NewInboundService(repo, &inboundObjectsFake{repo: repo, err: admin.ErrContentExpired})
	if _, err := service.ReadInboundContent(context.Background(), "operator@example.com", "app", "11112222333344445555666677778888"); !errors.Is(err, admin.ErrContentExpired) {
		t.Fatal(err)
	}
	line := out.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "inbound content missing before retention id=11112222333344445555666677778888") || strings.Contains(line, "@") {
		t.Fatalf("log line %q", line)
	}
}
