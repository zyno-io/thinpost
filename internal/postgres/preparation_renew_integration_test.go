package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/preparation"
)

func TestRenewPreparationWithPostgres(t *testing.T) {
	f := newWebhookFixture(t, "prepren_")
	ctx := f.ctx
	app := f.app("renew")
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, app)
	message := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,'submissions/renew',true,'example.com','com.example.','s',1)`, message, app)
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error { return nil })
	until := func() time.Time {
		var v time.Time
		if err := f.owner.QueryRowContext(ctx, `SELECT prepare_until FROM messages WHERE id=$1`, message).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	token := f.id()
	claim, err := f.repo.ClaimPreparation(ctx, token, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first := until()

	wrong := claim
	wrong.Token = f.id()
	if err = f.repo.RenewPreparation(ctx, wrong, time.Minute); !errors.Is(err, preparation.ErrLostLease) {
		t.Fatal("wrong token renewed", err)
	}
	wrong = claim
	wrong.AppID = f.id()
	if err = f.repo.RenewPreparation(ctx, wrong, time.Minute); !errors.Is(err, preparation.ErrLostLease) {
		t.Fatal("wrong app renewed", err)
	}
	if !until().Equal(first) {
		t.Fatal("rejected renewal changed the lease")
	}
	if err = f.repo.RenewPreparation(ctx, claim, time.Second); err == nil {
		t.Fatal("out of range ttl accepted")
	}

	// A claim renewed past its original deadline is not reclaimed as expired
	// and still commits.
	time.Sleep(3 * time.Second)
	if err = f.repo.RenewPreparation(ctx, claim, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if !until().After(first.Add(2 * time.Second)) {
		t.Fatal("renewal did not extend prepare_until")
	}
	time.Sleep(3 * time.Second)
	if !time.Now().After(first) {
		t.Fatal("test did not pass the original deadline")
	}
	if _, err = f.repo.ClaimPreparation(ctx, f.id(), 5*time.Second); !errors.Is(err, preparation.ErrNoWork) {
		t.Fatal("unexpected claim", err)
	}
	var state, owner string
	var attempts int
	if err = f.owner.QueryRowContext(ctx, `SELECT state,prepare_token,prepare_attempts FROM messages WHERE id=$1`, message).Scan(&state, &owner, &attempts); err != nil || state != "preparing" || owner != token || attempts != 1 {
		t.Fatal("renewed claim was swept", state, attempts, err)
	}
	claim.SigningDomain = "example.com"
	delivery := f.id()
	if err = f.repo.CommitPreparation(ctx, claim, []preparation.Delivery{{ID: delivery, RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", delivery)}}); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.RenewPreparation(ctx, claim, time.Minute); !errors.Is(err, preparation.ErrLostLease) {
		t.Fatal("finished claim renewed", err)
	}

	// An expired claim is lost and is not revived.
	second := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,'submissions/renew2',true,'example.com','com.example.','s',1)`, second, app)
	claim, err = f.repo.ClaimPreparation(ctx, f.id(), 30*time.Second)
	if err != nil || claim.MessageID != second {
		t.Fatal(claim, err)
	}
	f.exec(`UPDATE messages SET prepare_until=clock_timestamp()-interval '1 second' WHERE id=$1`, second)
	if err = f.repo.RenewPreparation(ctx, claim, time.Minute); !errors.Is(err, preparation.ErrLostLease) {
		t.Fatal("expired claim renewed", err)
	}
	var expired bool
	if err = f.owner.QueryRowContext(ctx, `SELECT prepare_until<clock_timestamp() FROM messages WHERE id=$1`, second).Scan(&expired); err != nil || !expired {
		t.Fatal("expired claim was revived", err)
	}
	claim.SigningDomain = "example.com"
	late := f.id()
	if err = f.repo.CommitPreparation(ctx, claim, []preparation.Delivery{{ID: late, RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", late)}}); !errors.Is(err, preparation.ErrLostLease) {
		t.Fatal("expired claim committed", err)
	}
}
