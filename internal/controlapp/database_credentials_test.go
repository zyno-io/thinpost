package controlapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestProjectedDatabaseCredentials(t *testing.T) {
	dir := t.TempDir()
	userFile, passFile := filepath.Join(dir, "username"), filepath.Join(dir, "password")
	user, password := "runtime+user", " p@ss:word/%+ "
	if err := os.WriteFile(userFile, []byte(user), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passFile, []byte(password), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"THINPOST_DATABASE_BASE_URL":      "postgres://db.example/thinpost?sslmode=verify-full&default_query_exec_mode=exec",
		"THINPOST_DATABASE_USERNAME_FILE": userFile,
		"THINPOST_DATABASE_PASSWORD_FILE": passFile,
	}
	get := func(key string) string { return values[key] }
	raw, err := DatabaseURL(get)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pgx.ParseConfig(raw)
	if err != nil || parsed.User != user || parsed.Password != password || parsed.Host != "db.example" || parsed.TLSConfig == nil || parsed.TLSConfig.InsecureSkipVerify {
		t.Fatal("credentials or transport changed during URL construction", err)
	}
	for _, mutate := range []func(){
		func() { values["THINPOST_DATABASE_URL"] = "postgres://db.example/thinpost?sslmode=verify-full" },
		func() {
			values["THINPOST_DATABASE_BASE_URL"] = "postgres://embedded:secret@db.example/thinpost?sslmode=verify-full"
		},
		func() {
			values["THINPOST_DATABASE_BASE_URL"] = "postgres://db.example/thinpost?sslmode=verify-full&ssl=true"
		},
		func() { values["THINPOST_DATABASE_USERNAME_FILE"] = "relative" },
		func() { values["THINPOST_DATABASE_PASSWORD_FILE"] = dir },
	} {
		copyValues := make(map[string]string, len(values))
		for k, v := range values {
			copyValues[k] = v
		}
		mutate()
		if _, err := DatabaseURL(get); err == nil || strings.Contains(err.Error(), password) {
			t.Fatal("invalid credential configuration accepted or exposed")
		}
		values = copyValues
	}
	if err := os.WriteFile(passFile, []byte(strings.Repeat("a", 1025)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DatabaseURL(get); err == nil {
		t.Fatal("oversized credential accepted")
	}
}
