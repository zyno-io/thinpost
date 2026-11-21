package controlbridge

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/egress"
	"github.com/zyno-io/thinpost/internal/journal"
)

// FromEnvironment leaves the bridge disabled unless a target is explicitly
// configured. All listed fields are required when it is enabled; secrets are
// read from mounted files and never included in returned error messages.
func FromEnvironment(getenv func(string) string) (*Config, error) {
	target := getenv("THINPOST_CONTROL_EDGE_TARGET")
	if target == "" {
		for _, key := range []string{"THINPOST_CONTROL_WORKER_ID", "THINPOST_CONTROL_JOURNAL_ID", "THINPOST_CONTROL_INSTANCE_ID", "THINPOST_CONTROL_IDENTITY", "THINPOST_CONTROL_EDGE_DNS", "THINPOST_CONTROL_EDGE_IDENTITY", "THINPOST_CONTROL_BOUNCE_ACTIVE_KEY", "THINPOST_CONTROL_BOUNCE_KEYS_FILE", "THINPOST_CONTROL_INFRASTRUCTURE_CIDRS", "THINPOST_CONTROL_ROUTE_EXCEPTIONS_FILE", "THINPOST_CONTROL_CLIENT_CERT", "THINPOST_CONTROL_CLIENT_KEY", "THINPOST_CONTROL_PRIVATE_CA"} {
			if getenv(key) != "" {
				return nil, errors.New("incomplete control bridge configuration")
			}
		}
		return nil, nil
	}
	c := &Config{Target: target, WorkerID: getenv("THINPOST_CONTROL_WORKER_ID"), JournalID: getenv("THINPOST_CONTROL_JOURNAL_ID"), ControllerID: getenv("THINPOST_CONTROL_INSTANCE_ID"), ControllerIdentity: getenv("THINPOST_CONTROL_IDENTITY"), WorkerDNS: getenv("THINPOST_CONTROL_EDGE_DNS"), WorkerIdentity: getenv("THINPOST_CONTROL_EDGE_IDENTITY"), ActiveBounceKey: getenv("THINPOST_CONTROL_BOUNCE_ACTIVE_KEY"), LeaseTTL: 60 * time.Second, ClockMargin: 5 * time.Second, PollInterval: 3 * time.Second}
	if c.WorkerID == "" || !journal.ValidIdentity(c.JournalID) || c.ControllerID == "" || c.ControllerIdentity == "" || c.WorkerDNS == "" || c.WorkerIdentity == "" || c.ActiveBounceKey == "" {
		return nil, errors.New("incomplete control bridge configuration")
	}
	// THINPOST_CONTROL_SMTP_SOURCE and THINPOST_CONTROL_BOUNCE_DOMAIN from an
	// earlier release are never read: the Edge reports its own addresses and
	// names. They may stay set while new pods roll out.
	var err error
	for _, value := range strings.Split(getenv("THINPOST_CONTROL_INFRASTRUCTURE_CIDRS"), ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, errors.New("invalid infrastructure CIDR")
		}
		c.Infrastructure = append(c.Infrastructure, prefix)
	}
	c.RouteExceptions, err = egress.LoadExceptions(getenv("THINPOST_CONTROL_ROUTE_EXCEPTIONS_FILE"))
	if err != nil {
		return nil, err
	}
	c.ClientCertificate, err = tls.LoadX509KeyPair(getenv("THINPOST_CONTROL_CLIENT_CERT"), getenv("THINPOST_CONTROL_CLIENT_KEY"))
	if err != nil {
		return nil, errors.New("cannot load control client certificate")
	}
	ca, err := os.ReadFile(getenv("THINPOST_CONTROL_PRIVATE_CA"))
	if err != nil {
		return nil, errors.New("cannot load private control CA")
	}
	c.PrivateCA = x509.NewCertPool()
	if !c.PrivateCA.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid private control CA")
	}
	c.BounceKeys, err = readKeys(getenv("THINPOST_CONTROL_BOUNCE_KEYS_FILE"))
	if err != nil {
		return nil, errors.New("cannot load bounce keyring")
	}
	return c, nil
}
func readKeys(path string) (map[string][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errors.New("invalid keyring")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid keyring")
	}
	keys := make(map[string][]byte)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid keyring")
		}
		version, ok := token.(string)
		if !ok || version == "" || len(version) > 64 || len(keys) >= 32 {
			return nil, errors.New("invalid keyring")
		}
		if _, exists := keys[version]; exists {
			return nil, errors.New("duplicate key version")
		}
		var encoded string
		if err = decoder.Decode(&encoded); err != nil {
			return nil, errors.New("invalid keyring")
		}
		key, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || len(key) < 32 {
			return nil, errors.New("invalid keyring")
		}
		keys[version] = key
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(keys) == 0 {
		return nil, errors.New("invalid keyring")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid keyring")
	}
	return keys, nil
}
