package admin

import (
	"context"
	"net/http"
)

// AppStore renames and deletes apps, and answers the shared "does this app
// exist for the console" question every app-scoped route asks first.
type AppStore interface {
	// AppExists is false for an unknown or deleted app.
	AppExists(ctx context.Context, appID string) (bool, error)
	// RenameApp changes the name and audits it (old and new name) in the same
	// transaction. Renaming to the current name is a no-op that audits nothing.
	// An unknown or deleted app is ErrNotFound.
	RenameApp(ctx context.Context, actor, appID, name string) (App, error)
	// DeleteApp soft-deletes the app in one transaction: it marks it deleted
	// and inactive, revokes its API keys and SMTP credentials, removes its
	// receiving domains and audits app.deleted. An unknown or already deleted
	// app is ErrNotFound. Messages, events and audit rows are kept.
	DeleteApp(ctx context.Context, actor, appID string) error
}

// appRoute serves PATCH and DELETE /api/v1/apps/{appId}.
func (h *Handler) appRoute(w http.ResponseWriter, r *http.Request, actor, appID string) {
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Name string `json:"name"`
		}
		if decode(r, &req) != nil || !safeText(req.Name, 128) {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.Apps == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		app, err := h.Apps.RenameApp(r.Context(), actor, appID, req.Name)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, app)
	case http.MethodDelete:
		if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.Apps == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		if err := h.Apps.DeleteApp(r.Context(), actor, appID); err != nil {
			storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		method(w, "PATCH, DELETE")
	}
}
