package admin

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// SMTP From policy modes.
const (
	SMTPFromOff     = "off"
	SMTPFromDomain  = "domain"
	SMTPFromAddress = "address"
)

// SMTP connection host sources.
const (
	SMTPHostAppSendingIP = "app_sending_ip"
	SMTPHostPrimary      = "primary"
)

// SMTP connection host statuses.
const (
	// SMTPHostOK: the host is the HELO name of the IP the app sends from.
	SMTPHostOK = "ok"
	// SMTPHostNotReported: the sending worker has not reported the IP, so no
	// host is known yet.
	SMTPHostNotReported = "not_reported"
	// SMTPHostIPUnavailable: the app's chosen IP is no longer on the sending
	// worker, so no host is offered.
	SMTPHostIPUnavailable = "ip_unavailable"
)

// SMTPCredential is one SMTP login of an app. Username is what the client
// sends as the SMTP user name; the password is never readable after creation.
type SMTPCredential struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Username   string     `json:"username"`
	Active     bool       `json:"active"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// IssuedSMTPCredential is returned once, at creation.
type IssuedSMTPCredential struct {
	Credential SMTPCredential `json:"credential"`
	Username   string         `json:"username"`
	Password   string         `json:"password"`
}

// SMTPFromPolicy decides the From address of mail submitted over SMTP. Only
// the fields of the chosen mode are set; Domain and Address are normalized.
type SMTPFromPolicy struct {
	Mode             string  `json:"mode"`
	Domain           *string `json:"domain,omitempty"`
	Address          *string `json:"address,omitempty"`
	OnlyUnauthorized bool    `json:"only_unauthorized"`
}

// SMTPSigningReady reports whether mail from Domain would be DKIM-signed right
// now.
type SMTPSigningReady struct {
	Domain string `json:"domain"`
	Ready  bool   `json:"ready"`
}

// SMTPSettings is the From policy. SigningReady lists the domain the policy
// targets; it is null when this deployment has no signing catalog and empty
// when the policy targets no domain.
type SMTPSettings struct {
	From         SMTPFromPolicy     `json:"from"`
	SigningReady []SMTPSigningReady `json:"signing_ready"`
}

// SMTPPort is a submission port clients can use.
type SMTPPort struct {
	Port     int    `json:"port"`
	Security string `json:"security"`
}

// SMTPConnection tells clients where to connect. Host is the HELO name of the
// IP the app sends from, as the sending worker last reported it; HostStatus
// says why there is none when it is absent.
type SMTPConnection struct {
	Host       *string    `json:"host,omitempty"`
	HostSource *string    `json:"host_source"`
	HostStatus string     `json:"host_status"`
	Ports      []SMTPPort `json:"ports"`
	// ObservedPorts are the submission ports the Edge last reported listening
	// on. They only drive an availability hint.
	ObservedPorts []int      `json:"observed_ports"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
}

// AppSMTP is the whole SMTP tab of an app.
type AppSMTP struct {
	Settings    SMTPSettings     `json:"settings"`
	Credentials []SMTPCredential `json:"credentials"`
	Connection  SMTPConnection   `json:"connection"`
}

// SMTPStore persists credentials and the From policy. Every mutation records
// its audit event in the same transaction. SetSMTPSettings is idempotent: an
// unchanged policy changes and audits nothing, and a domain that is not an
// active verified sender domain of the app is ErrConflict. IssueSMTPCredential
// returns ErrConflict at the active-credential limit.
type SMTPStore interface {
	GetSMTPSettings(ctx context.Context, appID string) (SMTPFromPolicy, error)
	SetSMTPSettings(ctx context.Context, actor, appID string, policy SMTPFromPolicy) (SMTPFromPolicy, error)
	ListSMTPCredentials(ctx context.Context, appID string) ([]SMTPCredential, error)
	IssueSMTPCredential(ctx context.Context, actor, appID, name string) (IssuedSMTPCredential, error)
	RevokeSMTPCredential(ctx context.Context, actor, appID, credentialID string) error
	// SMTPConnection resolves the host: the app's chosen sending IP, else the
	// main IP of the Edge's last report.
	SMTPConnection(ctx context.Context, appID string) (SMTPConnection, error)
}

// SMTPPorts are the fixed submission ports: 465 wraps the connection in TLS
// and 587 upgrades with STARTTLS.
func SMTPPorts() []SMTPPort {
	return []SMTPPort{{Port: 465, Security: "tls"}, {Port: 587, Security: "starttls"}}
}

// NormalizeSMTPFromPolicy checks the shape of a policy and returns it with only
// the fields of its mode set. The configured address must be an ASCII dot-atom
// local part with a normalized domain.
func NormalizeSMTPFromPolicy(p SMTPFromPolicy) (SMTPFromPolicy, bool) {
	out := SMTPFromPolicy{Mode: p.Mode, OnlyUnauthorized: p.OnlyUnauthorized}
	switch p.Mode {
	case SMTPFromOff:
		if p.Domain != nil || p.Address != nil {
			return SMTPFromPolicy{}, false
		}
	case SMTPFromDomain:
		if p.Domain == nil || p.Address != nil {
			return SMTPFromPolicy{}, false
		}
		domain, ok := NormalizeDomain(*p.Domain)
		if !ok || domain != *p.Domain {
			return SMTPFromPolicy{}, false
		}
		out.Domain = &domain
	case SMTPFromAddress:
		if p.Address == nil || p.Domain != nil {
			return SMTPFromPolicy{}, false
		}
		address, ok := normalizeSMTPAddress(*p.Address)
		if !ok || address != *p.Address {
			return SMTPFromPolicy{}, false
		}
		out.Address = &address
	default:
		return SMTPFromPolicy{}, false
	}
	return out, true
}

