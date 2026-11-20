// Package recipientpolicy provides an explicit envelope-recipient safeguard.
package recipientpolicy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/mail"
	"os"
	"strings"
)

// Policy is immutable after loading and may be shared by concurrent requests.
type Policy struct {
	unrestricted bool
	allowed      map[string]struct{}
}

// Unrestricted reports whether the policy allows any valid recipient.
func (p *Policy) Unrestricted() bool { return p != nil && p.unrestricted }

// Mode is "unrestricted" or "allowlist", the two values of
// THINPOST_RECIPIENT_POLICY, and is empty for a nil policy.
func (p *Policy) Mode() string {
	switch {
	case p == nil:
		return ""
	case p.unrestricted:
		return "unrestricted"
	}
	return "allowlist"
}

// Load requires an explicit mode. An allowlist is a bounded text file of exact
// mailbox addresses, one per line; comments beginning with # are permitted.
// Local parts remain case-sensitive and DNS names compare case-insensitively.
func Load(getenv func(string) string) (*Policy, error) {
	mode, path := getenv("THINPOST_RECIPIENT_POLICY"), getenv("THINPOST_RECIPIENT_ALLOWLIST_FILE")
	switch mode {
	case "unrestricted":
		if path != "" {
			return nil, errors.New("recipient allowlist is incompatible with unrestricted mode")
		}
		return &Policy{unrestricted: true}, nil
	case "allowlist":
		if path == "" {
			return nil, errors.New("recipient allowlist file is required")
		}
	default:
		return nil, errors.New("explicit recipient policy is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open recipient allowlist")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, errors.New("invalid recipient allowlist")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("invalid recipient allowlist")
	}
	allowed := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, ok := addressKey(line)
		if !ok || len(allowed) >= 512 {
			return nil, errors.New("invalid recipient allowlist")
		}
		allowed[key] = struct{}{}
	}
	if scanner.Err() != nil || len(allowed) == 0 {
		return nil, errors.New("invalid recipient allowlist")
	}
	return &Policy{allowed: allowed}, nil
}

func addressKey(value string) (string, bool) {
	if strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address == "" {
		return "", false
	}
	at := strings.LastIndexByte(address.Address, '@')
	if at <= 0 || at == len(address.Address)-1 {
		return "", false
	}
	return address.Address[:at] + "@" + strings.ToLower(address.Address[at+1:]), true
}

// Allows tests the parsed SMTP envelope mailbox, ignoring any display name.
func (p *Policy) Allows(value string) bool {
	if p == nil {
		return false
	}
	key, ok := addressKey(value)
	if !ok {
		return false
	}
	if p.unrestricted {
		return true
	}
	_, ok = p.allowed[key]
	return ok
}
