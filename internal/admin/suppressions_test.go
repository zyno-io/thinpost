package admin

import (
	"context"
	"strings"
	"testing"
)

type suppressionTestStore struct {
	*fakeStore
	query   SuppressionQuery
	added   []string
	deleted []string
}

func (s *suppressionTestStore) ListSuppressions(_ context.Context, app string, q SuppressionQuery) (SuppressionPage, error) {
	s.app, s.query = app, q
	return SuppressionPage{}, nil
}
func (s *suppressionTestStore) AddSuppressions(_ context.Context, actor, app string, addresses []string) ([]SuppressionResult, error) {
	s.actor, s.app, s.added = actor, app, addresses
	out := make([]SuppressionResult, len(addresses))
	for i, a := range addresses {
		out[i] = SuppressionResult{Address: a, Status: "added", Suppression: Suppression{ID: strings.Repeat("c", 32), Address: a, Reason: "manual", Origin: "console"}}
	}
	return out, nil
}
func (s *suppressionTestStore) DeleteSuppressions(_ context.Context, actor, app string, ids []string) (int, error) {
	s.actor, s.app, s.deleted = actor, app, ids
	return len(ids), nil
}

func TestSuppressionRoutes(t *testing.T) {
	store := &suppressionTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Suppressions: store}
	base := "/api/v1/apps/" + strings.Repeat("a", 32) + "/suppressions"
	id := strings.Repeat("b", 32)
	if w := send(h, "GET", base, "", "bad"); w.Code != 401 {
		t.Fatal("unauthenticated list", w.Code)
	}
	h.Authenticator = fakeAuth{admin: false}
	if w := send(h, "POST", base+"/delete", `{"ids":["`+id+`"]}`, "valid"); w.Code != 403 {
		t.Fatal("non-admin delete", w.Code)
	}
	h.Authenticator = fakeAuth{admin: true}

	// List: only reason, limit and cursor; strict values.
	if w := send(h, "GET", base+"?reason=manual&limit=10", "", "valid"); w.Code != 200 || store.query.Reason != "manual" || store.query.Limit != 10 || !strings.Contains(w.Body.String(), `"suppressions":[]`) {
		t.Fatal("list", w.Code, w.Body.String())
	}
	for _, q := range []string{"?q=a@example.com", "?address=a@example.com", "?reason=bogus", "?limit=0", "?limit=101", "?limit=05", "?limit=1&limit=2", "?cursor=" + strings.Repeat("x", 513)} {
		if w := send(h, "GET", base+q, "", "valid"); w.Code != 400 {
			t.Fatal("accepted list query", q, w.Code)
		}
	}

	// Add: addresses only in the body, 1..100, every entry valid.
	if w := send(h, "POST", base, `{"addresses":["Jane@Example.COM"," <b@example.org> "]}`, "valid"); w.Code != 400 {
		t.Fatal("untrimmed address accepted", w.Code)
	}
	if w := send(h, "POST", base, `{"addresses":["Jane@Example.COM","<b@example.org>"]}`, "valid"); w.Code != 200 || len(store.added) != 2 || store.added[0] != "Jane@Example.COM" || store.added[1] != "b@example.org" || store.actor != "operator-1" {
		t.Fatal("add", w.Code, store.added)
	}
	many := `"a@example.com"` + strings.Repeat(`,"a@example.com"`, 100)
	for _, body := range []string{`{}`, `{"addresses":[]}`, `{"addresses":[` + many + `]}`, `{"addresses":["Jane <a@example.com>"]}`, `{"addresses":["a@example.com, b@example.com"]}`, `{"addresses":["nodomain"]}`, `{"addresses":["a@example.com"],"reason":"x"}`, `{"addresses":[""]}`} {
		if w := send(h, "POST", base, body, "valid"); w.Code != 400 {
			t.Fatal("accepted add body", body, w.Code)
		}
	}
	if w := send(h, "POST", base+"?a=b@example.com", `{"addresses":["a@example.com"]}`, "valid"); w.Code != 400 {
		t.Fatal("query string on add accepted")
	}

	// Search: the term is in the body only.
	if w := send(h, "POST", base+"/search", `{"q":"jane","field":"address","reason":"manual","limit":5}`, "valid"); w.Code != 200 || store.query.Q != "jane" || store.query.Field != "address" || store.query.Limit != 5 {
		t.Fatal("search", w.Code)
	}
	if w := send(h, "POST", base+"/search", `{"q":"example.com","field":"domain"}`, "valid"); w.Code != 200 || store.query.Field != "domain" || store.query.Limit != 50 {
		t.Fatal("domain search", w.Code)
	}
	for _, body := range []string{`{}`, `{"q":""}`, `{"q":"a","field":"any"}`, `{"q":"a","reason":"x"}`, `{"q":"a","limit":0}`, `{"q":"a","limit":101}`, `{"q":"a","extra":1}`} {
		if w := send(h, "POST", base+"/search", body, "valid"); w.Code != 400 {
			t.Fatal("accepted search body", body, w.Code)
		}
	}
	if w := send(h, "POST", base+"/search?q=jane", `{"q":"jane"}`, "valid"); w.Code != 400 {
		t.Fatal("query string on search accepted")
	}
	if w := send(h, "GET", base+"/search", "", "valid"); w.Code != 405 {
		t.Fatal("search GET", w.Code)
	}

	// Delete: IDs only.
	if w := send(h, "POST", base+"/delete", `{"ids":["`+id+`"]}`, "valid"); w.Code != 200 || len(store.deleted) != 1 || !strings.Contains(w.Body.String(), `"deleted":1`) {
		t.Fatal("delete", w.Code)
	}
	for _, body := range []string{`{}`, `{"ids":[]}`, `{"ids":["a@example.com"]}`, `{"ids":["` + strings.ToUpper(id) + `"]}`, `{"ids":["` + id + `"],"x":1}`} {
		if w := send(h, "POST", base+"/delete", body, "valid"); w.Code != 400 {
			t.Fatal("accepted delete body", body, w.Code)
		}
	}
	if w := send(h, "DELETE", base+"/"+id, "", "valid"); w.Code == 200 {
		t.Fatal("ID path routes must not exist")
	}

	// Without a configured store the routes report unavailable.
	h.Suppressions = nil
	if w := send(h, "GET", base, "", "valid"); w.Code != 503 {
		t.Fatal("missing store", w.Code)
	}
}

