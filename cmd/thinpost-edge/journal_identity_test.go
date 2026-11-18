package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/journal"
)

func TestJournalIdentityCommandInitializesAndReusesIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	var first, second bytes.Buffer
	if err := printJournalIdentity(path, &first); err != nil {
		t.Fatal(err)
	}
	if err := printJournalIdentity(path, &second); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(first.String())
	if !journal.ValidIdentity(id) || first.String() != second.String() {
		t.Fatal("journal identity was not stable")
	}
}
