package tracking

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLinkKeys(t *testing.T) *LinkKeys {
	t.Helper()
	keys, err := NewLinkKeys(seedA, nil)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestEncryptedLinksSurviveAbsentAnalyticsAndRetainExactDestination(t *testing.T) {
	const origin = "https://track.example.com"
	const token = "000102030405060708090a0b0c0d0e0f"
	const destination = "https://example.net/path/%2F?case=private&next=%2Fhome#résumé"
	keys := testLinkKeys(t)
	for _, kind := range []string{"click", "open"} {
		target := ""
		if kind == "click" {
			target = destination
		}
		link, err := keys.link(origin, kind, token, target)
		if err != nil {
			t.Fatal(err)
		}
		assertOpaqueLink(t, link, origin, kind[:1], token, destination)
		for _, failure := range []error{nil, ErrNotFound, errors.New("database offline"), errors.New("mapping object missing")} {
			v := &fakeVisitor{err: failure}
			h := &Handler{Links: keys, Visitor: v}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(method, link, nil))
				if kind == "click" && (w.Code != http.StatusFound || w.Header().Get("Location") != destination || w.Body.Len() != 0) {
					t.Fatal("analytics failure broke or changed redirect", w.Code)
				}
				if kind == "open" && (w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/gif") {
					t.Fatal("analytics failure broke pixel", w.Code)
				}
				if v.record != (method == http.MethodGet) || (method == http.MethodHead && w.Body.Len() != 0) {
					t.Fatal("HEAD recorded an event or returned a body")
				}
				if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store, max-age=0" {
					t.Fatal("missing privacy headers")
				}
			}
		}
		// A replica with no database/objects can still serve an authentic link.
		w := httptest.NewRecorder()
		(&Handler{Links: keys}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, link, nil))
		if kind == "click" && w.Code != http.StatusFound || kind == "open" && w.Code != http.StatusOK {
			t.Fatal("stateless resolution failed", w.Code)
		}
	}
}

var seedA = bytes.Repeat([]byte{0xA1}, 32)

const (
	linkOrigin = "https://track.example.com"
	linkToken  = "000102030405060708090a0b0c0d0e0f"
	linkTarget = "https://example.net/private/path?case=secret"
)

func assertOpaqueLink(t *testing.T, link, origin, letter, token, destination string) {
	t.Helper()
	if !strings.HasPrefix(link, origin+"/t/"+letter+"/v3/") || strings.Count(strings.TrimPrefix(link, origin), "/") != 5 {
		t.Fatal("unexpected link shape")
	}
	payload := link[strings.LastIndex(link, "/")+1:]
	for _, secret := range []string{token, base64.RawURLEncoding.EncodeToString([]byte(destination)), "example.net", "secret"} {
		if secret != "" && strings.Contains(link, secret) {
			t.Fatal("link exposes content")
		}
	}
	if _, err := base64.RawURLEncoding.Strict().DecodeString(payload); err != nil {
		t.Fatal("payload is not base64url")
	}
}

func rotatedKeys(t *testing.T) (old, rotated *LinkKeys) {
	t.Helper()
	old = testLinkKeys(t)
	rotated, err := NewLinkKeys(bytes.Repeat([]byte{1}, 32), [][]byte{seedA})
	if err != nil {
		t.Fatal(err)
	}
	return old, rotated
}

