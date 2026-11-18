package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeWebhooks struct {
	calls              int
	op, actor, app, id string
	input              WebhookInput
	patch              WebhookPatch
	err                error
	items              []Webhook
	hook               Webhook
	testResult         WebhookTestResult
}

func (f *fakeWebhooks) rec(op, actor, app, id string) {
	f.calls++
	f.op, f.actor, f.app, f.id = op, actor, app, id
}
func (f *fakeWebhooks) ListWebhooks(_ context.Context, app string) ([]Webhook, error) {
	f.rec("list", "", app, "")
	return f.items, f.err
}
func (f *fakeWebhooks) CreateWebhook(_ context.Context, actor, app string, in WebhookInput) (IssuedWebhook, error) {
	f.rec("create", actor, app, "")
	f.input = in
	return IssuedWebhook{Webhook: f.hook, Secret: "tpwh_issued"}, f.err
}
func (f *fakeWebhooks) UpdateWebhook(_ context.Context, actor, app, id string, p WebhookPatch) (Webhook, error) {
	f.rec("update", actor, app, id)
	f.patch = p
	return f.hook, f.err
}
func (f *fakeWebhooks) DeleteWebhook(_ context.Context, actor, app, id string) error {
	f.rec("delete", actor, app, id)
	return f.err
}
func (f *fakeWebhooks) RotateWebhookSecret(_ context.Context, actor, app, id string) (IssuedWebhook, error) {
	f.rec("rotate", actor, app, id)
	return IssuedWebhook{Webhook: f.hook, Secret: "tpwh_rotated"}, f.err
}
func (f *fakeWebhooks) TestWebhook(_ context.Context, actor, app, id string) (WebhookTestResult, error) {
	f.rec("test", actor, app, id)
	return f.testResult, f.err
}

