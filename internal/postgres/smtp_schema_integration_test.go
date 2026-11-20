package postgres

import (
	"context"
	"strings"
	"testing"
)

const smtpTestDigest = `decode(repeat('ab',32),'hex')`

func TestSMTPSubmissionSchemaConstraints(t *testing.T) {
	ctx := context.Background()
	db := migratedSchemaForTest(t, ctx, "smtp_schema_")
	cred := strings.Repeat("c", 32)
	other := strings.Repeat("d", 32)
	for _, statement := range []string{
		`INSERT INTO apps(id,name) VALUES ('a','A'),('b','B')`,
		`INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('` + cred + `','a','Printer',` + smtpTestDigest + `)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	const meta = `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev`
	// An empty subject is allowed only with a credential, and the credential
	// must belong to the message's own app.
	if _, err := db.ExecContext(ctx, meta+`,smtp_credential_id,original_from_address) VALUES ('m1','a','k',false,'',1,'example.com','com.example.','`+cred+`','o@example.net')`); err != nil {
		t.Fatal("empty subject with credential rejected", err)
	}
	for name, statement := range map[string]string{
		"empty subject without credential": meta + `) VALUES ('x1','a','k',false,'',1,'example.com','com.example.')`,
		"credential of another app":        meta + `,smtp_credential_id) VALUES ('x2','b','k',false,'hi',1,'example.com','com.example.','` + cred + `')`,
		"unknown credential":               meta + `,smtp_credential_id) VALUES ('x3','a','k',false,'hi',1,'example.com','com.example.','` + other + `')`,
		"original From without credential": meta + `,original_from_address) VALUES ('x4','a','k',false,'hi',1,'example.com','com.example.','o@example.net')`,
		"original From too short":          meta + `,smtp_credential_id,original_from_address) VALUES ('x5','a','k',false,'hi',1,'example.com','com.example.','` + cred + `','a@')`,
		"subject over 998 bytes":           meta + `,smtp_credential_id) VALUES ('x6','a','k',false,repeat('s',999),1,'example.com','com.example.','` + cred + `')`,
		"credential id shape":              `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('XYZ','a','n',` + smtpTestDigest + `)`,
		"credential digest length":         `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('` + other + `','a','n',decode('ab','hex'))`,
		"duplicate digest":                 `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('` + other + `','a','n',` + smtpTestDigest + `)`,
		"empty credential name":            `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('` + other + `','a','',decode(repeat('cd',32),'hex'))`,
		"credential name too long":         `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('` + other + `','a',repeat('n',129),decode(repeat('cd',32),'hex'))`,
		"credential of unknown app":        `INSERT INTO smtp_credentials(id,app_id,name,digest) VALUES ('` + other + `','zz','n',decode(repeat('cd',32),'hex'))`,
		"revoked before created":           `UPDATE smtp_credentials SET revoked_at=created_at-interval '1 second' WHERE id='` + cred + `'`,
		"replay secret too short":          `INSERT INTO smtp_replay_secret(sealed) VALUES (decode('ab','hex'))`,
		"domain mode without domain":       `UPDATE apps SET smtp_from_mode='domain' WHERE id='a'`,
		"address mode without address":     `UPDATE apps SET smtp_from_mode='address' WHERE id='a'`,
		"unknown mode":                     `UPDATE apps SET smtp_from_mode='rewrite' WHERE id='a'`,
		"keep origin column is gone":       `UPDATE apps SET smtp_from_keep_origin=true WHERE id='a'`,
		"address without domain part":      `UPDATE apps SET smtp_from_mode='address', smtp_from_address='nobody' WHERE id='a'`,
		"address with quoted local part":   `UPDATE apps SET smtp_from_mode='address', smtp_from_address='"a b"@example.com' WHERE id='a'`,
		"address with consecutive dots":    `UPDATE apps SET smtp_from_mode='address', smtp_from_address='a..b@example.com' WHERE id='a'`,
		"address with uppercase domain":    `UPDATE apps SET smtp_from_mode='address', smtp_from_address='a@Example.com' WHERE id='a'`,
		"address local part over 64 bytes": `UPDATE apps SET smtp_from_mode='address', smtp_from_address=repeat('a',65)||'@example.com' WHERE id='a'`,
		"domain with uppercase":            `UPDATE apps SET smtp_from_mode='domain', smtp_from_domain='Example.com' WHERE id='a'`,
		"domain mode with an address":      `UPDATE apps SET smtp_from_mode='domain', smtp_from_domain='example.com', smtp_from_address='a@example.com' WHERE id='a'`,
		"address mode with a domain":       `UPDATE apps SET smtp_from_mode='address', smtp_from_address='a@example.com', smtp_from_domain='example.com' WHERE id='a'`,
		"off mode with a domain":           `UPDATE apps SET smtp_from_mode='off', smtp_from_domain='example.com' WHERE id='a'`,
		"too many submission ports":        `INSERT INTO edge_source_reports(worker_id,epoch,reported_at,submission_ports) VALUES ('w',1,now(),ARRAY[1,2,3,4,5,6,7,8,9])`,
	} {
		if _, err := db.ExecContext(ctx, statement); err == nil {
			t.Fatal("constraint did not reject:", name)
		}
	}
	for _, statement := range []string{
		`UPDATE apps SET smtp_from_mode='address', smtp_from_address='no-reply+x@example.com' WHERE id='a'`,
		`UPDATE apps SET smtp_from_mode='domain', smtp_from_domain='example.com', smtp_from_address=NULL WHERE id='a'`,
		`UPDATE smtp_credentials SET revoked_at=clock_timestamp(), last_used_at=clock_timestamp() WHERE id='` + cred + `'`,
		`INSERT INTO smtp_replay_secret(sealed) VALUES (decode(repeat('ab',40),'hex'))`,
		`INSERT INTO edge_source_reports(worker_id,epoch,reported_at,submission_ports) VALUES ('w',1,now(),ARRAY[465,587])`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	// The replay secret is a singleton.
	if _, err := db.ExecContext(ctx, `INSERT INTO smtp_replay_secret(sealed) VALUES (decode(repeat('ab',40),'hex'))`); err == nil {
		t.Fatal("second replay secret accepted")
	}
}