func TestEncryptedLinksRoundTripEveryKind(t *testing.T) {
	keys := testLinkKeys(t)
	hash, _ := HashToken(linkToken)
	open, err := keys.link(linkOrigin, "open", linkToken, "")
	if err != nil {
		t.Fatal(err)
	}
	click, err := keys.link(linkOrigin, "click", linkToken, linkTarget)
	if err != nil {
		t.Fatal(err)
	}
	unsub, err := keys.UnsubscribeLink(linkOrigin, linkToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range [][3]string{{open, "o"}, {click, "c"}, {unsub, "u"}} {
		assertOpaqueLink(t, l[0], linkOrigin, l[1], linkToken, linkTarget)
	}
	if got, kind, dest, err := keys.verify(strings.TrimPrefix(open, linkOrigin)); err != nil || got != hash || kind != "open" || dest != "" {
		t.Fatal("open round trip", err)
	}
	if got, kind, dest, err := keys.verify(strings.TrimPrefix(click, linkOrigin)); err != nil || got != hash || kind != "click" || dest != linkTarget {
		t.Fatal("click round trip", err)
	}
	if id, err := keys.verifyUnsubscribe(strings.TrimPrefix(unsub, linkOrigin)); err != nil || id != linkToken {
		t.Fatal("unsubscribe round trip", err)
	}
	again, _ := keys.link(linkOrigin, "open", linkToken, "")
	if again == open {
		t.Fatal("nonce was reused")
	}
}

func TestEncryptedLinksRejectTampering(t *testing.T) {
	_, keys := rotatedKeys(t)
	otherID := ""
	for id := range keys.aeads {
		if id != keys.active {
			otherID = id
		}
	}
	for _, kind := range []string{"o", "c", "u"} {
		var link string
		var err error
		switch kind {
		case "o":
			link, err = keys.link(linkOrigin, "open", linkToken, "")
		case "c":
			link, err = keys.link(linkOrigin, "click", linkToken, linkTarget)
		case "u":
			link, err = keys.UnsubscribeLink(linkOrigin, linkToken)
		}
		if err != nil {
			t.Fatal(err)
		}
		path := strings.TrimPrefix(link, linkOrigin)
		check := func(p string) error {
			if kind == "u" {
				_, err := keys.verifyUnsubscribe(p)
				return err
			}
			_, _, _, err := keys.verify(p)
			return err
		}
		if err := check(path); err != nil {
			t.Fatal(kind, err)
		}
		parts := strings.Split(path, "/")
		sealed, _ := base64.RawURLEncoding.DecodeString(parts[5])
		for i := range sealed {
			bad := bytes.Clone(sealed)
			bad[i] ^= 1
			parts[5] = base64.RawURLEncoding.EncodeToString(bad)
			if check(strings.Join(parts, "/")) == nil {
				t.Fatal("tampered byte accepted", kind, i)
			}
		}
		parts[5] = base64.RawURLEncoding.EncodeToString(sealed)
		good := strings.Join(parts, "/")
		cases := map[string]string{
			"truncated": good[:len(good)-1], "extended": good + "A", "trailing slash": good + "/",
			"key id swap": strings.Replace(good, keys.active, otherID, 1), "unknown key": strings.Replace(good, keys.active, strings.Repeat("0", 32), 1),
			"version": strings.Replace(good, "/v3/", "/v1/", 1), "empty payload": strings.Join(parts[:5], "/") + "/",
			"oversize": strings.Join(parts[:5], "/") + "/" + strings.Repeat("A", 4000),
			"padded":   good + "=",
		}
		for _, other := range []string{"o", "c", "u"} {
			if other != kind {
				cases["kind swap "+other] = strings.Replace(good, "/t/"+kind+"/", "/t/"+other+"/", 1)
			}
		}
		for name, p := range cases {
			if check(p) == nil {
				t.Fatal("accepted", kind, name)
			}
			// Cross-kind: whichever verifier, never accepted.
			if kind != "u" {
				if _, err := keys.verifyUnsubscribe(p); err == nil {
					t.Fatal("accepted as unsubscribe", name)
				}
			}
		}
	}
}

func TestEncryptedLinksHandlerRejectsBeforeAnalytics(t *testing.T) {
	keys := testLinkKeys(t)
	link, _ := keys.link(linkOrigin, "click", linkToken, linkTarget)
	path := strings.TrimPrefix(link, linkOrigin)
	v := &fakeVisitor{}
	h := &Handler{Links: keys, Visitor: v}
	for _, p := range []string{path + "?url=https://evil.example", path + "/extra", strings.Replace(path, "/v3/", "/%76%33/", 1), strings.Replace(path, "/c/", "/o/", 1),
		"/t/c/" + linkToken + "/v1/" + keys.active + "/" + base64.RawURLEncoding.EncodeToString([]byte(linkTarget)) + "/" + strings.Repeat("A", 86)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || w.Body.Len() != 0 {
			t.Fatal("untrusted redirect accepted", w.Code)
		}
	}
	// Flip one tag bit and re-encode canonically, so the path always differs from
	// the genuine link and still reaches the AEAD.
	cut := strings.LastIndex(path, "/") + 1
	raw, err := base64.RawURLEncoding.DecodeString(path[cut:])
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	w := httptest.NewRecorder()
	(&Handler{Links: keys, Visitor: v}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path[:cut]+base64.RawURLEncoding.EncodeToString(raw), nil))
	if w.Code != http.StatusNotFound || v.calls != 0 {
		t.Fatal("invalid link reached analytics")
	}
	for _, target := range []string{"javascript:alert(1)", "//example.com", "https://user:pass@example.com", "https://example.com/\r\nLocation: https://evil.example"} {
		if _, err := keys.link(linkOrigin, "click", linkToken, target); err == nil {
			t.Fatal("unsafe destination was encrypted")
		}
	}
}

func TestEncryptedLinksHandlerLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	keys := testLinkKeys(t)
	h := &Handler{Links: keys, Visitor: &fakeVisitor{}, Unsubscribes: &fakeUnsubscribes{}}
	click, _ := keys.link(linkOrigin, "click", linkToken, linkTarget)
	unsub, _ := keys.UnsubscribeLink(linkOrigin, linkToken)
	for _, link := range []string{click, unsub, click[:len(click)-3] + "AAA"} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(m, link, strings.NewReader("List-Unsubscribe=One-Click")))
		}
	}
	if buf.Len() != 0 {
		t.Fatal("handler logged")
	}
}

func TestLinkRotationKeepsOldGenerationViaPreviousSeeds(t *testing.T) {
	old, rotated := rotatedKeys(t)
	link, err := old.link(linkOrigin, "click", linkToken, linkTarget)
	if err != nil {
		t.Fatal(err)
	}
	unsub, _ := old.UnsubscribeLink(linkOrigin, linkToken)
	h := &Handler{Links: rotated}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, link, nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != linkTarget {
		t.Fatal("rotation broke old redirect", w.Code)
	}
	if id, err := rotated.verifyUnsubscribe(strings.TrimPrefix(unsub, linkOrigin)); err != nil || id != linkToken {
		t.Fatal("rotation broke old unsubscribe")
	}
	fresh, _ := rotated.link(linkOrigin, "open", linkToken, "")
	if strings.Contains(fresh, old.active) || !strings.Contains(fresh, "/"+rotated.active+"/") {
		t.Fatal("new links must use the active generation")
	}
	// Key removed: 404.
	withoutHistory, err := NewLinkKeys(bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	(&Handler{Links: withoutHistory}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, link, nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("removed key accepted", w.Code)
	}
}

func TestV1LinksNoLongerResolve(t *testing.T) {
	keys := testLinkKeys(t)
	sig := strings.Repeat("A", 86)
	for _, p := range []string{
		"/t/o/" + linkToken + "/v1/" + keys.active + "/" + sig,
		"/t/c/" + linkToken + "/v1/" + keys.active + "/" + base64.RawURLEncoding.EncodeToString([]byte(linkTarget)) + "/" + sig,
		"/t/u/" + linkToken + "/v1/" + keys.active + "/" + sig,
	} {
		v := &fakeVisitor{}
		u := &fakeUnsubscribes{}
		h := &Handler{Links: keys, Visitor: v, Unsubscribes: u}
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(m, p, strings.NewReader("List-Unsubscribe=One-Click")))
			if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed || v.calls != 0 || len(u.calls) != 0 {
				t.Fatal("v1 path resolved", p, w.Code)
			}
		}
	}
}

func TestLinkKeyIDsAndSeedValidation(t *testing.T) {
	a, _ := NewLinkKeys(seedA, nil)
	b, _ := NewLinkKeys(bytes.Repeat([]byte{1}, 32), nil)
	if a.active == b.active || len(a.active) != 32 {
		t.Fatal("key ids")
	}
	seed := seedA
	for name, previous := range map[string][][]byte{"duplicate of active": {seed}, "duplicate previous": {bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{2}, 32)}, "short": {make([]byte, 31)}, "long": {make([]byte, 33)}, "too many": make([][]byte, maxPreviousSeeds+1)} {
		if _, err := NewLinkKeys(seed, previous); err == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, bad := range [][]byte{nil, make([]byte, 31), make([]byte, 64)} {
		if _, err := NewLinkKeys(bad, nil); err == nil {
			t.Fatal("accepted bad active seed")
		}
	}
	var nilKeys *LinkKeys
	if _, err := nilKeys.UnsubscribeLink(linkOrigin, linkToken); err == nil {
		t.Fatal("nil keys issued a link")
	}
	if _, err := a.link(linkOrigin, "open", "short", ""); err == nil {
		t.Fatal("bad token")
	}
}