func TestWebhookRoutes(t *testing.T) {
	app, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f := &fakeWebhooks{hook: Webhook{ID: id, Endpoint: "https://hooks.example.com/in", Active: true}, testResult: WebhookTestResult{Success: false, Failure: "timeout"}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Webhooks: f}
	base := "/api/v1/apps/" + app + "/webhooks"
	one := base + "/" + id
	if w := send(h, "GET", base, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", base, `{"endpoint":"https://hooks.example.com/in"}`, "valid"); w.Code != 403 || f.calls != 0 {
		t.Fatal("non-admin reached webhooks", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}

	w := send(h, "GET", base, "", "valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"webhooks":[]`) || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), `"secret":`) {
		t.Fatal("list", w.Code, w.Body.String())
	}
	w = send(h, "POST", base, `{"endpoint":"HTTPS://Hooks.Example.com:443/in?x=1","description":"d","events":["accepted","opened"]}`, "valid")
	if w.Code != 201 || f.op != "create" || f.actor != "operator-1" || f.app != app || f.input.Endpoint != "https://hooks.example.com/in?x=1" || len(f.input.Events) != 2 || !strings.Contains(w.Body.String(), `"secret":"tpwh_issued"`) {
		t.Fatal("create", w.Code, w.Body.String(), f.input)
	}
	w = send(h, "PATCH", one, `{"active":false}`, "valid")
	if w.Code != 200 || f.op != "update" || f.id != id || f.patch.Active == nil || *f.patch.Active || strings.Contains(w.Body.String(), `"secret":`) {
		t.Fatal("patch", w.Code, w.Body.String())
	}
	if w = send(h, "PATCH", one, `{"events":[]}`, "valid"); w.Code != 200 || f.patch.Events == nil || len(*f.patch.Events) != 0 {
		t.Fatal("patch empty events", w.Code)
	}
	if w = send(h, "PATCH", one, `{"endpoint":"https://Hooks.Example.com/x"}`, "valid"); w.Code != 200 || *f.patch.Endpoint != "https://hooks.example.com/x" {
		t.Fatal("patch endpoint not normalized", w.Code)
	}
	if w = send(h, "DELETE", one, "", "valid"); w.Code != 204 || f.op != "delete" || w.Body.Len() != 0 {
		t.Fatal("delete", w.Code)
	}
	w = send(h, "POST", one+"/rotate-secret", "", "valid")
	if w.Code != 200 || f.op != "rotate" || !strings.Contains(w.Body.String(), `"secret":"tpwh_rotated"`) {
		t.Fatal("rotate", w.Code, w.Body.String())
	}
	w = send(h, "POST", one+"/test", "", "valid")
	if w.Code != 200 || f.op != "test" || !strings.Contains(w.Body.String(), `"failure":"timeout"`) || strings.Contains(w.Body.String(), `"secret":`) {
		t.Fatal("test", w.Code, w.Body.String())
	}
}

func TestWebhookRouteValidation(t *testing.T) {
	app, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f := &fakeWebhooks{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Webhooks: f}
	base := "/api/v1/apps/" + app + "/webhooks"
	one := base + "/" + id
	long := strings.Repeat("d", 257)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", base, ``, 400},
		{"POST", base, `{`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com","extra":1}`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com"}{}`, 400},
		{"POST", base, `{"endpoint":"http://hooks.example.com"}`, 400},
		{"POST", base, `{"endpoint":"https://10.0.0.1/x"}`, 400},
		{"POST", base, `{"endpoint":"https://localhost/x"}`, 400},
		{"POST", base, `{"endpoint":"https://u:p@hooks.example.com"}`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com:8443"}`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com/#f"}`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com","description":" x"}`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com","description":"` + long + `"}`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com","events":["bogus"]}`, 400},
		{"POST", base, `{"endpoint":"https://hooks.example.com","events":["opened","opened"]}`, 400},
		{"PATCH", one, `{}`, 400},
		{"PATCH", one, ``, 400},
		{"PATCH", one, `{"active":true,"unknown":1}`, 400},
		{"PATCH", one, `{"endpoint":"ftp://x.example.com"}`, 400},
		{"PATCH", one, `{"events":["bogus"]}`, 400},
		{"PATCH", one, `{"description":"` + long + `"}`, 400},
		{"DELETE", one + "?x=1", "", 400},
		{"DELETE", one, `{}`, 400},
		{"POST", one + "/rotate-secret?x=1", "", 400},
		{"POST", one + "/rotate-secret", `{}`, 400},
		{"POST", one + "/test?x=1", "", 400},
		{"POST", one + "/test", `{}`, 400},
		{"PUT", base, "", 405},
		{"DELETE", base, "", 405},
		{"GET", one, "", 405},
		{"PUT", one, "", 405},
		{"GET", one + "/test", "", 405},
		{"GET", one + "/rotate-secret", "", 405},
		{"POST", one + "/other", "", 404},
		{"DELETE", one + "/", "", 404},
		{"PATCH", one + "/", `{"active":false}`, 404},
		{"PATCH", base + "/short", `{"active":true}`, 404},
		{"POST", one + "/test/more", "", 404},
	} {
		if w := send(h, tc.method, tc.path, tc.body, "valid"); w.Code != tc.status {
			t.Errorf("%s %s %q: got %d, want %d", tc.method, tc.path, tc.body, w.Code, tc.status)
		}
	}
	if f.calls != 0 {
		t.Fatal("invalid request reached the service", f.op)
	}
	for path, allow := range map[string]string{base: "GET, POST", one: "PATCH, DELETE", one + "/test": "POST"} {
		if w := send(h, "PUT", path, "", "valid"); w.Header().Get("Allow") != allow {
			t.Errorf("%s: Allow %q, want %q", path, w.Header().Get("Allow"), allow)
		}
	}
}

func TestWebhookRouteErrorsAndUnavailable(t *testing.T) {
	app, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f := &fakeWebhooks{}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Webhooks: f}
	base := "/api/v1/apps/" + app + "/webhooks"
	one := base + "/" + id
	calls := []struct{ method, path, body string }{
		{"GET", base, ""},
		{"POST", base, `{"endpoint":"https://hooks.example.com/secret-path"}`},
		{"PATCH", one, `{"active":true}`},
		{"DELETE", one, ""},
		{"POST", one + "/rotate-secret", ""},
		{"POST", one + "/test", ""},
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{ErrNotFound, 404}, {ErrConflict, 409}, {ErrInvalidInput, 400}, {errors.New("db https://hooks.example.com/secret-path down"), 503}} {
		f.err = tc.err
		for _, c := range calls {
			w := send(h, c.method, c.path, c.body, "valid")
			if w.Code != tc.status || strings.Contains(w.Body.String(), "example.com") || strings.Contains(w.Body.String(), "tpwh_") {
				t.Errorf("%s %s with %v: %d %s", c.method, c.path, tc.err, w.Code, w.Body.String())
			}
			var body map[string]string
			if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body) != 1 || body["error"] == "" {
				t.Errorf("%s %s: not a plain error body %s", c.method, c.path, w.Body.String())
			}
		}
	}
	h.Webhooks = nil
	for _, c := range calls {
		if w := send(h, c.method, c.path, c.body, "valid"); w.Code != 503 {
			t.Errorf("%s %s without webhooks: %d", c.method, c.path, w.Code)
		}
	}
}

