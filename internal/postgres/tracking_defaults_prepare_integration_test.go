package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
)

type fixtureSigningKeys struct{ key *rsa.PrivateKey }

func (k fixtureSigningKeys) ForSender(context.Context, string, string) (preparation.SigningKey, error) {
	return preparation.SigningKey{Domain: "example.com", Selector: "mail", Signer: k.key}, nil
}

// An app default accepted through the real repository is rewritten by the real
// preparation service; an explicit opt-out is not. Only the DKIM key lookup is
// a fixture (there is no sender-key store in this package).
func TestTrackingDefaultAcceptedThenPreparedWithPostgres(t *testing.T) {
	f := newWebhookFixture(t, "trackprep_")
	ctx := f.ctx
	app := f.app("defaultprep")
	f.exec(`UPDATE apps SET default_track_clicks=true,submission_rate_per_minute=60000,submission_burst=100,submission_tokens=6000000000,recipient_daily_limit=1000,pending_message_limit=1000 WHERE id=$1`, app)
	digest := sha256.Sum256([]byte("key-" + app))
	f.exec(`INSERT INTO api_keys(id,digest,app_id) VALUES ($1,$2,$3)`, f.id(), digest[:], app)
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, app)
	authed, err := f.repo.LookupKey(ctx, digest)
	if err != nil || !authed.DefaultTrackClicks || authed.DefaultTrackOpens {
		t.Fatal("lookup", authed, err)
	}

	keyFile := filepath.Join(t.TempDir(), "keyring.json")
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	if err = os.WriteFile(keyFile, []byte(fmt.Sprintf(`{"active":"fixture","keys":{"fixture":%q}}`, encoded)), 0600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	store, err := objectstore.NewEncrypted(&trackingObjects{values: map[string][]byte{}}, ring)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := tracking.NewProtectedStore(store)
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{DB: f.repo.DB, Generation: f.repo.Generation, Objects: store}
	if _, err = f.repo.SetTrackingQuota(ctx, "operator", app, 100); err != nil {
		t.Fatal(err)
	}
	service, err := acceptance.New(repo, store, []byte("0123456789abcdef0123456789abcdef"), allowAllRecipients{})
	if err != nil {
		t.Fatal(err)
	}
	service.ApplyTrackingDefaults = true

	const destination = "https://example.net/private/order-4417"
	request := func(key string, clicks *bool) submission.Request {
		m := submission.Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", HTML: `<p><a href="` + destination + `">order</a></p>`}
		m.RequestTracking(nil, clicks)
		return submission.Request{Adapter: "resend", IdempotencyKey: key, Message: m}
	}
	off := false
	defaulted, err := service.Accept(ctx, authed, request("defaulted", nil))
	if err != nil {
		t.Fatal(err)
	}
	optedOut, err := service.Accept(ctx, authed, request("opted-out", &off))
	if err != nil {
		t.Fatal(err)
	}

	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	linkKeys, err := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	prep := &preparation.Service{Repository: repo, Objects: store, Keys: fixtureSigningKeys{signer},
		Tracking: &preparation.TrackingConfig{Origin: "https://tracking.example.com", Links: linkKeys, EventLimit: 10, Objects: protected}}
	for i := 0; i < 2; i++ {
		if err = prep.PrepareNext(ctx); err != nil {
			t.Fatal("prepare", err)
		}
	}
	mime := func(message string) string {
		var key string
		if err := f.owner.QueryRowContext(ctx, `SELECT final_object_key FROM deliveries WHERE message_id=$1`, message).Scan(&key); err != nil {
			t.Fatal(err)
		}
		raw, err := store.Get(ctx, key, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		// Undo quoted-printable soft line breaks before searching.
		return strings.ReplaceAll(string(raw), "=\r\n", "")
	}
	tracked := mime(defaulted.ID)
	if !strings.Contains(tracked, "https://tracking.example.com/t/c/") || strings.Contains(tracked, "order-4417") || strings.Contains(tracked, "example.net/private") {
		t.Fatal("defaulted click tracking did not rewrite the link")
	}
	if strings.Contains(tracked, "/t/o/") {
		t.Fatal("an opens default that is off added an open pixel")
	}
	if got := f.count(`SELECT count(*) FROM tracking_links l JOIN deliveries d ON d.id=l.delivery_id WHERE d.message_id=$1`, defaulted.ID); got != 1 {
		t.Fatal("tracked delivery has no committed link", got)
	}
	plain := mime(optedOut.ID)
	if !strings.Contains(plain, destination) || strings.Contains(plain, "tracking.example.com") {
		t.Fatal("explicit clicks off was rewritten")
	}
	if got := f.count(`SELECT count(*) FROM tracking_links l JOIN deliveries d ON d.id=l.delivery_id WHERE d.message_id=$1`, optedOut.ID); got != 0 {
		t.Fatal("opted-out delivery has tracking links", got)
	}
}