func TestMaxClickLinkFitsBound(t *testing.T) {
	keys := testLinkKeys(t)
	dest := "https://example.com/" + strings.Repeat("x", maxTargetBytes-len("https://example.com/"))
	link, err := keys.link(linkOrigin, "click", linkToken, dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, got, err := keys.verify(strings.TrimPrefix(link, linkOrigin)); err != nil || got != dest {
		t.Fatal("max-size click did not verify", err)
	}
	if len(strings.TrimPrefix(link, linkOrigin)) > maxLinkPathBytes {
		t.Fatal("exceeds path bound")
	}
}

func TestLinkKeyFileParsing(t *testing.T) {
	b64 := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	seed := b64(0xA1)
	path := filepath.Join(t.TempDir(), "keys.json")
	for name, data := range map[string]string{
		"empty": `{}`, "null seed": `{"seed":null}`, "bad seed": `{"seed":"bad"}`, "short seed": `{"seed":"` + base64.StdEncoding.EncodeToString(make([]byte, 31)) + `"}`,
		"dup field": `{"seed":"` + seed + `","seed":"` + seed + `"}`, "unknown": `{"seed":"` + seed + `","unknown":true}`,
		"public keys non-empty": `{"seed":"` + seed + `","previous_public_keys":["` + b64(2) + `"]}`,
		"public keys null":      `{"seed":"` + seed + `","previous_public_keys":null}`,
		"public keys string":    `{"seed":"` + seed + `","previous_public_keys":"x"}`,
		"public keys dup":       `{"seed":"` + seed + `","previous_public_keys":[],"previous_public_keys":[]}`,
		"zero seed":             `{"seed":"` + b64(0) + `"}`,
		"zero previous seed":    `{"seed":"` + seed + `","previous_seeds":["` + b64(0) + `"]}`,
		"null previous":         `{"seed":"` + seed + `","previous_seeds":null}`, "trailing": `{"seed":"` + seed + `"} {}`,
		"previous equals active": `{"seed":"` + seed + `","previous_seeds":["` + seed + `"]}`,
		"duplicate previous":     `{"seed":"` + seed + `","previous_seeds":["` + b64(1) + `","` + b64(1) + `"]}`,
		"previous bad size":      `{"seed":"` + seed + `","previous_seeds":["` + base64.StdEncoding.EncodeToString(make([]byte, 16)) + `"]}`,
		"previous not string":    `{"seed":"` + seed + `","previous_seeds":[1]}`,
		"previous not array":     `{"seed":"` + seed + `","previous_seeds":"x"}`,
		"unpadded":               `{"seed":"` + strings.TrimRight(seed, "=") + `"}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadLinkKeys(path); err == nil || strings.Contains(err.Error(), seed) {
			t.Fatal("invalid key file accepted or exposed", name)
		}
	}
	if _, err := LoadLinkKeys(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	if err := os.WriteFile(path, []byte(`{"seed":"`+b64(1)+`","previous_seeds":["`+seed+`"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadLinkKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := NewLinkKeys(seedA, nil)
	link, _ := old.link(linkOrigin, "open", linkToken, "")
	if _, _, _, err = keys.verify(strings.TrimPrefix(link, linkOrigin)); err != nil {
		t.Fatal("loaded previous seed did not decrypt", err)
	}
	if _, err = json.Marshal(keys); err == nil {
		t.Fatal("keys permitted serialization")
	}
	if fmt.Sprint(keys) != "<tracking keys redacted>" || fmt.Sprintf("%#v %+v", keys, *keys) != "<tracking keys redacted> <tracking keys redacted>" {
		t.Fatal("keys not redacted")
	}
	// Deprecated empty v1 field is tolerated (deployed secrets still carry it).
	for _, ok := range []string{`{"seed":"` + seed + `","previous_seeds":[]}`, `{"seed":"` + seed + `","previous_public_keys":[]}`, `{"previous_public_keys":[ ],"seed":"` + seed + `","previous_seeds":["` + b64(3) + `"]}`} {
		if err = os.WriteFile(path, []byte(ok), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadLinkKeys(path); err != nil {
			t.Fatal(ok, err)
		}
	}
}

func TestZeroSeedRejectedButSizingKeysWork(t *testing.T) {
	if _, err := NewLinkKeys(make([]byte, 32), nil); err == nil {
		t.Fatal("all-zero active seed accepted")
	}
	if _, err := NewLinkKeys(seedA, [][]byte{make([]byte, 32)}); err == nil {
		t.Fatal("all-zero previous seed accepted")
	}
	sizing := NewSizingLinkKeys()
	if _, err := sizing.UnsubscribeLink(linkOrigin, linkToken); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUnsubscribeOrigin(linkOrigin); err != nil {
		t.Fatal(err)
	}
}

func TestClickPlaintextIsPaddedToBlocks(t *testing.T) {
	keys := testLinkKeys(t)
	lengthOf := func(destination string) int {
		link, err := keys.link(linkOrigin, "click", linkToken, destination)
		if err != nil {
			t.Fatal(err)
		}
		return len(link)
	}
	base := "https://e.example/"
	// Destinations in the same 64-byte bucket produce identical link lengths.
	if lengthOf(base+"a") != lengthOf(base+strings.Repeat("a", 20)) {
		t.Fatal("link length leaks destination length within a block")
	}
	if lengthOf(base+"a") == lengthOf(base+strings.Repeat("a", 80)) {
		t.Fatal("padding did not grow with a larger destination")
	}
	open, _ := keys.link(linkOrigin, "open", linkToken, "")
	unsub, _ := keys.UnsubscribeLink(linkOrigin, linkToken)
	if len(open) != len(unsub) {
		t.Fatal("open and unsubscribe links differ in length")
	}
}

func TestNonCanonicalPlaintextRejected(t *testing.T) {
	keys := testLinkKeys(t)
	raw := make([]byte, 16)
	aead := keys.aeads[keys.active]
	build := func(plaintext []byte) string {
		nonce := make([]byte, aead.NonceSize())
		sealed := aead.Seal(nonce, nonce, plaintext, linkAAD("o", keys.active))
		return "/t/o/v3/" + keys.active + "/" + base64.RawURLEncoding.EncodeToString(sealed)
	}
	good := make([]byte, 64)
	copy(good, raw)
	if _, _, _, err := keys.verify(build(good)); err != nil {
		t.Fatal("canonical plaintext rejected", err)
	}
	nonZeroPad := bytes.Clone(good)
	nonZeroPad[63] = 1
	overLong := make([]byte, 128) // more padding than minimal
	badLen := bytes.Clone(good)
	badLen[17] = 200 // length beyond the plaintext
	for name, p := range map[string][]byte{"non-zero padding": nonZeroPad, "extra padding": overLong, "length overflow": badLen, "short": good[:63], "unaligned": append(bytes.Clone(good), 0)} {
		if _, _, _, err := keys.verify(build(p)); err == nil {
			t.Fatal("accepted", name)
		}
	}
}

func TestLinksAreNotBoundToTheHost(t *testing.T) {
	keys := testLinkKeys(t)
	v := &fakeVisitor{}
	h := &Handler{Links: keys, Visitor: v}
	// Minted for one origin, served at any Host (the handler ignores it).
	click, _ := keys.link("https://track.example.com", "click", linkToken, linkTarget)
	open, _ := keys.link("https://track.example.com", "open", linkToken, "")
	unsub, _ := keys.UnsubscribeLink("https://track.example.com", linkToken)
	for _, host := range []string{"track.example.com", "mail.customer.example", "203.0.113.9:8443", "[::1]", "bad host!", ""} {
		for _, link := range []string{click, open, unsub} {
			r := httptest.NewRequest(http.MethodGet, strings.TrimPrefix(link, "https://track.example.com"), nil)
			r.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusFound && w.Code != http.StatusOK {
				t.Fatal("link rejected at host", host, w.Code)
			}
		}
	}
	// Minting with a different origin only changes the URL prefix: the path
	// verifies the same way.
	other, err := keys.link("https://other.example.org", "open", linkToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, kind, _, err := keys.verify(strings.TrimPrefix(other, "https://other.example.org")); err != nil || kind != "open" {
		t.Fatal("a link minted for another origin did not verify", kind, err)
	}
	// Kind binding still holds: a click payload is not an open or unsubscribe link.
	payload := click[strings.LastIndex(click, "/")+1:]
	for _, letter := range []string{"o", "u"} {
		p := "/t/" + letter + "/v3/" + keys.active + "/" + payload
		if _, _, _, err := keys.verify(p); err == nil && letter == "o" {
			t.Fatal("click payload accepted as open")
		}
		if _, err := keys.verifyUnsubscribe(p); err == nil && letter == "u" {
			t.Fatal("click payload accepted as unsubscribe")
		}
	}
}

func TestLegacyV2LinksAreNotAccepted(t *testing.T) {
	keys := testLinkKeys(t)
	link, _ := keys.link(linkOrigin, "open", linkToken, "")
	old := strings.Replace(strings.TrimPrefix(link, linkOrigin), "/v3/", "/v2/", 1)
	if _, _, _, err := keys.verify(old); err == nil {
		t.Fatal("v2 link accepted")
	}
}
