package tracking

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/submission"
)

// UnsubscribeRecorder commits a one-click unsubscribe for a delivery. It
// returns ErrNotFound when the delivery or its recorded recipient is missing,
// and nil for the first request and for every duplicate. Any other error means
// the outcome is unknown or the runtime gate is closed (reported as 503). It is
// called only after the link payload has decrypted. Errors must carry no
// address, delivery or app identity.
type UnsubscribeRecorder interface {
	RecordUnsubscribe(context.Context, string) error
}

const (
	// maxUnsubscribeBody bounds the POST body (RFC 8058 sends 25 bytes).
	maxUnsubscribeBody = 1024
	unsubscribeTimeout = 5 * time.Second
	unsubscribeCSP     = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'"
	unsubscribeField   = "List-Unsubscribe"
	unsubscribeValue   = "One-Click"
)

const unsubscribePageHead = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Unsubscribe</title>` +
	`<style>body{font:16px/1.5 system-ui,sans-serif;max-width:28rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a}button{font:inherit;padding:.5rem 1.25rem;cursor:pointer}</style></head><body>`

// The pages hold no address, app name, subject or identifier.
const (
	unsubscribeConfirmPage = unsubscribePageHead + `<h1>Unsubscribe</h1><p>Select the button to stop receiving these emails.</p>` +
		`<form method="post"><input type="hidden" name="List-Unsubscribe" value="One-Click"><button type="submit">Unsubscribe</button></form></body></html>`
	unsubscribeDonePage    = unsubscribePageHead + `<h1>You're unsubscribed.</h1><p>You will not receive these emails again.</p></body></html>`
	unsubscribeMissingPage = unsubscribePageHead + `<h1>This link is no longer valid.</h1></body></html>`
	unsubscribeErrorPage   = unsubscribePageHead + `<h1>Something went wrong.</h1><p>Your request was not recorded. Try again later.</p></body></html>`
	unsubscribeBadPage     = unsubscribePageHead + `<h1>Request not understood.</h1></body></html>`
)

// ValidateUnsubscribeOrigin reports whether a one-click unsubscribe link on the
// origin is a valid List-Unsubscribe header value. NormalizeOrigin already
// admits only ASCII hosts; this remains as a defense in depth so a future
// change to origin validation cannot emit an invalid header.
func ValidateUnsubscribeOrigin(origin string) error {
	link, err := NewSizingLinkKeys().UnsubscribeLink(origin, strings.Repeat("0", 32))
	if err != nil || submission.ValidateListUnsubscribe("<"+link+">", true) != nil {
		return errors.New("tracking origin cannot carry a one-click unsubscribe link")
	}
	return nil
}

func isUnsubscribePath(path string) bool { return strings.HasPrefix(path, "/t/u/") }

func unsubscribePage(w http.ResponseWriter, r *http.Request, status int, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(page)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, page)
	}
}

// serveUnsubscribe handles /t/u/... The payload is decrypted before the
// method-specific work and before any storage access. GET and HEAD never change
// state: mail scanners prefetch links. Only a POST with the RFC 8058 body does.
// Nothing here logs, and no response echoes the path.
func (h *Handler) serveUnsubscribe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", unsubscribeCSP)
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		unsubscribePage(w, r, http.StatusNotFound, unsubscribeMissingPage)
		return
	}
	deliveryID, err := h.Links.verifyUnsubscribe(r.URL.Path)
	if err != nil {
		unsubscribePage(w, r, http.StatusNotFound, unsubscribeMissingPage)
		return
	}
	if r.Method != http.MethodPost {
		unsubscribePage(w, r, http.StatusOK, unsubscribeConfirmPage)
		return
	}
	if status := checkUnsubscribeBody(w, r); status != 0 {
		unsubscribePage(w, r, status, unsubscribeBadPage)
		return
	}
	if h.Unsubscribes == nil {
		unsubscribePage(w, r, http.StatusServiceUnavailable, unsubscribeErrorPage)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), unsubscribeTimeout)
	defer cancel()
	switch err = h.Unsubscribes.RecordUnsubscribe(ctx, deliveryID); {
	case err == nil:
		unsubscribePage(w, r, http.StatusOK, unsubscribeDonePage)
	case errors.Is(err, ErrNotFound):
		unsubscribePage(w, r, http.StatusNotFound, unsubscribeMissingPage)
	default:
		// Gate closed or database unavailable: unlike opens, correctness
		// matters, and mailbox providers and people retry.
		unsubscribePage(w, r, http.StatusServiceUnavailable, unsubscribeErrorPage)
	}
}

// checkUnsubscribeBody requires exactly the field List-Unsubscribe=One-Click,
// form-urlencoded or multipart, in at most 1 KiB. It returns 0 when valid.
func checkUnsubscribeBody(w http.ResponseWriter, r *http.Request) int {
	if r.ContentLength > maxUnsubscribeBody {
		return http.StatusRequestEntityTooLarge
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return http.StatusUnsupportedMediaType
	}
	body := http.MaxBytesReader(w, r.Body, maxUnsubscribeBody)
	switch mediaType {
	case "application/x-www-form-urlencoded":
		data, readErr := io.ReadAll(body)
		if readErr != nil {
			return http.StatusRequestEntityTooLarge
		}
		values, parseErr := url.ParseQuery(string(data))
		if parseErr != nil || len(values) != 1 || len(values[unsubscribeField]) != 1 || values[unsubscribeField][0] != unsubscribeValue {
			return http.StatusBadRequest
		}
		return 0
	case "multipart/form-data":
		if params["boundary"] == "" {
			return http.StatusBadRequest
		}
		reader := multipart.NewReader(body, params["boundary"])
		found := false
		for {
			part, nextErr := reader.NextPart()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil || found || part.FormName() != unsubscribeField || part.FileName() != "" {
				return http.StatusBadRequest
			}
			value, readErr := io.ReadAll(io.LimitReader(part, int64(len(unsubscribeValue))+1))
			if readErr != nil || string(value) != unsubscribeValue {
				return http.StatusBadRequest
			}
			found = true
		}
		if !found {
			return http.StatusBadRequest
		}
		return 0
	}
	return http.StatusUnsupportedMediaType
}
