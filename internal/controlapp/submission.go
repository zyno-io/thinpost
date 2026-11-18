package controlapp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// SubmissionFingerprintKeys supports an ordered JSON array of base64 keys.
// The first key signs new fingerprints; the rest verify existing records.
// The legacy single-key file remains supported for coordinated migration.
func SubmissionFingerprintKeys(getenv func(string) string) ([][]byte, error) {
	path := getenv("THINPOST_CONTROL_FINGERPRINT_KEYS_FILE")
	if path == "" {
		key, err := SubmissionKey(getenv)
		if err != nil || key == nil {
			return nil, err
		}
		return [][]byte{key}, nil
	}
	if getenv("THINPOST_CONTROL_FINGERPRINT_KEY_FILE") != "" {
		return nil, errors.New("set only one fingerprint key file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open fingerprint keyring")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid fingerprint keyring file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errors.New("invalid fingerprint keyring")
	}
	var encoded []string
	if err = json.Unmarshal(data, &encoded); err != nil || len(encoded) == 0 || len(encoded) > 8 {
		return nil, errors.New("invalid fingerprint keyring")
	}
	keys := make([][]byte, len(encoded))
	for i, value := range encoded {
		keys[i], err = base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(keys[i]) < 32 || len(keys[i]) > 1024 {
			return nil, errors.New("invalid fingerprint keyring")
		}
	}
	return keys, nil
}

// SubmissionKey returns the single fingerprint key, or nil when no key file is
// configured (the submission API is then off). The shared secret must survive
// the idempotency window.
func SubmissionKey(getenv func(string) string) ([]byte, error) {
	path := getenv("THINPOST_CONTROL_FINGERPRINT_KEY_FILE")
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid fingerprint key file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open fingerprint key")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid fingerprint key file")
	}
	key, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || len(key) < 32 || len(key) > 1024 {
		return nil, errors.New("invalid fingerprint key")
	}
	return key, nil
}
