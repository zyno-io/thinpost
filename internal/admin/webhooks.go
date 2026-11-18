package admin

import (
	"context"
	"time"
)

// Webhook is one endpoint and its event subscription. It never carries the
// signing secret or authentication values. Format is fixed at creation; an
// empty Events means every event of the format. SecretCreatedAt is absent for
// formats without a secret.
type Webhook struct {
	ID              string              `json:"id"`
	Format          string              `json:"format"`
	Endpoint        string              `json:"endpoint"`
	Description     string              `json:"description"`
	Events          []string            `json:"events,omitempty"`
	Active          bool                `json:"active"`
	CreatedAt       time.Time           `json:"created_at"`
	SecretCreatedAt *time.Time          `json:"secret_created_at,omitempty"`
	Auth            *WebhookAuthSummary `json:"auth,omitempty"`
}

// WebhookAuthSummary is what responses show of an endpoint's authentication:
// the Basic user name and custom header names, never passwords or values.
type WebhookAuthSummary struct {
	Username    string   `json:"username,omitempty"`
	HeaderNames []string `json:"header_names,omitempty"`
}

// SealedWebhookAuth is what the Store sees: ciphertext and its display summary.
// An empty Ciphertext removes the endpoint's authentication.
type SealedWebhookAuth struct {
	Ciphertext []byte
	Summary    WebhookAuthSummary
}

// Summary lists the names an auth value carries, for display.
func (a WebhookAuth) Summary() WebhookAuthSummary {
	s := WebhookAuthSummary{Username: a.Username()}
	for _, h := range a.Headers() {
		s.HeaderNames = append(s.HeaderNames, h.Name)
	}
	return s
}

// WebhookTarget is what a test send needs from storage: the endpoint, its
// format, and the sealed secret (nil when unsigned) and authentication (nil
// when none).
type WebhookTarget struct {
	Endpoint     string
	Format       string
	SealedSecret []byte
	SealedAuth   []byte
}

// IssuedWebhook carries the signing secret, shown once on create and rotate.
// Secret is empty for formats without one.
type IssuedWebhook struct {
	Webhook Webhook `json:"webhook"`
	Secret  string  `json:"secret,omitempty"`
}

// WebhookInput creates an endpoint. Format defaults to thinpost. The service
// seals Auth before the Store sees the input.
type WebhookInput struct {
	Format      string       `json:"format"`
	Endpoint    string       `json:"endpoint"`
	Description string       `json:"description"`
	Events      []string     `json:"events"`
	Auth        *WebhookAuth `json:"auth"`
}

// WebhookPatch leaves absent fields unchanged. A non-nil empty Events means
// every event. A non-nil Auth replaces the authentication; a zero WebhookAuth
// removes it. The format cannot be changed.
type WebhookPatch struct {
	Endpoint    *string      `json:"endpoint"`
	Description *string      `json:"description"`
	Events      *[]string    `json:"events"`
	Active      *bool        `json:"active"`
	Auth        *WebhookAuth `json:"auth"`
}

// WebhookTestResult reports one signed test delivery. HTTPStatus is 0 when no
// response arrived; Failure then names the class.
type WebhookTestResult struct {
	Success    bool   `json:"success"`
	HTTPStatus int    `json:"http_status"`
	Failure    string `json:"failure,omitempty"`
}

type WebhookAdmin interface {
	ListWebhooks(ctx context.Context, appID string) ([]Webhook, error)
	CreateWebhook(ctx context.Context, actor, appID string, in WebhookInput) (IssuedWebhook, error)
	UpdateWebhook(ctx context.Context, actor, appID, id string, patch WebhookPatch) (Webhook, error)
	DeleteWebhook(ctx context.Context, actor, appID, id string) error
	RotateWebhookSecret(ctx context.Context, actor, appID, id string) (IssuedWebhook, error)
	TestWebhook(ctx context.Context, actor, appID, id string) (WebhookTestResult, error)
}
