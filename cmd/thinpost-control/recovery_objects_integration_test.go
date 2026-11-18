package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/postgres"
)

func TestRecoveryObjectsCommand(t *testing.T) {
	dsn, endpoint := os.Getenv("THINPOST_TEST_DATABASE_URL"), os.Getenv("THINPOST_TEST_S3_ENDPOINT")
	if dsn == "" || endpoint == "" {
		t.Skip("requires disposable PostgreSQL and object storage")
	}
	validatedDSN, dsnErr := controlapp.DatabaseURL(func(name string) string {
		switch name {
		case "THINPOST_DATABASE_URL":
			return dsn
		case "THINPOST_ALLOW_INSECURE_LOCAL_DB":
			return "1"
		default:
			return ""
		}
	})
	u, parseErr := url.Parse(validatedDSN)
	if dsnErr != nil || parseErr != nil || u == nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("recovery command test requires a numeric loopback database")
	}
	secure := os.Getenv("THINPOST_TEST_S3_SECURE") == "1"
	accessKey, secretKey := "fixture", "fixture"
	if secure {
		if os.Getenv("THINPOST_TEST_S3_ALLOW_REMOTE") != "1" || endpoint == "" || endpoint != os.Getenv("THINPOST_TEST_S3_SECURE_ENDPOINT") {
			t.Fatal("secure test requires THINPOST_TEST_S3_ALLOW_REMOTE=1 and THINPOST_TEST_S3_SECURE_ENDPOINT equal to the endpoint")
		}
		accessKey, secretKey = os.Getenv("THINPOST_TEST_S3_ACCESS_KEY"), os.Getenv("THINPOST_TEST_S3_SECRET_KEY")
		if accessKey == "" || secretKey == "" {
			t.Fatal("secure test credentials are required")
		}
	} else {
		host, _, splitErr := net.SplitHostPort(endpoint)
		if splitErr != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			t.Fatal("insecure test object storage must be numeric loopback")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(id[:])
	schema, bucket := "recovery_cli_"+suffix, "thinpost-recovery-"+suffix
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Construct the staging-era schema from the immutable embedded migration
	// prefix and exercise the actual recovery CLI against that older schema.
	if _, err = db.ExecContext(ctx, `CREATE TABLE thinpost_schema_migrations(version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	files, err := postgres.Schema.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"schema.sql"}
	for _, file := range files {
		if len(file.Name()) < 4 {
			t.Fatal("invalid embedded migration filename")
		}
		version, versionErr := strconv.Atoi(file.Name()[:3])
		if versionErr != nil {
			t.Fatal(versionErr)
		}
		if version <= 17 {
			paths = append(paths, "migrations/"+file.Name())
		}
	}
	if len(paths) != 17 {
		t.Fatal("unexpected schema-17 migration prefix")
	}
	for index, path := range paths {
		body, readErr := postgres.Schema.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = db.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
		checksum := sha256.Sum256(body)
		if _, err = db.ExecContext(ctx, `INSERT INTO thinpost_schema_migrations(version,checksum) VALUES ($1,$2)`, index+1, hex.EncodeToString(checksum[:])); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("THINPOST_DATABASE_URL", u.String())
	t.Setenv("THINPOST_ALLOW_INSECURE_LOCAL_DB", "1")
	const gen = "11111111111111111111111111111111"
	const run = "22222222222222222222222222222222"
	runRecovery := func(args ...string) error {
		t.Helper()
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		old := os.Stdout
		os.Stdout = writer
		commandErr := recovery(args)
		os.Stdout = old
		_ = writer.Close()
		_, readErr := io.Copy(io.Discard, reader)
		_ = reader.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return commandErr
	}
	if err = runRecovery("inspect"); err != nil {
		t.Fatal("schema-17 recovery inspection", err)
	}
	if err = runRecovery("begin", "--generation", gen, "--recovery-id", run, "--actor", "operator", "--expected-revision", "1", "--workers-fenced", "--database-isolated"); err != nil {
		t.Fatal("schema-17 recovery begin", err)
	}
	if err = runRecovery("migrate-restored-schema", "--generation", gen, "--recovery-id", run, "--actor", "operator", "--expected-revision", "2", "--workers-fenced", "--database-isolated"); err != nil {
		t.Fatal("schema-17 recovery migration", err)
	}
	if complete, qerr := postgres.RecoveryQuarantineBatch(ctx, db, gen, run, "operator"); qerr != nil || !complete {
		t.Fatal(complete, qerr)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ('app','app');
		INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('message','app','source/fixture',true,'done');
		INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','message',0,'final/fixture','accepted');
		INSERT INTO bounce_receipts(id,delivery_id,object_key,fingerprint) VALUES ('bounce','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bounce/fixture',decode(repeat('00',32),'hex'));
		INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','tracking/v1/0000000000/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',now(),1)`); err != nil {
		t.Fatal(err)
	}
	objectKeys := []string{"source/fixture", "final/fixture", "bounce/fixture", "tracking/v1/0000000000/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disposable recovery test bucket: %s", bucket)
	exists, existsErr := client.BucketExists(ctx, bucket)
	if existsErr != nil || exists {
		t.Fatal("could not establish that the disposable bucket name is unused")
	}
	if err = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, objectKey := range objectKeys {
			if cleanupErr := client.RemoveObject(cleanupCtx, bucket, objectKey, minio.RemoveObjectOptions{}); cleanupErr != nil {
				t.Errorf("failed to remove synthetic object from disposable bucket %s", bucket)
			}
		}
		if cleanupErr := client.RemoveBucket(cleanupCtx, bucket); cleanupErr != nil {
			t.Errorf("failed to remove disposable bucket %s", bucket)
		}
	}()
	keyFile := filepath.Join(t.TempDir(), "keyring.json")
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if err = os.WriteFile(keyFile, []byte(fmt.Sprintf(`{"active":"v1","keys":{"v1":%q}}`, key)), 0600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objectstore.NewEncrypted(&objectstore.Store{Client: client, Bucket: bucket}, ring)
	if err != nil {
		t.Fatal(err)
	}
	for _, objectKey := range objectKeys {
		if err = objects.Put(ctx, objectKey, []byte("synthetic content")); err != nil {
			t.Fatal(err)
		}
	}
	insecureS3 := "1"
	if secure {
		insecureS3 = ""
	}
	for k, v := range map[string]string{"THINPOST_DATABASE_URL": u.String(), "THINPOST_ALLOW_INSECURE_LOCAL_DB": "1", "THINPOST_ALLOW_INSECURE_LOCAL_S3": insecureS3, "THINPOST_S3_ENDPOINT": endpoint, "THINPOST_S3_BUCKET": bucket, "THINPOST_S3_REGION": "us-east-1", "THINPOST_S3_ACCESS_KEY": accessKey, "THINPOST_S3_SECRET_KEY": secretKey, "THINPOST_OBJECT_KEYRING_FILE": keyFile} {
		t.Setenv(k, v)
	}
	args := []string{"objects", "--generation", gen, "--recovery-id", run, "--expected-revision", "4", "--workers-fenced", "--database-isolated", "--objects-frozen"}
	invoke := func(extra ...string) (postgres.RecoveryObjectPage, error) {
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		old := os.Stdout
		os.Stdout = writer
		commandArgs := append(append([]string(nil), args...), extra...)
		commandErr := recovery(commandArgs)
		os.Stdout = old
		_ = writer.Close()
		data, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		var page postgres.RecoveryObjectPage
		if unmarshalErr := json.Unmarshal(data, &page); unmarshalErr != nil {
			t.Fatal("invalid inventory output", unmarshalErr)
		}
		return page, commandErr
	}
	page, err := invoke("--limit", "2")
	if err != nil || page.Checked != 2 || len(page.Failed) != 0 || page.Next == nil || page.Next.Kind != "delivery" {
		t.Fatal("first inventory page rejected or incomplete", page, err)
	}
	page, err = invoke("--limit", "2", "--after-kind", page.Next.Kind, "--after-id", page.Next.ID)
	if err != nil || page.Checked != 2 || len(page.Failed) != 0 || page.Next != nil {
		t.Fatal("second inventory page rejected or incomplete", page, err)
	}
	if err = client.RemoveObject(ctx, bucket, "source/fixture", minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	// An existing but unauthentic object must fail just like a missing one.
	if err = client.RemoveObject(ctx, bucket, "final/fixture", minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	corrupt := "not an encrypted object"
	if _, err = client.PutObject(ctx, bucket, "final/fixture", strings.NewReader(corrupt), int64(len(corrupt)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	page, err = invoke()
	if err == nil || page.Checked != 4 || len(page.Failed) != 2 || page.Failed[0].Kind != "delivery" || page.Failed[1].Kind != "message" {
		t.Fatal("missing or corrupt object accepted", page, err)
	}
	wrongKey := base64.StdEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...))
	if err = os.WriteFile(keyFile, []byte(fmt.Sprintf(`{"active":"v1","keys":{"v1":%q}}`, wrongKey)), 0600); err != nil {
		t.Fatal(err)
	}
	page, err = invoke()
	if err == nil || page.Checked != 4 || len(page.Failed) != 4 {
		t.Fatal("wrong encryption key accepted", page, err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ('held','app','source/held',true,'recovery_hold')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO recovery_holds(recovery_id,kind,entity_id,original_state) VALUES ('00000000000000000000000000000001','message','held','queued'),($1,'message','held','recovery_hold')`, run); err != nil {
		t.Fatal(err)
	}
	invokeHistory := func(after string) (postgres.RecoveryHoldHistory, error) {
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		old := os.Stdout
		os.Stdout = writer
		historyArgs := []string{"hold-history", "--generation", gen, "--recovery-id", run, "--expected-revision", "4", "--kind", "message", "--entity-id", "held", "--limit", "1"}
		if after != "" {
			historyArgs = append(historyArgs, "--after-recovery-id", after)
		}
		commandErr := recovery(historyArgs)
		os.Stdout = old
		_ = writer.Close()
		data, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		var history postgres.RecoveryHoldHistory
		if unmarshalErr := json.Unmarshal(data, &history); unmarshalErr != nil {
			t.Fatal("invalid hold history output", unmarshalErr)
		}
		return history, commandErr
	}
	history, historyErr := invokeHistory("")
	if historyErr != nil || len(history.Entries) != 1 || history.Entries[0].OriginalState != "queued" || history.Next != "00000000000000000000000000000001" {
		t.Fatal("first hold history command page", history, historyErr)
	}
	history, historyErr = invokeHistory(history.Next)
	if historyErr != nil || len(history.Entries) != 1 || history.Entries[0].OriginalState != "recovery_hold" || history.Next != "" {
		t.Fatal("second hold history command page", history, historyErr)
	}
}
