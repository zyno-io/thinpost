package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/providerid"
)

// Search terms are secret operator input. Nothing in this file logs them,
// wraps them into an error or writes them to an audit row; every failure is a
// sentinel from the admin package.

var searchHexID = regexp.MustCompile(`^[0-9a-f]{8,32}$`)

// likeEscape escapes the LIKE metacharacters so user input matches literally.
// Queries use ESCAPE '\'.
func likeEscape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// searchPatterns is the set of per-branch parameters. A nil entry disables
// its branch.
type searchPatterns struct {
	idPrefix        *string // message and delivery ID prefix (hex, no metacharacters)
	recipientAddr   *string // address_lower LIKE
	recipientDomain *string // domain_lower LIKE
	recipientRev    *string // domain_rev LIKE
	senderRev       *string // sender_domain_rev LIKE
	subjectQuery    *string // tsquery text
}

func strPtr(value string) *string { return &value }

// normalizeSearchDomain lowercases, strips one leading "@" and one trailing
// ".", and requires a plain dotted name: 1-253 bytes, no whitespace, no "@",
// no empty labels.
func normalizeSearchDomain(q string) (string, bool) {
	domain := strings.ToLower(q)
	domain = strings.TrimPrefix(domain, "@")
	domain = strings.TrimSuffix(domain, ".")
	if len(domain) < 1 || len(domain) > 253 || !utf8.ValidString(domain) || strings.ContainsFunc(domain, func(r rune) bool { return unicode.IsSpace(r) || r == '@' }) {
		return "", false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" {
			return "", false
		}
	}
	return domain, true
}

// domainPattern converts a normalized domain into the reversed-label prefix
// pattern: example.com matches com.example. and com.example.mail. but not
// com.badexample.
func domainPattern(domain string) string {
	return likeEscape(reverseDomainLabels(domain)) + "%"
}

