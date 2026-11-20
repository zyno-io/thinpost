package postgres

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"strings"

	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

// dummySMTPDigest is compared against when a credential does not exist, so an
// unknown ID costs the same comparison as a wrong secret.
var dummySMTPDigest = [32]byte{0xd7, 0x1a, 0x93, 0x4c, 0x5e, 0x08, 0xb2, 0x66, 0x3f, 0xa1, 0x7c, 0x29, 0xe4, 0x50, 0x8d, 0x12,
	0x9b, 0x37, 0xc6, 0x41, 0xf0, 0x6a, 0x15, 0xde, 0x82, 0x4b, 0x09, 0xa7, 0x3c, 0xee, 0x58, 0x21}

// VerifySMTPCredential checks an SMTP username and the SHA-256 of its secret.
// It returns the owning app and whether that app is active. Unlike LookupKey
// it does not filter on the app's state: whether a paused app may submit is
// the caller's decision (a replayed message is returned even then). A revoked,
// unknown or mismatching credential is ErrInvalidKey. The digest comparison is
// constant time, and a missing row costs the same comparison.
func (r *Repository) VerifySMTPCredential(ctx context.Context, id string, digest [32]byte) (appID string, active bool, err error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var stored []byte
	err = tx.QueryRowContext(ctx, `SELECT c.app_id,c.digest,a.active FROM smtp_credentials c JOIN apps a ON a.id=c.app_id WHERE c.id=$1 AND c.revoked_at IS NULL`, id).Scan(&appID, &stored, &active)
	if errors.Is(err, sql.ErrNoRows) {
		_ = hmac.Equal(dummySMTPDigest[:], digest[:])
		return "", false, submission.ErrInvalidKey
	}
	if err != nil {
		return "", false, err
	}
	if !hmac.Equal(stored, digest[:]) {
		return "", false, submission.ErrInvalidKey
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return appID, active, nil
}

// TouchSMTPCredential records the credential's last use, at most once a
// minute. It is best effort: it gives up quickly instead of waiting for a lock
// and the caller ignores its error.
func (r *Repository) TouchSMTPCredential(ctx context.Context, id string) error {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='200ms'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE smtp_credentials SET last_used_at=clock_timestamp()
		WHERE id=$1 AND revoked_at IS NULL AND (last_used_at IS NULL OR last_used_at < clock_timestamp()-interval '60 seconds')`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SMTPPolicy reads the app's From policy and which of the candidate domains are
// active sender domains of the app. The policy's own target domain (the
// rewrite domain, or the domain of the fixed address) is always included in the
// lookup, so the caller need not know it in advance. Candidates are lowercase.
func (r *Repository) SMTPPolicy(ctx context.Context, appID string, candidates []string) (rawmime.Policy, []string, error) {
	if !opaqueID(appID) || len(candidates) > 256 {
		return rawmime.Policy{}, nil, submission.ErrInvalidMessage
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return rawmime.Policy{}, nil, err
	}
	defer tx.Rollback()
	policy, err := scanSMTPPolicy(tx.QueryRowContext(ctx, `SELECT `+smtpPolicyColumns+` FROM apps WHERE id=$1`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return rawmime.Policy{}, nil, submission.ErrInvalidMessage
	}
	if err != nil {
		return rawmime.Policy{}, nil, err
	}
	result := rawmime.Policy{Mode: rawmime.FromMode(policy.Mode), OnlyUnauthorized: policy.OnlyUnauthorized}
	lookup := append([]string(nil), candidates...)
	if policy.Domain != nil {
		result.Domain = *policy.Domain
		lookup = append(lookup, result.Domain)
	}
	if policy.Address != nil {
		result.Address = *policy.Address
		lookup = append(lookup, result.Address[strings.LastIndexByte(result.Address, '@')+1:])
	}
	verified := []string{}
	if len(lookup) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT domain FROM sender_domains WHERE app_id=$1 AND active AND domain = ANY($2)`, appID, lookup)
		if err != nil {
			return rawmime.Policy{}, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var domain string
			if err = rows.Scan(&domain); err != nil {
				return rawmime.Policy{}, nil, err
			}
			verified = append(verified, domain)
		}
		if err = rows.Err(); err != nil {
			return rawmime.Policy{}, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return rawmime.Policy{}, nil, err
	}
	return result, verified, nil
}

// LookupSMTPReplay finds the message an earlier submission of the same
// replay identity created, if its window is still open. It does not look at
// whether the app is paused: a retry after a lost acknowledgement must see the
// original ID whatever happened since.
func (r *Repository) LookupSMTPReplay(ctx context.Context, appID, key string) (submission.Receipt, bool, error) {
	if !opaqueID(appID) || key == "" || len(key) > 256 {
		return submission.Receipt{}, false, submission.ErrInvalidMessage
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return submission.Receipt{}, false, err
	}
	defer tx.Rollback()
	receipt := submission.Receipt{Replayed: true}
	err = tx.QueryRowContext(ctx, `SELECT m.id,m.accepted_at FROM idempotency_keys i JOIN messages m ON m.id=i.message_id
		WHERE i.app_id=$1 AND i.adapter=$2 AND i.key=$3 AND i.batch_receipt_ids IS NULL AND i.expires_at>now()`, appID, submission.AdapterSMTP, key).Scan(&receipt.ID, &receipt.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.Receipt{}, false, nil
	}
	if err != nil {
		return submission.Receipt{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return submission.Receipt{}, false, err
	}
	return receipt, true, nil
}

// SMTPReplaySealed returns the sealed replay secret, if one has been created.
func (r *Repository) SMTPReplaySealed(ctx context.Context) ([]byte, bool, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var sealed []byte
	err = tx.QueryRowContext(ctx, `SELECT sealed FROM smtp_replay_secret WHERE singleton`).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return sealed, true, nil
}

// InsertSMTPReplaySealed stores the sealed replay secret if none exists. The
// row is written once and never updated: concurrent replicas converge on
// whichever insert won, which they read back with SMTPReplaySealed.
func (r *Repository) InsertSMTPReplaySealed(ctx context.Context, sealed []byte) error {
	if len(sealed) < 32 || len(sealed) > 512 {
		return submission.ErrInvalidMessage
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO smtp_replay_secret(sealed) VALUES ($1) ON CONFLICT (singleton) DO NOTHING`, sealed); err != nil {
		return err
	}
	return tx.Commit()
}
