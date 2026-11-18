package egress

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/mail"
	"net/netip"
	"os"
	"strings"

	"github.com/zyno-io/thinpost/internal/admin"
)

const maxExceptions = 128

// Exception is an operator-approved SMTP route, not a general IP allowlist.
type Exception struct {
	Recipient string
	MX        string
	Address   netip.Addr
}

type exceptionKey struct {
	recipient string
	mx        string
	address   netip.Addr
}

func (e Exception) key() (exceptionKey, bool) {
	if strings.ContainsAny(e.Recipient, "\r\n") {
		return exceptionKey{}, false
	}
	mailbox, err := mail.ParseAddress(e.Recipient)
	if err != nil || mailbox.Address != e.Recipient {
		return exceptionKey{}, false
	}
	at := strings.LastIndexByte(e.Recipient, '@')
	if at <= 0 || at == len(e.Recipient)-1 {
		return exceptionKey{}, false
	}
	domain, ok := admin.NormalizeDomain(e.Recipient[at+1:])
	if !ok {
		return exceptionKey{}, false
	}
	mx, ok := admin.NormalizeDomain(e.MX)
	if !ok || !e.Address.IsValid() || e.Address.Zone() != "" || e.Address.Is4In6() {
		return exceptionKey{}, false
	}
	return exceptionKey{recipient: e.Recipient[:at] + "@" + domain, mx: mx, address: e.Address}, true
}

// LoadExceptions reads an optional bounded file of exact recipient, MX name,
// and numeric IP triples, one per line. It returns generic errors so addresses
// are not exposed through startup logs. A configured empty file is invalid.
func LoadExceptions(path string) ([]Exception, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open SMTP route exceptions")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return nil, errors.New("invalid SMTP route exceptions")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errors.New("invalid SMTP route exceptions")
	}
	var exceptions []Exception
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || len(exceptions) >= maxExceptions {
			return nil, errors.New("invalid SMTP route exceptions")
		}
		address, err := netip.ParseAddr(fields[2])
		if err != nil {
			return nil, errors.New("invalid SMTP route exceptions")
		}
		exception := Exception{Recipient: fields[0], MX: fields[1], Address: address}
		if _, ok := exception.key(); !ok {
			return nil, errors.New("invalid SMTP route exceptions")
		}
		exceptions = append(exceptions, exception)
	}
	if scanner.Err() != nil || len(exceptions) == 0 {
		return nil, errors.New("invalid SMTP route exceptions")
	}
	return exceptions, nil
}