func TestWebhookFormatAndAuthRoutes(t *testing.T) {
	app, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f := &fakeWebhooks{hook: Webhook{ID: id, Format: "postmark", Endpoint: "https://hooks.example.com/in", Active: true, Auth: &WebhookAuthSummary{Username: "user", HeaderNames: []string{"X-Token"}}}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: &fakeStore{}, Webhooks: f}
	base := "/api/v1/apps/" + app + "/webhooks"
	one := base + "/" + id
	ok := `{"format":"postmark","endpoint":"https://hooks.example.com/in","events":["Delivery","Bounce"],"auth":{"username":"user","password":"pass-secret","headers":[{"name":"X-Token","value":"value-secret"}]}}`
	w := send(h, "POST", base, ok, "valid")
	if w.Code != 201 || f.input.Format != "postmark" || f.input.Auth == nil || f.input.Auth.Username() != "user" || len(f.input.Auth.Headers()) != 1 {
		t.Fatal("create postmark", w.Code, w.Body.String())
	}
	for _, body := range []string{w.Body.String(), send(h, "GET", base, "", "valid").Body.String(), send(h, "PATCH", one, `{"auth":{"username":"u","password":"pass-secret"}}`, "valid").Body.String()} {
		for _, secret := range []string{"pass-secret", "value-secret"} {
			if strings.Contains(body, secret) {
				t.Fatalf("response leaked %s: %s", secret, body)
			}
		}
	}
	if !strings.Contains(w.Body.String(), `"auth":{"username":"user","header_names":["X-Token"]}`) {
		t.Fatal("create must show the auth summary", w.Body.String())
	}
	if send(h, "PATCH", one, `{"auth":{}}`, "valid"); f.patch.Auth == nil || !f.patch.Auth.IsZero() {
		t.Fatal("empty auth must be a removal request")
	}
	if send(h, "PATCH", one, `{"active":true}`, "valid"); f.patch.Auth != nil {
		t.Fatal("absent auth must stay unchanged")
	}
	before := f.calls
	headers := func(n int) string {
		var items []string
		for i := 0; i < n; i++ {
			items = append(items, `{"name":"X-H`+string(rune('a'+i))+`","value":"v"}`)
		}
		return `[` + strings.Join(items, ",") + `]`
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", base, `{"format":"sendgrid","endpoint":"https://hooks.example.com/in"}`},
		{"POST", base, `{"format":"postmark","endpoint":"https://hooks.example.com/in","events":["accepted"]}`},
		{"POST", base, `{"format":"thinpost","endpoint":"https://hooks.example.com/in","events":["Delivery"]}`},
		{"POST", base, `{"format":"resend","endpoint":"https://hooks.example.com/in","events":["email.bounced"],"auth":{"username":"u"}}`},
		{"POST", base, `{"endpoint":"https://hooks.example.com/in","auth":{"headers":` + headers(6) + `}}`},
		{"POST", base, `{"endpoint":"https://hooks.example.com/in","auth":{"headers":[{"name":"Host","value":"x"}]}}`},
		{"POST", base, `{"endpoint":"https://hooks.example.com/in","auth":{"headers":[{"name":"X-Token","value":"a\r\nB: c"}]}}`},
		{"POST", base, `{"endpoint":"https://hooks.example.com/in","auth":{"headers":[{"name":"X_Token","value":"a"}]}}`},
		{"POST", base, `{"endpoint":"https://hooks.example.com/in","auth":{"extra":1}}`},
		{"PATCH", one, `{"format":"resend"}`},
		{"PATCH", one, `{"auth":{"username":"u","password":"p","headers":[{"name":"Authorization","value":"x"}]}}`},
		{"PATCH", one, `{"events":["bogus"]}`},
	} {
		if w := send(h, tc.method, tc.path, tc.body, "valid"); w.Code != 400 {
			t.Errorf("%s %q: got %d", tc.method, tc.body, w.Code)
		}
	}
	if f.calls != before {
		t.Fatal("invalid request reached the service")
	}
	// An event valid in another format is accepted by the handler and narrowed by the store.
	if w := send(h, "PATCH", one, `{"events":["Delivery"]}`, "valid"); w.Code != 200 {
		t.Fatal("patch events", w.Code)
	}
}
