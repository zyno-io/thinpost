package postgres

import (
	"testing"

	"github.com/zyno-io/thinpost/internal/admin"
)

func TestSearchAcceptsEveryMessageIDForm(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	noSubject := func() (string, bool, error) { return "", false, nil }
	for _, q := range []string{id, "01234567-89ab-cdef-0123-456789abcdef", "01234567-89AB-CDEF-0123-456789ABCDEF", id + "@example.com", "<" + id + "@example.com>"} {
		for _, field := range []string{admin.SearchFieldID, admin.SearchFieldAny} {
			p, err := buildSearchPatterns(field, q, noSubject)
			if err != nil || p.idPrefix == nil || *p.idPrefix != id {
				t.Errorf("%s %q: %v %v", field, q, p.idPrefix, err)
			}
		}
	}
	// Partial IDs remain bare hex prefixes.
	if p, err := buildSearchPatterns(admin.SearchFieldID, "0123abcd", noSubject); err != nil || p.idPrefix == nil || *p.idPrefix != "0123abcd%" {
		t.Error("prefix search changed")
	}
	// A non-ID in the ID field is still invalid.
	if _, err := buildSearchPatterns(admin.SearchFieldID, "01234567-89ab-cdef-0123", noSubject); err == nil {
		t.Error("partial UUID text accepted")
	}
}