// SMTPAddressDomain is the domain part of a normalized configured address.
func SMTPAddressDomain(address string) string {
	return address[strings.LastIndexByte(address, '@')+1:]
}

func normalizeSMTPAddress(value string) (string, bool) {
	at := strings.LastIndexByte(value, '@')
	if at < 1 || len(value) > 254 || at > 64 {
		return "", false
	}
	local := value[:at]
	for _, atom := range strings.Split(local, ".") {
		if atom == "" {
			return "", false
		}
		for i := 0; i < len(atom); i++ {
			c := atom[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+/=?^_`{|}~-", c) >= 0) {
				return "", false
			}
		}
	}
	domain, ok := NormalizeDomain(value[at+1:])
	if !ok {
		return "", false
	}
	return local + "@" + domain, true
}

// SigningReadiness is the slice of the signing resolver the SMTP tab needs.
// Ready returns nil when mail from the domain would be DKIM-signed now.
type SigningReadiness interface {
	Ready(ctx context.Context, appID, domain string) error
}

// smtpRoute serves /api/v1/apps/{appId}/smtp...; it reports whether it handled
// the request.
func (h *Handler) smtpRoute(w http.ResponseWriter, r *http.Request, actor, appID string, path []string) bool {
	if len(path) < 6 || path[5] != "smtp" {
		return false
	}
	store, ok := h.Store.(SMTPStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return true
	}
	switch {
	case len(path) == 6:
		if r.Method != http.MethodGet {
			method(w, "GET")
			return true
		}
		h.getAppSMTP(w, r, store, appID)
	case len(path) == 7 && path[6] == "settings":
		if r.Method != http.MethodPut {
			method(w, "PUT")
			return true
		}
		h.updateAppSMTPSettings(w, r, store, actor, appID)
	case len(path) == 7 && path[6] == "credentials":
		if r.Method != http.MethodPost {
			method(w, "POST")
			return true
		}
		var request struct {
			Name string `json:"name"`
		}
		if decode(r, &request) != nil || !safeText(request.Name, 128) {
			fail(w, http.StatusBadRequest)
			return true
		}
		issued, err := store.IssueSMTPCredential(r.Context(), actor, appID, request.Name)
		if err != nil {
			storeError(w, err)
			return true
		}
		respond(w, http.StatusCreated, issued)
	case len(path) == 8 && path[6] == "credentials" && opaqueID(path[7]):
		if r.Method != http.MethodDelete {
			method(w, "DELETE")
			return true
		}
		if err := store.RevokeSMTPCredential(r.Context(), actor, appID, path[7]); err != nil {
			storeError(w, err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
	return true
}

func (h *Handler) getAppSMTP(w http.ResponseWriter, r *http.Request, store SMTPStore, appID string) {
	policy, err := store.GetSMTPSettings(r.Context(), appID)
	if err != nil {
		storeError(w, err)
		return
	}
	credentials, err := store.ListSMTPCredentials(r.Context(), appID)
	if err != nil {
		storeError(w, err)
		return
	}
	connection, err := store.SMTPConnection(r.Context(), appID)
	if err != nil {
		storeError(w, err)
		return
	}
	if credentials == nil {
		credentials = []SMTPCredential{}
	}
	respond(w, http.StatusOK, AppSMTP{Settings: h.smtpSettings(r.Context(), appID, policy), Credentials: credentials, Connection: connection})
}

func (h *Handler) updateAppSMTPSettings(w http.ResponseWriter, r *http.Request, store SMTPStore, actor, appID string) {
	var request struct {
		From *struct {
			Mode             string  `json:"mode"`
			Domain           *string `json:"domain"`
			Address          *string `json:"address"`
			OnlyUnauthorized *bool   `json:"only_unauthorized"`
		} `json:"from"`
	}
	if decode(r, &request) != nil || request.From == nil || request.From.OnlyUnauthorized == nil {
		fail(w, http.StatusBadRequest)
		return
	}
	policy, ok := NormalizeSMTPFromPolicy(SMTPFromPolicy{
		Mode: request.From.Mode, Domain: request.From.Domain,
		Address: request.From.Address, OnlyUnauthorized: *request.From.OnlyUnauthorized,
	})
	if !ok {
		fail(w, http.StatusBadRequest)
		return
	}
	stored, err := store.SetSMTPSettings(r.Context(), actor, appID, policy)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, h.smtpSettings(r.Context(), appID, stored))
}

// smtpSettings adds the signing check for the domain the policy targets. It is
// null without a signing catalog and empty when the policy targets no domain.
func (h *Handler) smtpSettings(ctx context.Context, appID string, policy SMTPFromPolicy) SMTPSettings {
	settings := SMTPSettings{From: policy}
	if h.SigningReadiness == nil {
		return settings
	}
	settings.SigningReady = []SMTPSigningReady{}
	target := ""
	switch {
	case policy.Domain != nil:
		target = *policy.Domain
	case policy.Address != nil:
		target = SMTPAddressDomain(*policy.Address)
	}
	if target != "" {
		settings.SigningReady = append(settings.SigningReady, SMTPSigningReady{Domain: target, Ready: h.SigningReadiness.Ready(ctx, appID, target) == nil})
	}
	return settings
}
