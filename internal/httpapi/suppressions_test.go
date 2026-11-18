package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

type fakeSuppressions struct {
	items     []submission.ProviderSuppression
	filter    submission.ProviderSuppressionFilter
	listed    int
	added     []string
	deleted   []string
	protected map[string]bool
	actor     string
	err       error
}

func (f *fakeSuppressions) ListProviderSuppressions(_ context.Context, _ string, filter submission.ProviderSuppressionFilter) ([]submission.ProviderSuppression, bool, error) {
	f.listed++
	f.filter = filter
	if f.err != nil {
		return nil, false, f.err
	}
	items := f.items
	more := false
	if len(items) > filter.Limit {
		items, more = items[:filter.Limit], true
	}
	return items, more, nil
}
func (f *fakeSuppressions) AddProviderSuppressions(_ context.Context, app submission.App, addresses []string) error {
	f.actor = "key:" + app.KeyID
	f.added = append(f.added, addresses...)
	return f.err
}
func (f *fakeSuppressions) DeleteProviderSuppressions(_ context.Context, app submission.App, addresses []string) ([]bool, error) {
	f.actor = "key:" + app.KeyID
	out := make([]bool, len(addresses))
	for i, a := range addresses {
		out[i] = !f.protected[a]
		if out[i] {
			f.deleted = append(f.deleted, a)
		}
	}
	return out, f.err
}

func supServer(store *fakeSuppressions) (*Server, *fakeService) {
	service := &fakeService{app: submission.App{ID: "app-a", KeyID: "key-1"}}
	return NewWithOptions(service, Options{Suppressions: store}), service
}

func do(s *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

var pm = map[string]string{"X-Postmark-Server-Token": "valid-key"}
var rs = map[string]string{"Authorization": "Bearer valid-key"}

const pmBase = "/message-streams/outbound/suppressions"

func TestPostmarkSuppressionAuthAndMethods(t *testing.T) {
	store := &fakeSuppressions{}
	s, _ := supServer(store)
	for _, c := range []struct {
		method, path string
		headers      map[string]string
		want         int
	}{
		{"GET", pmBase + "/dump", nil, 401},
		{"GET", pmBase + "/dump", map[string]string{"X-Postmark-Server-Token": "bad"}, 401},
		{"GET", pmBase + "/dump", rs, 401},
		{"POST", pmBase + "/dump", pm, 405},
		{"GET", pmBase, pm, 405},
		{"GET", pmBase + "/delete", pm, 405},
		{"GET", pmBase + "/other", pm, 404},
		{"GET", "/message-streams/outbound/other", pm, 404},
		{"GET", "/message-streams/broadcasts/suppressions/dump", pm, 422},
		{"GET", "/message-streams/Outbound/suppressions/dump", pm, 422},
	} {
		if w := do(s, c.method, c.path, "", c.headers); w.Code != c.want {
			t.Errorf("%s %s: got %d want %d", c.method, c.path, w.Code, c.want)
		}
	}
	// Without a store the routes are unavailable.
	if w := do(New(&fakeService{app: submission.App{ID: "a"}}), "GET", pmBase+"/dump", "", pm); w.Code != 503 {
		t.Errorf("no store: %d", w.Code)
	}
	if store.listed != 0 {
		t.Fatal("store reached by rejected requests")
	}
}

func TestPostmarkDumpShapeFiltersAndRejections(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	store := &fakeSuppressions{items: []submission.ProviderSuppression{
		{ID: "a", Address: "x@example.net", Reason: "hard_bounce", Origin: "recipient", CreatedAt: at},
		{ID: "b", Address: "y@example.net", Reason: "unsubscribe", Origin: "recipient", CreatedAt: at},
		{ID: "c", Address: "z@example.net", Reason: "manual", Origin: "console", CreatedAt: at},
	}}
	s, _ := supServer(store)
	w := do(s, "GET", pmBase+"/dump?SuppressionReason=ManualSuppression&Origin=Customer&fromdate=2026-03-01&todate=2026-03-04", "", pm)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var got struct {
		Suppressions []map[string]string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Suppressions) != 3 {
		t.Fatalf("body %s", w.Body)
	}
	want := map[string]string{"EmailAddress": "x@example.net", "SuppressionReason": "HardBounce", "Origin": "Recipient", "CreatedAt": "2026-03-04T05:06:07Z"}
	for k, v := range want {
		if got.Suppressions[0][k] != v {
			t.Errorf("%s = %q", k, got.Suppressions[0][k])
		}
	}
	if got.Suppressions[1]["SuppressionReason"] != "ManualSuppression" || got.Suppressions[2]["Origin"] != "Customer" {
		t.Error("mapping")
	}
	f := store.filter
	if strings.Join(f.Reasons, ",") != "manual,unsubscribe" || strings.Join(f.Origins, ",") != "api,console" ||
		!f.From.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) || !f.To.Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("filter %+v", f)
	}
	store.listed = 0
	for _, q := range []string{"EmailAddress=x@example.net", "emailaddress=x", "SuppressionReason=Nope", "Origin=Nope", "fromdate=yesterday", "x=1", "Origin=Customer&Origin=Recipient"} {
		w := do(s, "GET", pmBase+"/dump?"+q, "", pm)
		if w.Code != 422 || strings.Contains(w.Body.String(), "x@example.net") {
			t.Errorf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	if store.listed != 0 {
		t.Fatal("rejected query reached the store")
	}
	// Admin origin never matches here.
	w = do(s, "GET", pmBase+"/dump?Origin=Admin", "", pm)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Suppressions":[]`) || store.listed != 0 {
		t.Errorf("admin origin: %s", w.Body)
	}
}

