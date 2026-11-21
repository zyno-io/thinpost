package tracking

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newChallenge(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hostCheckGet(h http.Handler, method, target, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if host != "" {
		r.Host = host
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHostCheckEndpoint(t *testing.T) {
	keys := testLinkKeys(t)
	h := &Handler{Links: keys}
	c := newChallenge(t)
	w := hostCheckGet(h, http.MethodGet, "/t/h/v1/"+c, "anything.example")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("bad response", w.Code, w.Header())
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "thinpost-host-check v1 "+keys.active+" ") || !keys.VerifyHostCheck(c, body) {
		t.Fatal("proof invalid", body)
	}
	// Host independent.
	if w2 := hostCheckGet(h, http.MethodGet, "/t/h/v1/"+c, "other.example"); w2.Body.String() != body {
		t.Fatal("proof depends on Host")
	}
	// HEAD: same headers, no body.
	if w := hostCheckGet(h, http.MethodHead, "/t/h/v1/"+c, ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD", w.Code, w.Body.Len())
	}
	if w := hostCheckGet(h, http.MethodPost, "/t/h/v1/"+c, ""); w.Code != 405 {
		t.Fatal("POST", w.Code)
	}
	for name, target := range map[string]string{
		"query": "/t/h/v1/" + c + "?a=b", "empty query": "/t/h/v1/" + c + "?", "short": "/t/h/v1/" + c[:42], "long": "/t/h/v1/" + c + "A",
		"padded": "/t/h/v1/" + c[:42] + "A=", "not base64": "/t/h/v1/" + strings.Repeat("!", 43), "empty": "/t/h/v1/", "extra segment": "/t/h/v1/" + c + "/x",
		"noncanonical": "/t/h/v1/" + c[:42] + "B",
	} {
		w := hostCheckGet(h, http.MethodGet, target, "")
		if w.Code != 404 || w.Body.Len() != 0 {
			// c[:42]+"B" may coincide with c; skip only that case.
			if name == "noncanonical" && c[42:] == "B" {
				continue
			}
			t.Fatal("accepted", name, w.Code)
		}
	}
}

func TestHostCheckPreviousSeedAndSizing(t *testing.T) {
	old, rotated := rotatedKeys(t)
	c := newChallenge(t)
	oldBody, err := old.HostCheckProof(c)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.VerifyHostCheck(c, oldBody) {
		t.Fatal("previous seed proof rejected")
	}
	newBody, _ := rotated.HostCheckProof(c)
	if old.VerifyHostCheck(c, newBody) {
		t.Fatal("unknown key id accepted")
	}
	sizing := NewSizingLinkKeys()
	if _, err := sizing.HostCheckProof(c); err == nil {
		t.Fatal("sizing key produced a proof")
	}
	if w := hostCheckGet(&Handler{Links: sizing}, http.MethodGet, "/t/h/v1/"+c, ""); w.Code != 404 {
		t.Fatal("sizing handler served a proof", w.Code)
	}
	if w := hostCheckGet(&Handler{}, http.MethodGet, "/t/h/v1/"+c, ""); w.Code != 404 {
		t.Fatal("no keys", w.Code)
	}
	var nilKeys *LinkKeys
	if nilKeys.VerifyHostCheck(c, oldBody) || sizing.VerifyHostCheck(c, oldBody) {
		t.Fatal("verified without keys")
	}
}

func TestVerifyHostCheck(t *testing.T) {
	keys := testLinkKeys(t)
	c := newChallenge(t)
	good, _ := keys.HostCheckProof(c)
	if !keys.VerifyHostCheck(c, good) || !keys.VerifyHostCheck(c, good+"\n") {
		t.Fatal("good proof rejected")
	}
	fields := strings.Split(good, " ")
	mac := fields[3]
	flipped := []byte(mac)
	if flipped[0] == 'A' {
		flipped[0] = 'B'
	} else {
		flipped[0] = 'A'
	}
	for name, body := range map[string]string{
		"wrong key id":  strings.Join([]string{fields[0], fields[1], strings.Repeat("0", 32), mac}, " "),
		"tampered mac":  strings.Join([]string{fields[0], fields[1], fields[2], string(flipped)}, " "),
		"truncated mac": good[:len(good)-1],
		"wrong version": strings.Replace(good, " v1 ", " v2 ", 1),
		"extra field":   good + " x",
		"empty":         "",
	} {
		if keys.VerifyHostCheck(c, body) {
			t.Fatal("accepted", name)
		}
	}
	if keys.VerifyHostCheck(newChallenge(t), good) {
		t.Fatal("proof accepted for another challenge")
	}
	if keys.VerifyHostCheck("short", good) {
		t.Fatal("malformed challenge accepted")
	}
}