func searchTextBound(q string) bool {
	if len(q) < 1 || len(q) > 254 || !utf8.ValidString(q) || strings.TrimSpace(q) != q {
		return false
	}
	for _, c := range q {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// buildSearchPatterns derives the enabled branches for a field. words reports
// whether the subject query has at least one word; subjectQuery is supplied
// lazily because words come from the database.
func buildSearchPatterns(field, q string, subjectQuery func() (string, bool, error)) (searchPatterns, error) {
	var p searchPatterns
	lower := strings.ToLower(q)
	hasSpace := strings.ContainsFunc(q, unicode.IsSpace)
	wantID := field == admin.SearchFieldID || field == admin.SearchFieldAny
	wantRecipient := field == admin.SearchFieldRecipient || field == admin.SearchFieldAny
	wantRecipientDomain := field == admin.SearchFieldRecipientDomain || field == admin.SearchFieldAny
	wantSenderDomain := field == admin.SearchFieldSenderDomain || field == admin.SearchFieldAny
	wantSubject := field == admin.SearchFieldSubject || field == admin.SearchFieldAny
	isAny := field == admin.SearchFieldAny

	// A full ID in any provider rendering (UUID text, "id@domain", "<id@domain>")
	// is the same message ID; partial IDs stay bare hex prefixes.
	idQuery := lower
	if id, ok := providerid.Normalize(q); ok {
		idQuery = id
	}
	if wantID && searchHexID.MatchString(idQuery) {
		if len(idQuery) == 32 {
			p.idPrefix = strPtr(idQuery)
		} else {
			p.idPrefix = strPtr(idQuery + "%")
		}
	}
	if wantRecipient && len(q) >= 2 && !hasSpace {
		switch at := strings.IndexByte(lower, '@'); {
		case at == 0:
			p.recipientDomain = strPtr(likeEscape(lower[1:]) + "%")
		case at > 0:
			p.recipientAddr = strPtr(likeEscape(lower) + "%")
		default:
			p.recipientAddr = strPtr(likeEscape(lower) + "%")
			p.recipientDomain = strPtr(likeEscape(lower) + "%")
		}
	}
	if wantRecipientDomain || wantSenderDomain {
		domain, ok := normalizeSearchDomain(q)
		if ok && (!isAny || strings.Contains(domain, ".")) {
			pattern := strPtr(domainPattern(domain))
			if wantRecipientDomain {
				p.recipientRev = pattern
			}
			if wantSenderDomain {
				p.senderRev = pattern
			}
		}
	}
	if wantSubject {
		query, ok, err := subjectQuery()
		if err != nil {
			return p, err
		}
		if ok {
			p.subjectQuery = strPtr(query)
		}
	}
	if p == (searchPatterns{}) {
		return p, admin.ErrInvalidInput
	}
	return p, nil
}

// tsquery builds `'lex':* & ...` from lexemes that PostgreSQL itself produced.
func tsqueryFromLexemes(lexemes []string) string {
	escaper := strings.NewReplacer(`\`, `\\`, `'`, `''`)
	parts := make([]string, len(lexemes))
	for i, lexeme := range lexemes {
		parts[i] = "'" + escaper.Replace(lexeme) + "':*"
	}
	return strings.Join(parts, " & ")
}

// searchSQL is one constant statement. Each branch is guarded by its own
// parameter, which custom plans fold away, returns its newest limit+1 rows
// after the cursor, and the union keeps the overall newest limit+1. That is
// exact: every row of the overall top N is within the top N of each branch
// that matches it. Recipient and delivery-ID branches start from messages and
// use EXISTS so that a message matching many rows is counted once.
const searchCursor = `($2::timestamptz IS NULL OR (m.accepted_at,m.id)<($2::timestamptz,$3::text))`

var searchSQL = `WITH cand AS (SELECT u.accepted_at,u.id FROM (
	(SELECT m.accepted_at,m.id FROM messages m
		WHERE $5::text IS NOT NULL AND m.app_id=$1 AND m.id LIKE $5 AND ` + searchCursor + `
		ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4)
	UNION
	(SELECT m.accepted_at,m.id FROM messages m
		WHERE $5::text IS NOT NULL AND m.app_id=$1 AND ` + searchCursor + `
		AND EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id=m.id AND d.id LIKE $5)
		ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4)
	UNION
	(SELECT m.accepted_at,m.id FROM messages m
		WHERE $6::text IS NOT NULL AND m.app_id=$1 AND ` + searchCursor + `
		AND EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id=m.id AND r.app_id=$1 AND r.address_lower LIKE $6 ESCAPE '\')
		ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4)
	UNION
	(SELECT m.accepted_at,m.id FROM messages m
		WHERE $7::text IS NOT NULL AND m.app_id=$1 AND ` + searchCursor + `
		AND EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id=m.id AND r.app_id=$1 AND r.domain_lower LIKE $7 ESCAPE '\')
		ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4)
	UNION
	(SELECT m.accepted_at,m.id FROM messages m
		WHERE $8::text IS NOT NULL AND m.app_id=$1 AND ` + searchCursor + `
		AND EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id=m.id AND r.app_id=$1 AND r.domain_rev LIKE $8 ESCAPE '\')
		ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4)
	UNION
	(SELECT m.accepted_at,m.id FROM messages m
		WHERE $9::text IS NOT NULL AND m.app_id=$1 AND m.sender_domain_rev IS NOT NULL AND m.sender_domain_rev LIKE $9 ESCAPE '\' AND ` + searchCursor + `
		ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4)
	UNION
	(SELECT m.accepted_at,m.id FROM messages m
		WHERE $10::text IS NOT NULL AND m.app_id=$1 AND m.subject IS NOT NULL AND NOT m.enhanced_protection
		AND to_tsvector('simple'::regconfig, regexp_replace(m.subject, '[^[:alnum:]]+', ' ', 'g')) @@ $10::text::tsquery AND ` + searchCursor + `
		ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4)
	) u ORDER BY u.accepted_at DESC,u.id DESC LIMIT $4)
SELECT %SUMMARY% FROM messages m LEFT JOIN deliveries d ON d.message_id=m.id
WHERE m.app_id=$1 AND m.id IN (SELECT id FROM cand)
GROUP BY m.id ORDER BY m.accepted_at DESC,m.id DESC`

// SearchMessages returns newest-first history filtered by the query. It is
// not audited. EP subjects never match: the subject index is partial on
// NOT enhanced_protection and the subject branch repeats that predicate.
func (r *Repository) SearchMessages(ctx context.Context, actor, appID string, query admin.MessageQuery) (admin.MessagePage, error) {
	var page admin.MessagePage
	if !adminText(actor, 256) || !opaqueID(appID) || query.Limit < 1 || query.Limit > 100 || !admin.ValidSearchField(query.Field) || !searchTextBound(query.Text) {
		return page, admin.ErrInvalidInput
	}
	cursor, err := parseHistoryCursor(query.Cursor)
	if err != nil {
		return page, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	// Prefix LIKE only uses text_pattern_ops indexes under per-execution plans,
	// and an unbounded search must not hold a connection.
	if _, err = tx.ExecContext(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`); err != nil {
		return page, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
		return page, err
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return page, admin.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	patterns, err := buildSearchPatterns(query.Field, query.Text, func() (string, bool, error) {
		return subjectTSQuery(ctx, tx, query.Text)
	})
	if err != nil {
		return page, err
	}
	var before any
	if !cursor.At.IsZero() {
		before = cursor.At
	}
	statement := strings.Replace(searchSQL, "%SUMMARY%", summaryColumns(listRecipientLimit), 1)
	rows, err := tx.QueryContext(ctx, statement, appID, before, cursor.ID, query.Limit+1,
		patterns.idPrefix, patterns.recipientAddr, patterns.recipientDomain, patterns.recipientRev, patterns.senderRev, patterns.subjectQuery)
	if err != nil {
		return page, err
	}
	page, err = scanSummaryPage(rows, query.Limit)
	if err != nil {
		return admin.MessagePage{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.MessagePage{}, err
	}
	return page, nil
}

// subjectTSQuery lets PostgreSQL tokenize the term with the same expression
// that built the index, so query and stored words are normalized identically.
func subjectTSQuery(ctx context.Context, tx *sql.Tx, text string) (string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT lexeme FROM unnest(to_tsvector('simple'::regconfig, regexp_replace($1::text, '[^[:alnum:]]+', ' ', 'g'))) ORDER BY positions[1] LIMIT 8`, text)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var lexemes []string
	for rows.Next() {
		var lexeme string
		if err = rows.Scan(&lexeme); err != nil {
			return "", false, err
		}
		lexemes = append(lexemes, lexeme)
	}
	if err = rows.Err(); err != nil {
		return "", false, err
	}
	if len(lexemes) == 0 {
		return "", false, nil
	}
	return tsqueryFromLexemes(lexemes), true, nil
}

// RevealSubject returns the recorded subject. For an Enhanced Protection
// message an audit row (actor, app, action and message ID only) commits in the
// same transaction before the subject is returned. Other messages are not
// audited.
func (r *Repository) RevealSubject(ctx context.Context, actor, appID, messageID string) (string, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) {
		return "", admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var protected bool
	var subject sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT enhanced_protection,subject FROM messages WHERE app_id=$1 AND id=$2`, appID, messageID).Scan(&protected, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", admin.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !subject.Valid {
		return "", admin.ErrConflict
	}
	if protected {
		if err = recordAudit(ctx, tx, actor, appID, "message.subject.reveal", messageID); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return subject.String, nil
}
