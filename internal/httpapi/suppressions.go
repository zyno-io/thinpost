package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
)

// Provider-compatible suppression APIs: the Postmark message-stream routes and
// Resend's GET /suppressions. Addresses only ever travel in request bodies and
// response bodies. Routes that carry an address in the URL path or query
// (Postmark dump?EmailAddress=, Resend get/remove by address) are
// deliberately not implemented because request URLs are logged by ingress.

const (
	postmarkStream           = "outbound"
	postmarkSuppressionBatch = 50
	postmarkDumpCap          = 10000
	postmarkInactiveMessage  = "You tried to send to recipient(s) that have been marked as inactive."
	postmarkFailedAuthority  = "You do not have the required authority to change this suppression."
	postmarkFailedAddress    = "An invalid email address was provided."
)

func postmarkError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]any{"ErrorCode": code, "Message": message})
}

func (s *Server) serveSuppressions(w http.ResponseWriter, r *http.Request) {
	provider := "postmark"
	if r.URL.Path == "/suppressions" {
		provider = "resend"
	}
	var route, stream string
	if provider == "postmark" {
		// /message-streams/{stream}/suppressions[/dump|/delete]
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 4 || len(parts) > 5 || parts[0] != "" || parts[1] != "message-streams" || parts[2] == "" || parts[3] != "suppressions" {
			fail(w, provider, http.StatusNotFound, "not found")
			return
		}
		stream, route = parts[2], "add"
		if len(parts) == 5 {
			route = parts[4]
			if route != "dump" && route != "delete" {
				fail(w, provider, http.StatusNotFound, "not found")
				return
			}
		}
		want := http.MethodPost
		if route == "dump" {
			want = http.MethodGet
		}
		if r.Method != want {
			w.Header().Set("Allow", want)
			fail(w, provider, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	} else if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		fail(w, provider, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.service == nil || s.suppressions == nil {
		fail(w, provider, http.StatusServiceUnavailable, "suppressions unavailable")
		return
	}
	key, ok := credential(r, provider)
	if !ok {
		fail(w, provider, http.StatusUnauthorized, "invalid API key")
		return
	}
	app, err := s.service.Authenticate(r.Context(), key)
	if err != nil || app.ID == "" {
		if errors.Is(err, submission.ErrInvalidKey) || err == nil {
			fail(w, provider, http.StatusUnauthorized, "invalid API key")
		} else {
			fail(w, provider, http.StatusServiceUnavailable, "authentication unavailable")
		}
		return
	}
	if provider == "resend" {
		s.resendSuppressionList(w, r, app)
		return
	}
	if stream != postmarkStream {
		postmarkError(w, http.StatusUnprocessableEntity, 1221, "The message stream was not found; only the outbound stream is supported.")
		return
	}
	switch route {
	case "dump":
		s.postmarkSuppressionDump(w, r, app)
	case "add", "delete":
		s.postmarkSuppressionChange(w, r, app, route == "delete")
	}
}

var postmarkReasons = map[string][]string{
	"HardBounce":        {"hard_bounce"},
	"SpamComplaint":     {"spam_complaint"},
	"ManualSuppression": {"manual", "unsubscribe"},
}

func postmarkReasonName(reason string) string {
	switch reason {
	case "hard_bounce":
		return "HardBounce"
	case "spam_complaint":
		return "SpamComplaint"
	default:
		return "ManualSuppression"
	}
}

func postmarkOriginName(origin string) string {
	if origin == admin.SuppressionOriginRecipient {
		return "Recipient"
	}
	return "Customer"
}

// singleQuery matches query names case-insensitively, because the SDKs
// disagree on casing (the Postmark SDK sends suppressionReason, fromDate and
// count). Keys are returned under the canonical name; two keys that
// canonicalize alike count as a repeat.
func singleQuery(r *http.Request, allowed ...string) (url.Values, string) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, "invalid query"
	}
	out := url.Values{}
	for name, v := range values {
		canonical := ""
		for _, a := range allowed {
			if strings.EqualFold(a, name) {
				canonical = a
			}
		}
		if canonical == "" {
			// Never echo the name or value: they could be an address.
			return nil, "unsupported query parameter"
		}
		if len(v) != 1 || out.Has(canonical) {
			return nil, "repeated query parameter"
		}
		out[canonical] = v
	}
	return out, ""
}

func (s *Server) postmarkSuppressionDump(w http.ResponseWriter, r *http.Request, app submission.App) {
	for name := range r.URL.Query() {
		if !strings.EqualFold(name, "EmailAddress") {
			continue
		}
		postmarkError(w, http.StatusUnprocessableEntity, 1409, "Filtering by EmailAddress is not supported; addresses are never accepted in URLs. Page through the dump instead.")
		return
	}
	// count and offset are sent by the SDK's getSuppressions; the dump is not
	// paged, so they are accepted and ignored.
	values, bad := singleQuery(r, "SuppressionReason", "Origin", "fromdate", "todate", "count", "offset")
	if bad != "" {
		postmarkError(w, http.StatusUnprocessableEntity, 1409, bad)
		return
	}
	filter := submission.ProviderSuppressionFilter{Limit: postmarkDumpCap + 1}
	if v := values.Get("SuppressionReason"); v != "" {
		reasons, ok := postmarkReasons[v]
		if !ok {
			postmarkError(w, http.StatusUnprocessableEntity, 1404, "Parameter 'SuppressionReason' is invalid.")
			return
		}
		filter.Reasons = reasons
	}
	empty := false
	switch values.Get("Origin") {
	case "":
	case "Recipient":
		filter.Origins = []string{admin.SuppressionOriginRecipient}
	case "Customer":
		filter.Origins = []string{admin.SuppressionOriginAPI, admin.SuppressionOriginConsole}
	case "Admin":
		// Thinpost has no Postmark-admin origin.
		empty = true
	default:
		postmarkError(w, http.StatusUnprocessableEntity, 1405, "Parameter 'Origin' is invalid.")
		return
	}
	if v := values.Get("fromdate"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			postmarkError(w, http.StatusUnprocessableEntity, 1409, "Parameter 'fromdate' must be YYYY-MM-DD.")
			return
		}
		filter.From = t
	}
	if v := values.Get("todate"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			postmarkError(w, http.StatusUnprocessableEntity, 1409, "Parameter 'todate' must be YYYY-MM-DD.")
			return
		}
		filter.To = t.AddDate(0, 0, 1)
	}
	out := []map[string]any{}
	if !empty {
		items, _, err := s.suppressions.ListProviderSuppressions(r.Context(), app.ID, filter)
		if err != nil {
			fail(w, "postmark", http.StatusServiceUnavailable, "suppressions unavailable")
			return
		}
		if len(items) > postmarkDumpCap {
			postmarkError(w, http.StatusUnprocessableEntity, 1407, "More than 10000 suppressions match; narrow the range with fromdate and todate.")
			return
		}
		for _, item := range items {
			out = append(out, map[string]any{"EmailAddress": item.Address, "SuppressionReason": postmarkReasonName(item.Reason), "Origin": postmarkOriginName(item.Origin), "CreatedAt": item.CreatedAt.UTC().Format(time.RFC3339)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"Suppressions": out})
}

// suppressionWriteFailure answers a failed provider write. A key or app that
// went inactive is the same 401 the send path gives at commit.
func suppressionWriteFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, submission.ErrInvalidKey) {
		fail(w, "postmark", http.StatusUnauthorized, "invalid API key")
		return
	}
	fail(w, "postmark", http.StatusServiceUnavailable, "suppressions unavailable")
}

