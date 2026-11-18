package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/submission"
)

type attachmentFixtureService struct {
	requests []submission.Request
}

func (f *attachmentFixtureService) Authenticate(_ context.Context, key string) (submission.App, error) {
	if key != "fixture-key" {
		return submission.App{}, submission.ErrInvalidKey
	}
	return submission.App{ID: "fixture-app"}, nil
}

func (f *attachmentFixtureService) Accept(_ context.Context, _ submission.App, request submission.Request) (submission.Receipt, error) {
	f.requests = append(f.requests, request)
	return submission.Receipt{ID: "fixture-message", AcceptedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, nil
}

func TestRuntimeAcceptsAttachmentsWithoutFlag(t *testing.T) {
	values := map[string]string{
		"THINPOST_DATABASE_URL":          "postgres://fixture@db.example/thinpost?sslmode=verify-full",
		"THINPOST_DEPLOYMENT_GENERATION": "00000000000000000000000000000001",
		"THINPOST_OIDC_ISSUER":           "https://issuer.example", "THINPOST_OIDC_AUDIENCE": "api://thinpost",
		"THINPOST_OIDC_JWKS_URL": "https://issuer.example/keys", "THINPOST_OIDC_ADMIN_SCOPE": "admin",
		"THINPOST_OIDC_ADMIN_ROLE": "Admin", "THINPOST_OIDC_CLIENT_ID": "client",
		"THINPOST_OIDC_SCOPES": "openid api://thinpost/admin",
	}
	config, err := controlapp.Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	service := &attachmentFixtureService{}
	request := httptest.NewRequest(http.MethodPost, "/emails", strings.NewReader(`{"from":"a@example.com","to":"b@example.net","subject":"test","text":"body","attachments":[{"filename":"fixture.txt","content":"eA=="}]}`))
	request.Header.Set("Authorization", "Bearer fixture-key")
	response := httptest.NewRecorder()
	submissionHandler(service, config, nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(service.requests) != 1 || len(service.requests[0].Message.Attachments) != 1 || string(service.requests[0].Message.Attachments[0].Content) != "x" {
		t.Fatal("runtime did not preserve attachment", response.Code)
	}
}

func TestRuntimeTrackingConfigurationControlsAdmission(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		config := controlapp.Config{}
		if enabled {
			config.Tracking = &controlapp.TrackingConfig{}
		}
		service := &attachmentFixtureService{}
		request := httptest.NewRequest(http.MethodPost, "/email", strings.NewReader(`{"From":"a@example.com","To":"b@example.net","Subject":"tracked","HtmlBody":"<a href=\"https://example.net/\">link</a>","TrackOpens":true,"TrackLinks":"HtmlOnly"}`))
		request.Header.Set("X-Postmark-Server-Token", "fixture-key")
		response := httptest.NewRecorder()
		submissionHandler(service, config, nil).ServeHTTP(response, request)
		if !enabled {
			if response.Code != http.StatusBadRequest || len(service.requests) != 0 {
				t.Fatal("disabled runtime accepted tracking", response.Code)
			}
			continue
		}
		if response.Code != http.StatusOK || len(service.requests) != 1 || service.requests[0].Message.Tracking == nil || !service.requests[0].Message.Tracking.Opens || !service.requests[0].Message.Tracking.Clicks {
			t.Fatal("enabled runtime lost tracking options", response.Code)
		}
	}
}