func TestPostmarkDumpCap(t *testing.T) {
	store := &fakeSuppressions{}
	for i := 0; i < postmarkDumpCap+1; i++ {
		store.items = append(store.items, submission.ProviderSuppression{ID: "i", Address: "a@example.net", Reason: "manual", Origin: "api"})
	}
	s, _ := supServer(store)
	w := do(s, "GET", pmBase+"/dump", "", pm)
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"ErrorCode":1407`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	store.items = store.items[:postmarkDumpCap]
	if w = do(s, "GET", pmBase+"/dump", "", pm); w.Code != 200 {
		t.Fatalf("exact cap: %d", w.Code)
	}
}

func TestPostmarkAddAndDelete(t *testing.T) {
	store := &fakeSuppressions{protected: map[string]bool{"spam@example.net": true}}
	s, _ := supServer(store)
	body := `{"Suppressions":[{"EmailAddress":"a@example.net"},{"EmailAddress":"not an address"},{"EmailAddress":"Name <b@example.net>"}]}`
	w := do(s, "POST", pmBase, body, pm)
	var got struct {
		Suppressions []struct {
			EmailAddress, Status string
			Message              *string
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Suppressions) != 3 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got.Suppressions[0].Status != "Suppressed" || got.Suppressions[0].Message != nil || got.Suppressions[1].Status != "Failed" || got.Suppressions[2].Status != "Failed" {
		t.Errorf("%+v", got.Suppressions)
	}
	if len(store.added) != 1 || store.added[0] != "a@example.net" || store.actor != "key:key-1" {
		t.Errorf("added %v actor %q", store.added, store.actor)
	}
	w = do(s, "POST", pmBase+"/delete", `{"Suppressions":[{"EmailAddress":"a@example.net"},{"EmailAddress":"spam@example.net"},{"EmailAddress":"bad"}]}`, pm)
	got.Suppressions = nil
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Suppressions) != 3 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got.Suppressions[0].Status != "Deleted" || got.Suppressions[1].Status != "Failed" || got.Suppressions[2].Status != "Failed" {
		t.Errorf("%+v", got.Suppressions)
	}
	if len(store.deleted) != 1 {
		t.Errorf("deleted %v", store.deleted)
	}
	// Bounds and strictness.
	var many []string
	for i := 0; i < 51; i++ {
		many = append(many, `{"EmailAddress":"a@example.net"}`)
	}
	for name, c := range map[string]struct{ target, body string }{
		"51 items":        {pmBase, `{"Suppressions":[` + strings.Join(many, ",") + `]}`},
		"empty":           {pmBase, `{"Suppressions":[]}`},
		"unknown field":   {pmBase, `{"Suppressions":[{"EmailAddress":"a@example.net"}],"x":1}`},
		"duplicate field": {pmBase, `{"Suppressions":[],"Suppressions":[]}`},
		"query":           {pmBase + "?EmailAddress=a@example.net", `{"Suppressions":[{"EmailAddress":"a@example.net"}]}`},
		"not json":        {pmBase + "/delete", `nope`},
	} {
		before := len(store.added)
		w := do(s, "POST", c.target, c.body, pm)
		if w.Code != 422 || len(store.added) != before {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := do(s, "POST", pmBase, `{"Suppressions":[`+strings.Join(many[:50], ",")+`]}`, pm); w.Code != 200 {
		t.Errorf("50 items: %d", w.Code)
	}
}

func TestPostmark406SingleAndBatch(t *testing.T) {
	service := &fakeService{app: submission.App{ID: "app-a"}, acceptErr: submission.ErrAllRecipientsSuppressed}
	s := New(service)
	msg := `{"From":"sender@example.com","To":"a@example.net","Subject":"A","TextBody":"body"}`
	w := do(s, "POST", "/email", msg, pm)
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"ErrorCode":406`) || strings.Contains(w.Body.String(), "a@example.net") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !service.requests[0].RejectAllSuppressed {
		t.Fatal("postmark single must request the rejection")
	}
	w = do(s, "POST", "/email/batch", "["+msg+","+msg+"]", pm)
	var results []struct{ ErrorCode int }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results) != 2 || results[0].ErrorCode != 406 || results[1].ErrorCode != 406 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !service.requests[1].RejectAllSuppressed {
		t.Fatal("postmark batch must request the rejection")
	}
	// Other adapters never set it.
	service2 := &fakeService{app: submission.App{ID: "app-a"}}
	do(New(service2), "POST", "/emails", `{"from":"sender@example.com","to":["a@example.net"],"subject":"A","text":"b"}`, rs)
	if len(service2.requests) != 1 || service2.requests[0].RejectAllSuppressed {
		t.Fatal("resend must not request the rejection")
	}
}