func (s *Server) postmarkSuppressionChange(w http.ResponseWriter, r *http.Request, app submission.App, remove bool) {
	if r.URL.RawQuery != "" {
		postmarkError(w, http.StatusUnprocessableEntity, 1409, "Query parameters are not supported.")
		return
	}
	var body struct {
		Suppressions []struct {
			EmailAddress string `json:"EmailAddress"`
		} `json:"Suppressions"`
	}
	if decodeJSON(r, &body, limits{body: maxBody}) != nil || len(body.Suppressions) == 0 {
		postmarkError(w, http.StatusUnprocessableEntity, 1409, "A proper request body must be provided.")
		return
	}
	if len(body.Suppressions) > postmarkSuppressionBatch {
		postmarkError(w, http.StatusUnprocessableEntity, 1410, "You cannot provide more than 50 suppressions for this request.")
		return
	}
	type item struct {
		address string
		valid   bool
	}
	items := make([]item, len(body.Suppressions))
	var valid []string
	for i, entry := range body.Suppressions {
		address, ok := admin.NormalizeSuppressionAddress(entry.EmailAddress)
		items[i] = item{entry.EmailAddress, ok}
		if ok {
			items[i].address = address
			valid = append(valid, address)
		}
	}
	removable := map[string]bool{}
	if len(valid) > 0 {
		if remove {
			results, err := s.suppressions.DeleteProviderSuppressions(r.Context(), app, valid)
			if err != nil {
				suppressionWriteFailure(w, err)
				return
			}
			for i, address := range valid {
				removable[strings.ToLower(address)] = removable[strings.ToLower(address)] || results[i]
			}
		} else if err := s.suppressions.AddProviderSuppressions(r.Context(), app, valid); err != nil {
			suppressionWriteFailure(w, err)
			return
		}
	}
	okStatus := "Suppressed"
	if remove {
		okStatus = "Deleted"
	}
	out := make([]map[string]any, len(items))
	for i, it := range items {
		entry := map[string]any{"EmailAddress": body.Suppressions[i].EmailAddress, "Status": okStatus, "Message": nil}
		switch {
		case !it.valid:
			entry["Status"], entry["Message"] = "Failed", postmarkFailedAddress
		case remove && !removable[strings.ToLower(it.address)]:
			entry["Status"], entry["Message"] = "Failed", postmarkFailedAuthority
		}
		out[i] = entry
	}
	writeJSON(w, http.StatusOK, map[string]any{"Suppressions": out})
}

