package admin

import (
	"context"
	"net/netip"
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
	KeepOrigin       bool    `json:"keep_origin"`
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

// SMTPConnection tells clients where to connect. Host comes only from
// operator-set sending-IP rows (never from an Edge report); PrimaryUnnamed is
// set when the app uses the primary address and no row names it.
type SMTPConnection struct {
	Host           *string    `json:"host,omitempty"`
	HostSource     *string    `json:"host_source"`
	PrimaryUnnamed bool       `json:"primary_unnamed"`
	Ports          []SMTPPort `json:"ports"`
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
	// SMTPConnection resolves the host: the app's assigned sending IP, else the
	// sending-IP row whose address is primary.
	SMTPConnection(ctx context.Context, appID string, primary netip.Addr) (SMTPConnection, error)
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
		if p.Domain != nil || p.Address != nil || p.KeepOrigin {
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
		out.Domain, out.KeepOrigin = &domain, p.KeepOrigin
	case SMTPFromAddress:
		if p.Address == nil || p.Domain != nil || p.KeepOrigin {
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