func TestResendSuppressionList(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	id1, id2 := strings.Repeat("a", 32), strings.Repeat("b", 32)
	store := &fakeSuppressions{items: []submission.ProviderSuppression{
		{ID: id1, Address: "x@example.net", Reason: "hard_bounce", Origin: "recipient", SourceMessageID: strings.Repeat("c", 32), CreatedAt: at},
		{ID: id2, Address: "y@example.net", Reason: "unsubscribe", Origin: "recipient", CreatedAt: at},
	}}
	s, _ := supServer(store)
	w := do(s, "GET", "/suppressions?limit=1&origin=bounce", "", rs)
	var got struct {
		Object  string
		HasMore bool `json:"has_more"`
		Data    []map[string]any
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Object != "list" || !got.HasMore || len(got.Data) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	d := got.Data[0]
	if d["id"] != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" || d["email"] != "x@example.net" || d["origin"] != "bounce" || d["source_id"] != "cccccccc-cccc-cccc-cccc-cccccccccccc" {
		t.Errorf("%v", d)
	}
	if store.filter.Limit != 1 || strings.Join(store.filter.Reasons, ",") != "hard_bounce" {
		t.Errorf("%+v", store.filter)
	}
	w = do(s, "GET", "/suppressions?after=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "", rs)
	if w.Code != 200 || store.filter.After != id1 || store.filter.Limit != 20 {
		t.Errorf("after: %d %+v", w.Code, store.filter)
	}
	got.Data = nil
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Data[1]["origin"] != "manual" || got.Data[1]["source_id"] != nil {
		t.Errorf("%v", got.Data[1])
	}
	store.listed = 0
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "origin=x", "after=zzz", "after=" + id1 + "&before=" + id2, "email=x@example.net", "limit=1&limit=2"} {
		if w := do(s, "GET", "/suppressions?"+q, "", rs); w.Code != 422 {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
	if store.listed != 0 {
		t.Fatal("rejected query reached the store")
	}
	store.err = submission.ErrInvalidMessage
	if w := do(s, "GET", "/suppressions?after="+id1, "", rs); w.Code != 422 {
		t.Errorf("unknown cursor: %d", w.Code)
	}
	store.err = context.DeadlineExceeded
	if w := do(s, "GET", "/suppressions", "", rs); w.Code != 503 || strings.Contains(w.Body.String(), "deadline") {
		t.Errorf("store error: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		method, path string
		headers      map[string]string
		want         int
	}{
		{"GET", "/suppressions", nil, 401},
		{"GET", "/suppressions", pm, 401},
		{"POST", "/suppressions", rs, 405},
		{"DELETE", "/suppressions", rs, 405},
		{"GET", "/suppressions/" + id1, rs, 404},
		{"DELETE", "/suppressions/x@example.net", rs, 404},
	} {
		if w := do(s, c.method, c.path, "", c.headers); w.Code != c.want {
			t.Errorf("%s %s: %d", c.method, c.path, w.Code)
		}
	}
}

// The postmark SDK 5.1.0 sends getSuppressions as camelCase with count and
// offset; these are the exact query strings it produces.
func TestPostmarkDumpAcceptsSDKQueryStrings(t *testing.T) {
	store := &fakeSuppressions{}
	s, _ := supServer(store)
	for _, q := range []string{
		"count=100&offset=0",
		"count=100&offset=0&suppressionReason=HardBounce",
		"count=100&offset=0&suppressionReason=ManualSuppression&origin=Customer&fromDate=2026-03-01&toDate=2026-03-04",
		"SuppressionReason=HardBounce&FROMDATE=2026-03-01",
	} {
		store.listed = 0
		if w := do(s, "GET", pmBase+"/dump?"+q, "", pm); w.Code != 200 || store.listed != 1 {
			t.Errorf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	do(s, "GET", pmBase+"/dump?count=100&offset=0&suppressionReason=ManualSuppression&origin=Customer&fromDate=2026-03-01&toDate=2026-03-04", "", pm)
	f := store.filter
	if strings.Join(f.Reasons, ",") != "manual,unsubscribe" || strings.Join(f.Origins, ",") != "api,console" ||
		!f.From.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) || !f.To.Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("camelCase filter not applied: %+v", f)
	}
	store.listed = 0
	for _, q := range []string{
		"emailAddress=x@example.net", "EMAILADDRESS=x@example.net", "count=1&count=2",
		"fromdate=2026-03-01&fromDate=2026-03-02", "suppressionReason=HardBounce&SuppressionReason=HardBounce",
	} {
		w := do(s, "GET", pmBase+"/dump?"+q, "", pm)
		if w.Code != 422 || strings.Contains(w.Body.String(), "x@example.net") {
			t.Errorf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	if w := do(s, "GET", pmBase+"/dump?emailAddress=x@example.net", "", pm); !strings.Contains(w.Body.String(), "EmailAddress is not supported") {
		t.Errorf("specific rejection lost: %s", w.Body)
	}
	if store.listed != 0 {
		t.Fatal("rejected query reached the store")
	}
}

func TestPostmarkSuppressionAddressValidityMatchesStore(t *testing.T) {
	// 998 bytes as submitted, 999 once lowercased (U+023A is 2 bytes, its
	// lowercase 3): the item must fail alone, not the whole request.
	long := strings.Repeat("a", 998-len("Ⱥ@example.net")) + "Ⱥ@example.net"
	if len(long) != 998 || len(strings.ToLower(long)) != 999 {
		t.Fatalf("fixture lengths %d %d", len(long), len(strings.ToLower(long)))
	}
	store := &fakeSuppressions{}
	s, _ := supServer(store)
	w := do(s, "POST", pmBase, `{"Suppressions":[{"EmailAddress":"`+long+`"},{"EmailAddress":"ok@example.net"}]}`, pm)
	var got struct{ Suppressions []struct{ Status string } }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Suppressions) != 2 || got.Suppressions[0].Status != "Failed" || got.Suppressions[1].Status != "Suppressed" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(store.added) != 1 || store.added[0] != "ok@example.net" {
		t.Errorf("added %v", store.added)
	}
}

func TestProviderSuppressionWriteWithRevokedKeyIs401(t *testing.T) {
	store := &fakeSuppressions{err: submission.ErrInvalidKey}
	s, _ := supServer(store)
	body := `{"Suppressions":[{"EmailAddress":"a@example.net"}]}`
	for _, target := range []string{pmBase, pmBase + "/delete"} {
		if w := do(s, "POST", target, body, pm); w.Code != 401 {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}
