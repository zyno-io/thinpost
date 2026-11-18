package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

// Bounds mirror the schema 28 CHECK constraints. Everything is validated before
// any INSERT so a violation, which PostgreSQL would log together with the
// failing row, cannot occur. Values here are never logged or put in errors.
const (
	maxRecordedSubjectBytes = 998
	maxRecordedAddressBytes = 998
	maxRecordedDomainBytes  = 253
	maxRecordedRecipients   = 100
	maxRecordedSenderName   = 998
)

// validRecordedSubject reports whether a subject satisfies the schema bounds.
func validRecordedSubject(subject string) bool {
	return len(subject) >= 1 && len(subject) <= maxRecordedSubjectBytes && utf8.ValidString(subject) && !strings.ContainsAny(subject, "\x00\r\n")
}

// validRecordedSender reports whether the From address and display name satisfy
// the schema 31 bounds and agree with the recorded domain.
func validRecordedSender(address, name, domain string) bool {
	if len(address) < 3 || len(address) > maxRecordedAddressBytes || !utf8.ValidString(address) || strings.IndexByte(address, 0) >= 0 {
		return false
	}
	at := strings.LastIndexByte(address, '@')
	if at < 1 || strings.ToLower(address[at+1:]) != domain {
		return false
	}
	return len(name) <= maxRecordedSenderName && utf8.ValidString(name) && !strings.ContainsAny(name, "\x00\r\n")
}

// nullableName is the column value of a display name: NULL when empty.
func nullableName(name string) any {
	if name == "" {
		return nil
	}
	return name
}

// reverseDomainLabels reverses the dot-separated labels of a domain and appends
// a trailing dot: mail.example.com becomes "com.example.mail.". "This domain or
// any subdomain" is then the single prefix match LIKE 'com.example.%'. Label
// reversal preserves the byte length (plus the trailing dot). The insert path
// and the search path must both use this helper.
func reverseDomainLabels(domain string) string {
	labels := strings.Split(domain, ".")
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	return strings.Join(labels, ".") + "."
}

// validRecordedDomain checks a stored sender domain.
func validRecordedDomain(domain string) bool {
	return len(domain) >= 1 && len(domain) <= maxRecordedDomainBytes && utf8.ValidString(domain) && strings.IndexByte(domain, 0) < 0
}

// recipientKeys derives the lowercased lookup forms of an envelope address.
// Lowercasing is done in Go so it does not depend on the database locale.
func recipientKeys(address string) (lower, domain, domainRev string, ok bool) {
	if len(address) < 3 || len(address) > maxRecordedAddressBytes || !utf8.ValidString(address) || strings.IndexByte(address, 0) >= 0 {
		return "", "", "", false
	}
	lower = strings.ToLower(address)
	at := strings.LastIndexByte(lower, '@')
	if len(lower) > maxRecordedAddressBytes || at < 1 || at == len(lower)-1 {
		return "", "", "", false
	}
	domain = lower[at+1:]
	return lower, domain, reverseDomainLabels(domain), true
}

type recipientRows struct {
	messageIDs, addresses, lowers, domains, revs, kinds []string
	indexes                                             []int32
}

// add validates recipients for one message and appends them. kinds is parallel
// to recipients: "to" first, then "cc", then "bcc" (never out of that order).
func (rows *recipientRows) add(messageID string, recipients, kinds []string, count int) bool {
	if len(recipients) != count || len(kinds) != count || count < 1 || count > maxRecordedRecipients {
		return false
	}
	rank := 0
	for i, address := range recipients {
		var kindRank int
		switch kinds[i] {
		case "to":
			kindRank = 0
		case "cc":
			kindRank = 1
		case "bcc":
			kindRank = 2
		default:
			return false
		}
		if i == 0 && kindRank != 0 || kindRank < rank {
			return false
		}
		rank = kindRank
		lower, domain, rev, ok := recipientKeys(address)
		if !ok {
			return false
		}
		rows.messageIDs = append(rows.messageIDs, messageID)
		rows.indexes = append(rows.indexes, int32(i))
		rows.addresses = append(rows.addresses, address)
		rows.lowers = append(rows.lowers, lower)
		rows.domains = append(rows.domains, domain)
		rows.revs = append(rows.revs, rev)
		rows.kinds = append(rows.kinds, kinds[i])
	}
	return true
}

// insertRecipients stores every row in one statement inside the submission
// transaction. The composite foreign key ties each row to the app that owns
// the message, which keeps search tenant-isolated.
func insertRecipients(ctx context.Context, tx *sql.Tx, appID string, rows *recipientRows) error {
	if len(rows.messageIDs) == 0 {
		return errors.New("no recipients to record")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind)
		SELECT t.m,$1,t.i,t.a,t.l,t.d,t.r,t.k FROM unnest($2::text[],$3::int4[],$4::text[],$5::text[],$6::text[],$7::text[],$8::text[]) AS t(m,i,a,l,d,r,k)`,
		appID, rows.messageIDs, rows.indexes, rows.addresses, rows.lowers, rows.domains, rows.revs, rows.kinds)
	return err
}