func (s *Server) resendSuppressionList(w http.ResponseWriter, r *http.Request, app submission.App) {
	values, bad := singleQuery(r, "limit", "after", "before", "origin")
	if bad != "" {
		fail(w, "resend", http.StatusUnprocessableEntity, bad)
		return
	}
	filter := submission.ProviderSuppressionFilter{Limit: 20}
	if v := values.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			fail(w, "resend", http.StatusUnprocessableEntity, "limit must be between 1 and 100")
			return
		}
		filter.Limit = n
	}
	switch values.Get("origin") {
	case "":
	case "bounce":
		filter.Reasons = []string{"hard_bounce"}
	case "complaint":
		filter.Reasons = []string{"spam_complaint"}
	case "manual":
		filter.Reasons = []string{"manual", "unsubscribe"}
	default:
		fail(w, "resend", http.StatusUnprocessableEntity, "origin must be bounce, complaint or manual")
		return
	}
	if values.Get("after") != "" && values.Get("before") != "" {
		fail(w, "resend", http.StatusUnprocessableEntity, "after and before cannot be combined")
		return
	}
	for name, dest := range map[string]*string{"after": &filter.After, "before": &filter.Before} {
		if v := values.Get(name); v != "" {
			id, ok := providerid.Normalize(v)
			if !ok {
				fail(w, "resend", http.StatusUnprocessableEntity, name+" is not a valid suppression id")
				return
			}
			*dest = id
		}
	}
	items, more, err := s.suppressions.ListProviderSuppressions(r.Context(), app.ID, filter)
	if errors.Is(err, submission.ErrInvalidMessage) {
		fail(w, "resend", http.StatusUnprocessableEntity, "cursor suppression not found")
		return
	}
	if err != nil {
		fail(w, "resend", http.StatusServiceUnavailable, "suppressions unavailable")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		origin := "manual"
		switch item.Reason {
		case "hard_bounce":
			origin = "bounce"
		case "spam_complaint":
			origin = "complaint"
		}
		var source any
		if item.SourceMessageID != "" {
			source = providerid.Resend(item.SourceMessageID)
		}
		data = append(data, map[string]any{"id": providerid.Resend(item.ID), "email": item.Address, "origin": origin, "source_id": source, "created_at": item.CreatedAt.UTC().Format(time.RFC3339Nano)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "has_more": more, "data": data})
}
