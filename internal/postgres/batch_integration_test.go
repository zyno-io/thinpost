package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/httpapi"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
)

type batchAllowedRecipients struct{}

func (batchAllowedRecipients) Allows(string) bool { return true }

type batchMemoryObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (o *batchMemoryObjects) Put(_ context.Context, key string, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.data == nil {
		o.data = make(map[string][]byte)
	}
	if _, exists := o.data[key]; exists {
		return errors.New("object exists")
	}
	o.data[key] = bytes.Clone(data)
	return nil
}

func (o *batchMemoryObjects) Get(_ context.Context, key string, maximum int64) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, exists := o.data[key]
	if !exists || int64(len(data)) > maximum {
		return nil, errors.New("missing or oversized object")
	}
	return bytes.Clone(data), nil
}

func (o *batchMemoryObjects) VerifyPresent(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.data[key]; !exists {
		return errors.New("object missing")
	}
	return nil
}

func TestResendBatchPostgresAtomicityAndIdempotency(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "batch_test_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	owner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	const generation = "00000000000000000000000000000aaa"
	keyDigest := sha256.Sum256([]byte("test-key"))
	if _, err = owner.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `INSERT INTO apps(id,name,recipient_daily_limit,pending_message_limit) VALUES ('batch-app','Batch',1000,1000),('quota-app','Quota',1,1000),('pending-app','Pending',1000,1),('http-app','HTTP',1000,1000),('grid-app','Grid',1000,1000),('grid-fail','Grid fail',1000,1000);
		INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ('batch-app','example.com',true,now()),('quota-app','example.com',true,now()),('pending-app','example.com',true,now()),('http-app','example.com',true,now()),('grid-app','example.com',true,now()),('grid-fail','example.com',true,now());`); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"batch-app", "quota-app", "pending-app"} {
		digest := sha256.Sum256(append(keyDigest[:], []byte(app)...))
		if _, err = owner.ExecContext(ctx, `INSERT INTO api_keys(id,digest,app_id) VALUES ($1,$2,$3)`, app+"-key", digest[:], app); err != nil {
			t.Fatal(err)
		}
	}
	apiKey := "disposable-http-batch-key"
	apiDigest := sha256.Sum256([]byte(apiKey))
	if _, err = owner.ExecContext(ctx, `INSERT INTO api_keys(id,digest,app_id) VALUES ('http-key',$1,'http-app')`, apiDigest[:]); err != nil {
		t.Fatal(err)
	}
	gridDigest, failDigest := sha256.Sum256([]byte("grid-fixture-key")), sha256.Sum256([]byte("grid-failure-key"))
	if _, err = owner.ExecContext(ctx, `INSERT INTO api_keys(id,digest,app_id) VALUES ('grid-key',$1,'grid-app'),('grid-fail-key',$2,'grid-fail')`, gridDigest[:], failDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE apps SET submission_rate_per_minute=60000,submission_burst=100,submission_tokens=6000000000 WHERE id IN ('http-app','grid-app','grid-fail')`); err != nil {
		t.Fatal(err)
	}
	runtimeDB, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: runtimeDB, Generation: generation, Objects: objectVerifierFunc(func(context.Context, string) error { return nil })}
	hash := sha256.Sum256([]byte("whole batch"))
	makeRecord := func(app, key string, seed int) acceptance.BatchRecord {
		return acceptance.BatchRecord{Record: acceptance.Record{AppID: app, KeyID: app + "-key", Adapter: "resend", IdempotencyKey: key, RequestHash: hash}, Items: []acceptance.BatchItem{
			withItemMetadata(acceptance.BatchItem{ObjectKey: fixtureObjectKey("submissions", fmt.Sprintf("%032x", seed)), From: "sender@example.com", RecipientCount: 1, EnhancedProtection: true}),
			withItemMetadata(acceptance.BatchItem{ObjectKey: fixtureObjectKey("submissions", fmt.Sprintf("%032x", seed+1)), From: "sender@example.com", RecipientCount: 1}),
		}}
	}
	t.Run("HTTP through encrypted objects and PostgreSQL", func(t *testing.T) {
		keyringFile := filepath.Join(t.TempDir(), "keys.json")
		encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
		if err := os.WriteFile(keyringFile, []byte(fmt.Sprintf(`{"active":"test","keys":{"test":%q}}`, encoded)), 0600); err != nil {
			t.Fatal(err)
		}
		keyring, err := objectstore.LoadKeyring(keyringFile)
		if err != nil {
			t.Fatal(err)
		}
		raw := &batchMemoryObjects{}
		objects, err := objectstore.NewEncrypted(raw, keyring)
		if err != nil {
			t.Fatal(err)
		}
		apiRepo := &Repository{DB: runtimeDB, Generation: generation, Objects: objects}
		service, err := acceptance.New(apiRepo, objects, bytes.Repeat([]byte{8}, 32), batchAllowedRecipients{})
		if err != nil {
			t.Fatal(err)
		}
		handler := controlapp.Router(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), httpapi.NewWithOptions(service, httpapi.Options{Tracking: true}), nil, nil)
		body := `[{"from":"sender@example.com","to":"one@example.net","subject":"one","text":"sample-private-one","html":"<b>one</b>"},{"from":"sender@example.com","to":"two@example.net","subject":"two","text":"sample-private-two","html":"<b>two</b>"}]`
		send := func(payload, tracking string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/emails/batch", strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer "+apiKey)
			request.Header.Set("Idempotency-Key", "http-batch")
			request.Header.Set("X-Thinpost-Enhanced-Protection", "true")
			request.Header.Set("X-Thinpost-Track-Opens", tracking)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		}
		first := send(body, "true")
		if first.Code != http.StatusOK {
			t.Fatal("batch HTTP acceptance failed", first.Code, first.Body.String())
		}
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err = json.Unmarshal(first.Body.Bytes(), &result); err != nil || len(result.Data) != 2 {
			t.Fatal("invalid batch HTTP receipt", err)
		}
		for _, entry := range result.Data {
			var key string
			var tlsRequired bool
			// The adapter returns the provider-shaped ID; the database keys on 32 hex.
			messageID, _ := providerid.Normalize(entry.ID)
			if err = owner.QueryRowContext(ctx, `SELECT object_key,enhanced_protection FROM messages WHERE app_id='http-app' AND id=$1`, messageID).Scan(&key, &tlsRequired); err != nil || !tlsRequired {
				t.Fatal("batch message not committed with TLS", err)
			}
			plain, err := objects.Get(ctx, key, 4096)
			if err != nil || !bytes.Contains(plain, []byte("sample-private-")) {
				t.Fatal("encrypted batch source could not be read", err)
			}
			var source struct {
				Message submission.Message `json:"message"`
			}
			if err = json.Unmarshal(plain, &source); err != nil || source.Message.Tracking == nil || !source.Message.Tracking.Opens {
				t.Fatal("batch tracking option was not preserved", err)
			}
			raw.mu.Lock()
			ciphertext := bytes.Clone(raw.data[key])
			raw.mu.Unlock()
			if bytes.Contains(ciphertext, []byte("sample-private-")) {
				t.Fatal("batch source leaked plaintext into object storage")
			}
		}
		retry := send(body, "true")
		if retry.Code != http.StatusOK || retry.Body.String() != first.Body.String() {
			t.Fatal("HTTP retry did not return the original ordered response")
		}
		changed := send(strings.Replace(body, "sample-private-two", "different", 1), "true")
		if changed.Code != http.StatusConflict {
			t.Fatal("HTTP idempotency conflict was not reported", changed.Code)
		}
		if changedTracking := send(body, "false"); changedTracking.Code != http.StatusConflict {
			t.Fatal("changed tracking policy did not conflict", changedTracking.Code)
		}
		if _, err = owner.ExecContext(ctx, `UPDATE apps SET submission_rate_per_minute=1,submission_burst=2,submission_tokens=0,submission_refilled_at=clock_timestamp() WHERE id='http-app'`); err != nil {
			t.Fatal(err)
		}
		raw.mu.Lock()
		beforeDenied := len(raw.data)
		raw.mu.Unlock()
		if limited := send(body, "true"); limited.Code != http.StatusTooManyRequests {
			t.Fatal("exhausted retry was not rate-limited", limited.Code)
		}
		raw.mu.Lock()
		afterDenied := len(raw.data)
		raw.mu.Unlock()
		if beforeDenied != afterDenied {
			t.Fatal("rate-limited retry uploaded objects")
		}
		if _, err = owner.ExecContext(ctx, `UPDATE apps SET submission_tokens=120000000 WHERE id='http-app'`); err != nil {
			t.Fatal(err)
		}
		if resumed := send(body, "true"); resumed.Code != http.StatusOK || resumed.Body.String() != first.Body.String() {
			t.Fatal("retry after refill did not return its original receipt", resumed.Code)
		}
		var count, used int
		if err = owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages WHERE app_id='http-app'),recipient_quota_used FROM apps WHERE id='http-app'`).Scan(&count, &used); err != nil || count != 2 || used != 2 {
			t.Fatal("HTTP batch retry duplicated messages or quota", count, used, err)
		}
	})
	t.Run("SendGrid personalizations commit together", func(t *testing.T) {
		keyringFile := filepath.Join(t.TempDir(), "keys.json")
		encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
		if err := os.WriteFile(keyringFile, []byte(fmt.Sprintf(`{"active":"test","keys":{"test":%q}}`, encoded)), 0600); err != nil {
			t.Fatal(err)
		}
		keyring, err := objectstore.LoadKeyring(keyringFile)
		if err != nil {
			t.Fatal(err)
		}
		raw := &batchMemoryObjects{}
		objects, err := objectstore.NewEncrypted(raw, keyring)
		if err != nil {
			t.Fatal(err)
		}
		apiRepo := &Repository{DB: runtimeDB, Generation: generation, Objects: objects}
		service, err := acceptance.New(apiRepo, objects, bytes.Repeat([]byte{8}, 32), batchAllowedRecipients{})
		if err != nil {
			t.Fatal(err)
		}
		handler := httpapi.New(service)
		body := `{"from":{"email":"sender@example.com"},"subject":"global","personalizations":[{"to":[{"email":"one@example.net"}],"subject":"first"},{"to":[{"email":"two@example.net"}],"from":{"email":"other@example.com"},"subject":"second"}],"content":[{"type":"text/plain","value":"grid-private-body"}]}`
		send := func(key, payload string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/v3/mail/send", strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer "+key)
			request.Header.Set("X-Thinpost-Enhanced-Protection", "true")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		}
		if response := send("grid-fixture-key", body); response.Code != http.StatusAccepted || response.Body.Len() != 0 {
			t.Fatal("SendGrid batch rejected", response.Code, response.Body.String())
		}
		rows, err := owner.QueryContext(ctx, `SELECT object_key,enhanced_protection FROM messages WHERE app_id='grid-app'`)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for rows.Next() {
			var key string
			var enhancedProtection bool
			if err = rows.Scan(&key, &enhancedProtection); err != nil || !enhancedProtection {
				rows.Close()
				t.Fatal("TLS policy lost", err)
			}
			keys = append(keys, key)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if len(keys) != 2 {
			t.Fatal("incorrect committed batch size", len(keys))
		}
		seen := make(map[string]submission.Message)
		for _, key := range keys {
			plain, getErr := objects.Get(ctx, key, 4096)
			if getErr != nil {
				t.Fatal(getErr)
			}
			var source struct {
				Message submission.Message `json:"message"`
			}
			if err = json.Unmarshal(plain, &source); err != nil {
				t.Fatal(err)
			}
			seen[source.Message.Subject] = source.Message
			raw.mu.Lock()
			ciphertext := bytes.Clone(raw.data[key])
			raw.mu.Unlock()
			if bytes.Contains(ciphertext, []byte("grid-private-body")) || bytes.Contains(ciphertext, []byte("one@example.net")) {
				t.Fatal("personalization leaked into raw storage")
			}
		}
		if len(seen) != 2 || len(seen["first"].To) != 1 || !strings.Contains(seen["first"].To[0], "one@example.net") || len(seen["second"].To) != 1 || !strings.Contains(seen["second"].To[0], "two@example.net") || !strings.Contains(seen["second"].From, "other@example.com") {
			t.Fatal("personalizations were mixed")
		}
		meta, err := owner.QueryContext(ctx, `SELECT m.subject,m.sender_domain,m.sender_domain_rev,m.recipient_count,string_agg(r.address,',' ORDER BY r.recipient_index) FROM messages m JOIN message_recipients r ON r.message_id=m.id AND r.app_id=m.app_id WHERE m.app_id='grid-app' GROUP BY m.id ORDER BY m.subject`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for meta.Next() {
			var subject, domain, rev, addresses string
			var count int
			if err = meta.Scan(&subject, &domain, &rev, &count, &addresses); err != nil {
				meta.Close()
				t.Fatal(err)
			}
			got = append(got, fmt.Sprint(subject, "|", domain, "|", rev, "|", count, "|", addresses))
		}
		meta.Close()
		if fmt.Sprint(got) != "[first|example.com|com.example.|1|one@example.net second|example.com|com.example.|1|two@example.net]" {
			t.Fatal("batch per-item metadata incorrect", got)
		}
		unauthorized := strings.Replace(body, "other@example.com", "other@unauthorized.example", 1)
		if response := send("grid-failure-key", unauthorized); response.Code != http.StatusBadRequest {
			t.Fatal("unauthorized override accepted", response.Code)
		}
		if _, err = owner.ExecContext(ctx, `CREATE FUNCTION fixture_grid_fail_second() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.app_id='grid-fail' AND EXISTS (SELECT 1 FROM messages WHERE app_id=NEW.app_id) THEN RAISE EXCEPTION 'synthetic insert failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fixture_grid_fail_second BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION fixture_grid_fail_second()`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = owner.ExecContext(context.Background(), `DROP TRIGGER fixture_grid_fail_second ON messages; DROP FUNCTION fixture_grid_fail_second()`)
		}()
		if response := send("grid-failure-key", body); response.Code != http.StatusServiceUnavailable {
			t.Fatal("insert failure reported success", response.Code)
		}
		var count, used int
		if err = owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages WHERE app_id='grid-fail')+(SELECT count(*) FROM message_recipients WHERE app_id='grid-fail'),recipient_quota_used FROM apps WHERE id='grid-fail'`).Scan(&count, &used); err != nil || count != 0 || used != 0 {
			t.Fatal("failed batch left partial records or quota", count, used, err)
		}
	})
	t.Run("concurrent whole-batch retry", func(t *testing.T) {
		const workers = 8
		type result struct {
			receipt submission.BatchReceipt
			err     error
		}
		results := make(chan result, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				<-start
				receipt, err := repo.CommitBatch(ctx, makeRecord("batch-app", "retry", 100+seed*10))
				results <- result{receipt, err}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		var first []string
		for result := range results {
			if result.err != nil || len(result.receipt.IDs) != 2 {
				t.Fatal("batch retry failed", result.err)
			}
			if first == nil {
				first = result.receipt.IDs
			} else if !reflect.DeepEqual(first, result.receipt.IDs) {
				t.Fatal("concurrent retries returned different IDs")
			}
		}
		var messages, quota int
		if err := owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages WHERE app_id='batch-app'),recipient_quota_used FROM apps WHERE id='batch-app'`).Scan(&messages, &quota); err != nil || messages != 2 || quota != 2 {
			t.Fatal("batch duplicated messages or quota", messages, quota, err)
		}
		var firstTLS, secondTLS bool
		if err := owner.QueryRowContext(ctx, `SELECT (SELECT enhanced_protection FROM messages WHERE id=$1),(SELECT enhanced_protection FROM messages WHERE id=$2)`, first[0], first[1]).Scan(&firstTLS, &secondTLS); err != nil || !firstTLS || secondTLS {
			t.Fatal("per-message TLS policy or ordering changed", err)
		}
		rotated := makeRecord("batch-app", "retry", 999)
		rotated.PreviousHashes = [][32]byte{hash}
		rotated.RequestHash = sha256.Sum256([]byte("new signing key"))
		if receipt, err := repo.CommitBatch(ctx, rotated); err != nil || !reflect.DeepEqual(receipt.IDs, first) {
			t.Fatal("fingerprint rotation did not preserve batch retry", err)
		}
		changed := makeRecord("batch-app", "retry", 2000)
		changed.RequestHash = sha256.Sum256([]byte("different batch"))
		if _, err := repo.CommitBatch(ctx, changed); !errors.Is(err, submission.ErrIdempotencyConflict) {
			t.Fatal("changed batch did not conflict", err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE idempotency_keys SET batch_receipt_ids='[]'::jsonb WHERE app_id='batch-app' AND key='retry'`); err == nil {
			t.Fatal("empty persisted batch receipt was accepted")
		}
		if _, err := owner.ExecContext(ctx, `UPDATE idempotency_keys SET batch_receipt_ids=jsonb_build_array(repeat('x',4001)) WHERE app_id='batch-app' AND key='retry'`); err == nil {
			t.Fatal("oversized persisted batch receipt was accepted")
		}
		var originalIDs []byte
		if err := owner.QueryRowContext(ctx, `SELECT batch_receipt_ids FROM idempotency_keys WHERE app_id='batch-app' AND key='retry'`).Scan(&originalIDs); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE idempotency_keys SET batch_receipt_ids=jsonb_build_array(message_id,message_id) WHERE app_id='batch-app' AND key='retry'`); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.CommitBatch(ctx, makeRecord("batch-app", "retry", 2500)); err == nil {
			t.Fatal("corrupt duplicate batch receipt was accepted")
		}
		if _, err := owner.ExecContext(ctx, `UPDATE idempotency_keys SET batch_receipt_ids=$1::jsonb WHERE app_id='batch-app' AND key='retry'`, string(originalIDs)); err != nil {
			t.Fatal(err)
		}
		single := withMetadata(acceptance.Record{AppID: "batch-app", KeyID: "batch-app-key", Adapter: "resend", IdempotencyKey: "retry", RequestHash: hash, ObjectKey: fixtureObjectKey("submissions", fmt.Sprintf("%032x", 3000)), From: "sender@example.com", RecipientCount: 1})
		if _, err := repo.Commit(ctx, single); !errors.Is(err, submission.ErrIdempotencyConflict) {
			t.Fatal("single endpoint reused a live batch marker", err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE idempotency_keys SET expires_at=now()-interval '1 second' WHERE app_id='batch-app' AND key='retry'`); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Commit(ctx, single); err != nil {
			t.Fatal("expired batch marker could not be reused by single submission", err)
		}
		var isSingle bool
		if err := owner.QueryRowContext(ctx, `SELECT batch_receipt_ids IS NULL FROM idempotency_keys WHERE app_id='batch-app' AND key='retry'`).Scan(&isSingle); err != nil || !isSingle {
			t.Fatal("reused single marker retained batch receipt", err)
		}
		if _, err := repo.CommitBatch(ctx, makeRecord("batch-app", "retry", 4000)); !errors.Is(err, submission.ErrIdempotencyConflict) {
			t.Fatal("batch endpoint reused a live single marker", err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE idempotency_keys SET expires_at=now()-interval '1 second' WHERE app_id='batch-app' AND key='retry'`); err != nil {
			t.Fatal(err)
		}
		if receipt, err := repo.CommitBatch(ctx, makeRecord("batch-app", "retry", 4000)); err != nil || len(receipt.IDs) != 2 {
			t.Fatal("expired single marker could not be reused by batch", err)
		}
	})
	t.Run("quota and pending limits roll back all items", func(t *testing.T) {
		for _, tc := range []struct {
			app  string
			want error
		}{{"quota-app", submission.ErrQuotaExceeded}, {"pending-app", submission.ErrBacklogExceeded}} {
			_, err := repo.CommitBatch(ctx, makeRecord(tc.app, "rollback", 5000))
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: expected %v, got %v", tc.app, tc.want, err)
			}
			var messages, markers, used int
			err = owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages WHERE app_id=$1),(SELECT count(*) FROM idempotency_keys WHERE app_id=$1),recipient_quota_used FROM apps WHERE id=$1`, tc.app).Scan(&messages, &markers, &used)
			if err != nil || messages != 0 || markers != 0 || used != 0 {
				t.Fatal("failed batch left records or spent quota", tc.app, messages, markers, used, err)
			}
		}
	})
	t.Run("authorization is rechecked before publication", func(t *testing.T) {
		record := makeRecord("batch-app", "unauthorized", 5500)
		record.Items[1].From = "sender@other.example"
		if _, err := repo.CommitBatch(ctx, record); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatal("batch accepted an unauthorized sender", err)
		}
		if _, err := owner.ExecContext(ctx, `UPDATE api_keys SET active=false WHERE id='batch-app-key'`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = owner.ExecContext(context.Background(), `UPDATE api_keys SET active=true WHERE id='batch-app-key'`)
		}()
		record.Items[1].From = "sender@example.com"
		if _, err := repo.CommitBatch(ctx, record); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatal("batch accepted a revoked API key", err)
		}
		var messages, markers int
		err := owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages WHERE object_key IN ($1,$2)),(SELECT count(*) FROM idempotency_keys WHERE app_id='batch-app' AND key='unauthorized')`, record.Items[0].ObjectKey, record.Items[1].ObjectKey).Scan(&messages, &markers)
		if err != nil || messages != 0 || markers != 0 {
			t.Fatal("authorization failure published part of batch", messages, markers, err)
		}
	})
	t.Run("missing object rolls back the complete batch", func(t *testing.T) {
		record := makeRecord("batch-app", "missing", 6000)
		missing := record.Items[1].ObjectKey
		broken := &Repository{DB: runtimeDB, Generation: generation, Objects: objectVerifierFunc(func(_ context.Context, key string) error {
			if key == missing {
				return errors.New("missing")
			}
			return nil
		})}
		var before int
		if err := owner.QueryRowContext(ctx, `SELECT recipient_quota_used FROM apps WHERE id='batch-app'`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if _, err := broken.CommitBatch(ctx, record); err == nil {
			t.Fatal("accepted batch with missing object")
		}
		var objects, markers, after int
		err := owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM messages WHERE object_key IN ($1,$2)),(SELECT count(*) FROM idempotency_keys WHERE app_id='batch-app' AND key='missing'),recipient_quota_used FROM apps WHERE id='batch-app'`, record.Items[0].ObjectKey, missing).Scan(&objects, &markers, &after)
		if err != nil || objects != 0 || markers != 0 || after != before {
			t.Fatal("missing object left partial batch", objects, markers, before, after, err)
		}
	})
}