func TestNormalizeSuppressionAddress(t *testing.T) {
	for in, want := range map[string]string{
		"a@example.com":        "a@example.com",
		"Jane.Doe@Example.COM": "Jane.Doe@Example.COM",
		"<a@example.com>":      "a@example.com",
		`"a b"@example.com`:    "a b@example.com",
	} {
		if got, ok := NormalizeSuppressionAddress(in); !ok || got != want {
			t.Errorf("%q = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "a", "a@", "@b.example", "Jane <a@example.com>", "a@example.com b@example.com", "a@example.com,b@example.com", " a@example.com", "a@example.com\x00", "a@" + strings.Repeat("x", 1000), "a@example.com (comment)"} {
		if got, ok := NormalizeSuppressionAddress(in); ok {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestSuppressionAddAcceptsContractSizedBody(t *testing.T) {
	store := &suppressionTestStore{fakeStore: &fakeStore{}}
	h := &Handler{Authenticator: fakeAuth{admin: true}, Store: store, Suppressions: store}
	base := "/api/v1/apps/" + strings.Repeat("a", 32) + "/suppressions"
	var entries []string
	for i := 0; i < 100; i++ {
		entries = append(entries, `"`+strings.Repeat("x", 980)+string(rune('a'+i%26))+strings.Repeat("y", 3)+`@example.com"`)
	}
	body := `{"addresses":[` + strings.Join(entries, ",") + `]}`
	if len(body) <= 16384 {
		t.Fatal("body too small")
	}
	if w := send(h, "POST", base, body, "valid"); w.Code != 200 || len(store.added) != 100 {
		t.Fatal("contract-sized add rejected", w.Code)
	}
	if w := send(h, "POST", base, `{"addresses":["a@example.com"],"pad":"`+strings.Repeat("z", 130000)+`"}`, "valid"); w.Code != 400 {
		t.Fatal("oversized body accepted")
	}
}

func TestNormalizeSuppressionAddressBoundsTheLowercasedForm(t *testing.T) {
	// 998 bytes as given, 999 lowercased (U+023A lowercases from 2 to 3 bytes).
	long := strings.Repeat("a", 998-len("Ⱥ@example.net")) + "Ⱥ@example.net"
	if _, ok := NormalizeSuppressionAddress(long); ok {
		t.Fatal("address whose lowercase exceeds 998 bytes accepted")
	}
	if _, ok := NormalizeSuppressionAddress(strings.Repeat("a", 998-len("@example.net")) + "@example.net"); !ok {
		t.Fatal("998-byte address rejected")
	}
}
