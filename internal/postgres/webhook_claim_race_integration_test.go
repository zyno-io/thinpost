package postgres

// A delete racing an uncommitted claim must finish that claim's attempt.

import (
	"testing"
	"time"
)

func TestWebhookDeleteDuringClaimPostgres(t *testing.T) {
	f := newWebhookFixture(t, "wh_claim_race_")
	app := f.app("race")
	item := f.create(app, "https://race.example.com/")
	event := f.event(app)
	f.exec("INSERT INTO webhook_deliveries(event_id,subscription_id,app_id) VALUES ($1,$2,$3)", event, item.ID, app)
	f.exec("CREATE FUNCTION claim_slow() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1.5); RETURN NEW; END $$")
	f.exec("CREATE TRIGGER claim_slow AFTER INSERT ON webhook_attempts FOR EACH ROW EXECUTE FUNCTION claim_slow()")
	token := f.id()
	claimDone := make(chan error, 1)
	go func() {
		_, err := f.repo.ClaimWebhook(f.ctx, token, time.Minute)
		claimDone <- err
	}()
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	if err := f.repo.DeleteWebhook(f.ctx, "operator", app, item.ID); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("delete did not wait for the racing claim")
	}
	if err := <-claimDone; err != nil {
		t.Fatal("claim", err)
	}
	var delivery, attempt string
	if err := f.owner.QueryRowContext(f.ctx, "SELECT state FROM webhook_deliveries WHERE event_id=$1", event).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.QueryRowContext(f.ctx, "SELECT state FROM webhook_attempts WHERE id=$1", token).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if delivery != "cancelled" || attempt != "unknown" {
		t.Fatalf("delivery=%s attempt=%s; want cancelled/unknown", delivery, attempt)
	}
}
