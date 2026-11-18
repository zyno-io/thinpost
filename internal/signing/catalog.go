package signing

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type catalogEntry struct {
	AppID    string `json:"app_id"`
	Domain   string `json:"domain"`
	Selector string `json:"selector"`
	KeyPath  string `json:"key_path"`
}

// LoadCatalog reads an operator-managed catalog and mounted unencrypted RSA
// private key files. Paths and key bytes are never returned in errors. Rotation
// requires a coordinated restart and DNS overlap; this is not a key manager.
func LoadCatalog(path string) ([]Entry, error) {
	data, err := readBounded(path, 256<<10)
	if err != nil {
		return nil, errors.New("cannot read signing catalog")
	}
	var records []catalogEntry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&records); err != nil || len(records) == 0 || len(records) > 1024 {
		return nil, errors.New("invalid signing catalog")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid signing catalog")
	}
	entries := make([]Entry, 0, len(records))
	for _, record := range records {
		if !filepath.IsAbs(record.KeyPath) {
			return nil, errors.New("invalid signing key path")
		}
		encoded, err := readBounded(record.KeyPath, 64<<10)
		if err != nil {
			return nil, errors.New("cannot read signing key")
		}
		block, rest := pem.Decode(encoded)
		if block == nil || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) > 0 {
			return nil, errors.New("invalid signing key")
		}
		var parsed any
		switch block.Type {
		case "PRIVATE KEY":
			parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		default:
			return nil, errors.New("unsupported signing key")
		}
		if err != nil {
			return nil, errors.New("invalid signing key")
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok || key.Validate() != nil {
			return nil, errors.New("unsupported signing key")
		}
		var signer crypto.Signer = key
		entries = append(entries, Entry{AppID: record.AppID, Domain: record.Domain, Selector: record.Selector, Signer: signer})
	}
	return entries, nil
}
func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, nil
}
