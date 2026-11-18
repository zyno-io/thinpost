package controlapp

import (
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// configuredDatabaseURL accepts either one complete URL or a credential-free
// base URL with two projected files. It never includes file contents in errors.
func configuredDatabaseURL(getenv func(string) string) (string, error) {
	raw := getenv("THINPOST_DATABASE_URL")
	base := getenv("THINPOST_DATABASE_BASE_URL")
	usernamePath := getenv("THINPOST_DATABASE_USERNAME_FILE")
	passwordPath := getenv("THINPOST_DATABASE_PASSWORD_FILE")
	if base == "" && usernamePath == "" && passwordPath == "" {
		return raw, nil
	}
	if raw != "" || base == "" || !filepath.IsAbs(usernamePath) || !filepath.IsAbs(passwordPath) {
		return "", errors.New("incomplete database credential configuration")
	}
	connection, err := url.Parse(base)
	if err != nil || connection.User != nil {
		return "", errors.New("invalid database base URL")
	}
	username, err := readDatabaseCredential(usernamePath, 256)
	if err != nil {
		return "", errors.New("cannot load database username")
	}
	password, err := readDatabaseCredential(passwordPath, 1024)
	if err != nil {
		return "", errors.New("cannot load database password")
	}
	connection.User = url.UserPassword(username, password)
	return connection.String(), nil
}

func readDatabaseCredential(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("credential is not a regular file")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(value) == 0 || int64(len(value)) > limit || !utf8.Valid(value) || strings.IndexByte(string(value), 0) >= 0 {
		return "", errors.New("invalid credential")
	}
	return string(value), nil
}
