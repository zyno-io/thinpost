package admin

import (
	"net/http"

	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// webhooks serves /apps/{appId}/webhooks. Responses and audit records carry
// no endpoint URL or secret except the one-time secret in IssuedWebhook.
func (h *Handler) webhooks(w http.ResponseWriter, r *http.Request, actor, appID string) {
	if h.Webhooks == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := h.Webhooks.ListWebhooks(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		if items == nil {
			items = []Webhook{}
		}
		respond(w, http.StatusOK, map[string]any{"webhooks": items})
	case http.MethodPost:
		var in WebhookInput
		endpoint, ok := "", false
		if decode(r, &in) == nil {
			var internal bool
			endpoint, internal, ok = h.WebhookDestinations.NormalizeEndpoint(in.Endpoint)
			if in.Format == "" {
				in.Format = format.Thinpost
			}
			ok = ok && (!internal || InternalEndpointPermitted(in.Format, in.Auth.hasCredentials()))
		}
		if in.Format == "" {
			in.Format = format.Thinpost
		}
		_, known := format.Lookup(in.Format)
		if !ok || !known || !validWebhookDescription(in.Description) || len(in.Events) > 0 && !format.ValidEvents(in.Format, in.Events) ||
			in.Auth != nil && !ValidWebhookAuth(*in.Auth) {
			fail(w, http.StatusBadRequest)
			return
		}
		in.Endpoint = endpoint
		issued, err := h.Webhooks.CreateWebhook(r.Context(), actor, appID, in)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusCreated, issued)
	default:
		method(w, "GET, POST")
	}
}

func validWebhookDescription(s string) bool { return s == "" || safeText(s, 256) }

func noBody(r *http.Request) bool {
	return r.URL.RawQuery == "" && r.ContentLength <= 0 && len(r.TransferEncoding) == 0
}

// webhook serves /apps/{appId}/webhooks/{id} and its rotate-secret and test actions.
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request, actor, appID, id, action string) {
	if h.Webhooks == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	switch action {
	case "":
		switch r.Method {
		case http.MethodPatch:
			h.updateWebhook(w, r, actor, appID, id)
		case http.MethodDelete:
			if !noBody(r) {
				fail(w, http.StatusBadRequest)
				return
			}
			if err := h.Webhooks.DeleteWebhook(r.Context(), actor, appID, id); err != nil {
				storeError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			method(w, "PATCH, DELETE")
		}
	case "rotate-secret", "test":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		if !noBody(r) {
			fail(w, http.StatusBadRequest)
			return
		}
		if action == "test" {
			result, err := h.Webhooks.TestWebhook(r.Context(), actor, appID, id)
			if err != nil {
				storeError(w, err)
				return
			}
			respond(w, http.StatusOK, result)
			return
		}
		issued, err := h.Webhooks.RotateWebhookSecret(r.Context(), actor, appID, id)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, issued)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) updateWebhook(w http.ResponseWriter, r *http.Request, actor, appID, id string) {
	var patch WebhookPatch
	if decode(r, &patch) != nil || patch.Endpoint == nil && patch.Description == nil && patch.Events == nil && patch.Active == nil && patch.Auth == nil {
		fail(w, http.StatusBadRequest)
		return
	}
	if patch.Endpoint != nil {
		endpoint, internal, ok := h.WebhookDestinations.NormalizeEndpoint(*patch.Endpoint)
		// The row's format and stored credentials are checked by the Store.
		if !ok || internal && patch.Auth.hasCredentials() {
			fail(w, http.StatusBadRequest)
			return
		}
		patch.Endpoint = &endpoint
	}
	if patch.Description != nil && !validWebhookDescription(*patch.Description) || patch.Events != nil && len(*patch.Events) > 0 && !ValidWebhookEventNames(*patch.Events) || patch.Auth != nil && !ValidWebhookAuth(*patch.Auth) {
		fail(w, http.StatusBadRequest)
		return
	}
	item, err := h.Webhooks.UpdateWebhook(r.Context(), actor, appID, id, patch)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, item)
}
