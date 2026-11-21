package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
)

// A delivery is handed to a worker only from a source that worker reported.
// The cases below are the safety contract of the sending-IP claim.
func TestSendingIPsPostgres(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	schema := "sendingips_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	owner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	const generation = "00000000000000000000000000000ccc"
	counter := 0
	id := func() string { counter++; return fmt.Sprintf("%032x", counter+5000) }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation)
	db, cleanup := restrictedRuntimeForTest(t, ctx, owner, u.String())
	defer cleanup()
	repo := &Repository{DB: db, Generation: generation}

	t.Run("runtime grants", func(t *testing.T) {
		for _, statement := range []string{
			`INSERT INTO sending_ips(id,name,address,helo_name,bounce_domain) VALUES ('` + strings.Repeat("a", 32) + `','grant-test','198.51.100.7','smtp.grant.example','b.grant.example')`,
			`UPDATE sending_ips SET name='grant-test2' WHERE false`,
			`DELETE FROM sending_ips WHERE name='grant-test'`,
			`INSERT INTO edge_source_reports(worker_id,epoch,reported_at) VALUES ('grant-worker',1,now())`,
			`UPDATE edge_source_reports SET epoch=2 WHERE worker_id='grant-worker'`,
			`UPDATE apps SET sending_ip_id=NULL WHERE false`,
		} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		for _, statement := range []string{`TRUNCATE sending_ips`, `DELETE FROM edge_source_reports`} {
			if _, err := db.ExecContext(ctx, statement); err == nil {
				t.Fatalf("runtime role may run %s", statement)
			}
		}
		exec(`DELETE FROM edge_source_reports`)
	})

	// Addresses are TEST-NET-free public examples: the store applies the same
	// public-address rule as the Edge.
	const primaryAddr, hostingAddr, thirdAddr = "45.0.1.22", "45.0.0.10", "45.0.0.11"
	var primaryRow, hostingRow admin.SendingIP

	t.Run("create validates and deduplicates", func(t *testing.T) {
		for _, bad := range []admin.SendingIPInput{
			{Name: "primary", Address: hostingAddr, HeloName: "smtp.example.org", BounceDomain: "example.org"},
			{Name: "removed", Address: hostingAddr, HeloName: "smtp.example.org", BounceDomain: "example.org"},
			{Name: "Bad", Address: hostingAddr, HeloName: "smtp.example.org", BounceDomain: "example.org"},
			{Name: "ok", Address: "10.0.0.1", HeloName: "smtp.example.org", BounceDomain: "example.org"},
			{Name: "ok", Address: "2607:f8b0:4000::1", HeloName: "smtp.example.org", BounceDomain: "example.org"},
			{Name: "ok", Address: "203.0.113.4", HeloName: "smtp.example.org", BounceDomain: "example.org"},
			{Name: "ok", Address: hostingAddr, HeloName: "SMTP.example.org", BounceDomain: "example.org"},
			{Name: "ok", Address: hostingAddr, HeloName: "smtp.example.org", BounceDomain: ""},
		} {
			if _, err := repo.CreateSendingIP(ctx, "operator", bad); !errors.Is(err, admin.ErrInvalidInput) {
				t.Fatalf("accepted %+v: %v", bad, err)
			}
		}
		// The migration's CHECK refuses the reserved words even past the store.
		if _, err := owner.ExecContext(ctx, `INSERT INTO sending_ips(id,name,address,helo_name,bounce_domain) VALUES ('`+strings.Repeat("b", 32)+`','other','45.0.0.99','smtp.example.org','b.example.org')`); err == nil {
			t.Fatal("the database accepted a reserved name")
		}
		var err error
		primaryRow, err = repo.CreateSendingIP(ctx, "operator", admin.SendingIPInput{Name: "app-mail", Address: primaryAddr, HeloName: "smtp.mx.example.net", BounceDomain: "mx.example.net"})
		if err != nil || primaryRow.Address != primaryAddr {
			t.Fatal(err)
		}
		hostingRow, err = repo.CreateSendingIP(ctx, "operator", admin.SendingIPInput{Name: "hosting", Address: hostingAddr, HeloName: "smtp.hosting.example.org", BounceDomain: "hosting.example.org"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repo.CreateSendingIP(ctx, "operator", admin.SendingIPInput{Name: "hosting", Address: thirdAddr, HeloName: "smtp.example.org", BounceDomain: "example.org"}); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("duplicate name", err)
		}
		if _, err = repo.CreateSendingIP(ctx, "operator", admin.SendingIPInput{Name: "other-ip", Address: hostingAddr, HeloName: "smtp.example.org", BounceDomain: "example.org"}); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("duplicate address", err)
		}
		// The failed duplicates left no row or audit behind.
		var rows, audits int
		if err = owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM sending_ips),(SELECT count(*) FROM audit_events WHERE action='sending_ip.created' AND app_id IS NULL)`).Scan(&rows, &audits); err != nil || rows != 2 || audits != 2 {
			t.Fatal("rows/audits", rows, audits, err)
		}
	})

	t.Run("audit rows never hold an address", func(t *testing.T) {
		var leaked bool
		if err := owner.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events WHERE details::text LIKE '%45.0.1.%' OR details::text LIKE '%45.0.0.%' OR resource_id LIKE '%.%')`).Scan(&leaked); err != nil || leaked {
			t.Fatal("an address reached the audit log", err)
		}
	})

	t.Run("update", func(t *testing.T) {
		patch := admin.SendingIPPatch{HeloName: ptr("smtp.hosting.example.org")}
		if _, err := repo.UpdateSendingIP(ctx, "operator", hostingRow.ID, patch); err != nil {
			t.Fatal(err)
		}
		var audits int
		owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action='sending_ip.updated'`).Scan(&audits)
		if audits != 0 {
			t.Fatal("an identical edit was audited")
		}
		updated, err := repo.UpdateSendingIP(ctx, "operator", hostingRow.ID, admin.SendingIPPatch{Name: ptr("hosting-2"), BounceDomain: ptr("example.org")})
		if err == nil {
			_, err = repo.UpdateSendingIP(ctx, "operator", hostingRow.ID, admin.SendingIPPatch{BounceDomain: ptr("elsewhere.example.net")})
			if !errors.Is(err, admin.ErrInvalidInput) {
				t.Fatal("a bounce domain the HELO name is not under was accepted", err)
			}
			err = nil
		}
		if err != nil || updated.Name != "hosting-2" || updated.Address != hostingAddr {
			t.Fatal("edit", updated, err)
		}
		if _, err = repo.UpdateSendingIP(ctx, "operator", hostingRow.ID, admin.SendingIPPatch{Name: ptr("app-mail")}); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("name collision", err)
		}
		if _, err = repo.UpdateSendingIP(ctx, "operator", id(), admin.SendingIPPatch{Name: ptr("x")}); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown row", err)
		}
		// Put it back for the claim scenarios.
		if _, err = repo.UpdateSendingIP(ctx, "operator", hostingRow.ID, admin.SendingIPPatch{Name: ptr("hosting"), BounceDomain: ptr("hosting.example.org")}); err != nil {
			t.Fatal(err)
		}
	})

	app := func(name string) string {
		a := id()
		exec(`INSERT INTO apps(id,name) VALUES ($1,$2)`, a, name)
		return a
	}
	appNull, appHosting, appPrimary := app("Null"), app("Hosting"), app("Primary")
	t.Run("assign", func(t *testing.T) {
		got, err := repo.GetAppSendingIP(ctx, appNull)
		if err != nil || got.SendingIPID != nil {
			t.Fatal("default assignment", got, err)
		}
		set, err := repo.SetAppSendingIP(ctx, "operator", appHosting, &hostingRow.ID)
		if err != nil || set.SendingIPID == nil || *set.Name != "hosting" {
			t.Fatal("assign", set, err)
		}
		if _, err = repo.SetAppSendingIP(ctx, "operator", appHosting, &hostingRow.ID); err != nil {
			t.Fatal(err)
		}
		var audits int
		owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action='app.sending_ip.updated' AND app_id=$1`, appHosting).Scan(&audits)
		if audits != 1 {
			t.Fatal("identical assignment was audited", audits)
		}
		if _, err = repo.SetAppSendingIP(ctx, "operator", appPrimary, &primaryRow.ID); err != nil {
			t.Fatal(err)
		}
		unknown := id()
		if _, err = repo.SetAppSendingIP(ctx, "operator", appNull, &unknown); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown sending ip", err)
		}
		if _, err = repo.SetAppSendingIP(ctx, "operator", id(), &hostingRow.ID); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown app", err)
		}
		// Deleting an IP in use is refused, and the row stays.
		if err = repo.DeleteSendingIP(ctx, "operator", hostingRow.ID); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("deleted an IP that an app uses", err)
		}
		created, err := repo.CreateAppWithSendingIP(ctx, "operator", "Created", &hostingRow.ID)
		if err != nil || created.SendingIPID == nil || *created.SendingIPID != hostingRow.ID {
			t.Fatal("create with IP", created, err)
		}
		if _, err = repo.CreateAppWithSendingIP(ctx, "operator", "Nope", &unknown); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("create with unknown IP", err)
		}
		apps, err := repo.ListApps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listed := 0
		for _, a := range apps {
			if a.ID == appHosting && a.SendingIPID != nil && *a.SendingIPID == hostingRow.ID || a.ID == appNull && a.SendingIPID == nil {
				listed++
			}
		}
		if listed != 2 {
			t.Fatal("ListApps does not report the assignment")
		}
		// Back to the primary, then to the hosting IP again.
		back, err := repo.SetAppSendingIP(ctx, "operator", created.ID, nil)
		if err != nil || back.SendingIPID != nil {
			t.Fatal("unassign", back, err)
		}
		exec(`DELETE FROM audit_events WHERE app_id=$1`, created.ID)
		exec(`DELETE FROM apps WHERE id=$1`, created.ID)
	})

	lease, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// restorePrimary puts the primary's record back after a test removed it.
	restorePrimary := func() {
		exec(`INSERT INTO sending_ips(id,name,address,helo_name,bounce_domain) VALUES ($1,$2,$3::text::inet,$4,$5) ON CONFLICT DO NOTHING`, primaryRow.ID, primaryRow.Name, primaryRow.Address, primaryRow.HeloName, primaryRow.BounceDomain)
		exec(`UPDATE apps SET sending_ip_id=$2 WHERE id=$1`, appPrimary, primaryRow.ID)
	}
	prim, host := netip.MustParseAddr(primaryAddr), netip.MustParseAddr(hostingAddr)
	// reported is what a worker reports in Ready: each address with its name.
	const primaryName, hostingName = "mx.example.net", "smtp.hosting.example.org"
	reported := func(sources ...delivery.Source) delivery.EdgeCapabilities {
		return delivery.EdgeCapabilities{Sources: sources}
	}
	primarySource, hostingSource := delivery.Source{Address: prim, Hostname: primaryName}, delivery.Source{Address: host, Hostname: hostingName}
	// queue adds one queued delivery for appID and returns its ID.
	queue := func(appID string) string {
		m, d := id(), id()
		exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,$3,false,'prepared')`, m, appID, "source-"+m)
		exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,$3)`, d, m, "signed-"+d)
		return d
	}
	attemptSource := func(deliveryID string) (sql.NullString, bool) {
		var source sql.NullString
		err := owner.QueryRowContext(ctx, `SELECT host(source_address) FROM delivery_attempts WHERE delivery_id=$1`, deliveryID).Scan(&source)
		return source, err == nil
	}
	reset := func() {
		exec(`DELETE FROM webhook_events`)
		exec(`DELETE FROM delivery_attempts`)
		exec(`DELETE FROM deliveries`)
		exec(`DELETE FROM messages`)
	}

	t.Run("a pinned IP the worker did not report is never claimed", func(t *testing.T) {
		reset()
		pinned, free := queue(appHosting), queue(appNull)
		// The worker reports only its primary: the hosting app's delivery must
		// stay queued, while another app's delivery is claimed.
		claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource))
		if err != nil || claim.DeliveryID != free {
			t.Fatal("expected the unpinned app's delivery", claim.DeliveryID, err)
		}
		if _, err = repo.ClaimDelivery(ctx, lease, id(), reported(primarySource)); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("claimed a delivery whose source the worker did not report", err)
		}
		var state string
		if err = owner.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, pinned).Scan(&state); err != nil || state != "queued" {
			t.Fatal("delivery was not left queued", state, err)
		}
		// Once the address is reported it is claimed, from that address, with
		// the name reported for it as both HELO and bounce domain.
		claim, err = repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, hostingSource))
		if err != nil || claim.DeliveryID != pinned || claim.SourceAddress != hostingAddr || claim.HeloName != hostingName || claim.BounceDomain != hostingName || claim.ResolvedAddress != hostingAddr || claim.SourceName != "hosting" {
			t.Fatalf("pinned claim %+v %v", claim, err)
		}
		if source, ok := attemptSource(pinned); !ok || source.String != hostingAddr {
			t.Fatal("the attempt did not record its source", source)
		}
	})

	t.Run("the HELO and bounce domain are the reported name, not the stored record", func(t *testing.T) {
		reset()
		// The stored record names smtp.hosting.example.org and hosting.example.org;
		// what the worker reports for the address wins.
		d := queue(appHosting)
		claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, delivery.Source{Address: host, Hostname: "smtp.reported.example.org"}))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != hostingAddr || claim.HeloName != "smtp.reported.example.org" || claim.BounceDomain != "smtp.reported.example.org" {
			t.Fatalf("reported name %+v %v", claim, err)
		}
	})

	t.Run("unassigned apps use the primary's reported name and keep the empty source fields", func(t *testing.T) {
		reset()
		// The primary has a record (primaryRow) with its own bounce domain and a
		// different name; neither is used. The worker sends from its primary and
		// its own name, the return path is the primary's reported name, and the
		// attempt records no source.
		exec(`UPDATE sending_ips SET bounce_domain='bounce.other.example.net' WHERE id=$1`, primaryRow.ID)
		d := queue(appNull)
		claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, hostingSource))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != "" || claim.HeloName != "" || claim.BounceDomain != primaryName || claim.SourceName != "" || claim.ResolvedAddress != "" {
			t.Fatalf("unassigned claim %+v %v", claim, err)
		}
		if source, _ := attemptSource(d); source.Valid {
			t.Fatal("an unassigned app's attempt recorded a source")
		}
		exec(`UPDATE sending_ips SET bounce_domain=$2 WHERE id=$1`, primaryRow.ID, primaryRow.BounceDomain)
		// With no record for the primary at all it behaves the same.
		exec(`UPDATE apps SET sending_ip_id=NULL WHERE id=$1`, appPrimary)
		exec(`DELETE FROM sending_ips WHERE id=$1`, primaryRow.ID)
		reset()
		d = queue(appNull)
		claim, err = repo.ClaimDelivery(ctx, lease, id(), reported(primarySource))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != "" || claim.HeloName != "" || claim.BounceDomain != primaryName {
			t.Fatalf("unrecorded primary %+v %v", claim, err)
		}
		restorePrimary()
	})

	t.Run("an app pinned to the primary is sent from it, by name", func(t *testing.T) {
		reset()
		d := queue(appPrimary)
		claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, hostingSource))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != primaryAddr || claim.HeloName != primaryName || claim.BounceDomain != primaryName || claim.SourceName != "app-mail" {
			t.Fatalf("primary-pinned claim %+v %v", claim, err)
		}
		if source, _ := attemptSource(d); source.String != primaryAddr {
			t.Fatal("primary not recorded")
		}
	})

	t.Run("a worker that reported no sources is given nothing, and the mail waits", func(t *testing.T) {
		reset()
		pinnedHosting, unassigned, pinnedPrimary := queue(appHosting), queue(appNull), queue(appPrimary)
		if _, err := repo.ClaimDelivery(ctx, lease, id(), delivery.EdgeCapabilities{}); !errors.Is(err, delivery.ErrNoWork) {
			t.Fatal("claimed with no reported source", err)
		}
		var queued int
		if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE state='queued' AND id=ANY($1)`, "{"+pinnedHosting+","+unassigned+","+pinnedPrimary+"}").Scan(&queued); err != nil || queued != 3 {
			t.Fatal("deliveries did not stay queued", queued, err)
		}
		// Once it reports, everything flows, each from its own address.
		seen := map[string]delivery.Claim{}
		for {
			claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, hostingSource))
			if errors.Is(err, delivery.ErrNoWork) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			seen[claim.DeliveryID] = claim
		}
		if len(seen) != 3 || seen[pinnedHosting].BounceDomain != hostingName || seen[unassigned].BounceDomain != primaryName || seen[pinnedPrimary].BounceDomain != primaryName {
			t.Fatalf("claims after reporting: %+v", seen)
		}
	})

	t.Run("the commit reports the IP name, never the address", func(t *testing.T) {
		reset()
		queue(appHosting)
		attempt := id()
		claim, err := repo.ClaimDelivery(ctx, lease, attempt, reported(primarySource, hostingSource))
		if err != nil {
			t.Fatal(err)
		}
		committed, err := repo.CommitDeliveryResultIdentified(ctx, delivery.Result{AttemptID: attempt, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"})
		if err != nil || committed.SourceName != "hosting" {
			t.Fatal("committed", committed, err)
		}
		// A deleted IP is reported as removed.
		reset()
		queue(appPrimary)
		attempt = id()
		claim, err = repo.ClaimDelivery(ctx, lease, attempt, reported(primarySource))
		if err != nil || claim.SourceAddress != primaryAddr {
			t.Fatal(claim, err)
		}
		exec(`UPDATE apps SET sending_ip_id=NULL WHERE id=$1`, appPrimary)
		exec(`DELETE FROM sending_ips WHERE id=$1`, primaryRow.ID)
		committed, err = repo.CommitDeliveryResultIdentified(ctx, delivery.Result{AttemptID: attempt, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"})
		if err != nil || committed.SourceName != "removed" {
			t.Fatal("removed", committed, err)
		}
		restorePrimary()
		// A delivery sent from the primary with no named source logs "primary".
		reset()
		queue(appNull)
		attempt = id()
		claim, err = repo.ClaimDelivery(ctx, lease, attempt, reported(primarySource))
		if err != nil {
			t.Fatal(err)
		}
		committed, err = repo.CommitDeliveryResultIdentified(ctx, delivery.Result{AttemptID: attempt, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"})
		if err != nil || committed.SourceName != "primary" {
			t.Fatal("primary", committed, err)
		}
	})
	reset()

	t.Run("edge reports are written only under the live lease", func(t *testing.T) {
		list, err := repo.ListSendingIPs(ctx)
		if err != nil || list.EdgeReportedAt != nil || list.EdgeReportsSources || len(list.IPs) != 2 || list.IPs[0].OnEdge != nil {
			t.Fatal("before any report", list, err)
		}
		if list.UnpinnedAppCount != 1 {
			t.Fatal("unpinned apps", list.UnpinnedAppCount)
		}
		caps := reported(primarySource, delivery.Source{Address: netip.MustParseAddr(thirdAddr), Hostname: "smtp.third.example.net"})
		if err = repo.RecordEdgeSources(ctx, ControllerLease{WorkerID: "worker", ControllerID: "controller", Epoch: lease.Epoch + 1}, caps); !errors.Is(err, delivery.ErrLeaseInvalid) {
			t.Fatal("a stale lease wrote a report", err)
		}
		if err = repo.RecordEdgeSources(ctx, lease, caps); err != nil {
			t.Fatal(err)
		}
		list, err = repo.ListSendingIPs(ctx)
		if err != nil || list.EdgeReportedAt == nil || !list.EdgeReportsSources {
			t.Fatal("after report", list, err)
		}
		byName := map[string]admin.SendingIP{}
		for _, ip := range list.IPs {
			byName[ip.Name] = ip
		}
		if ip := byName["app-mail"]; ip.OnEdge == nil || !*ip.OnEdge || !ip.IsPrimary || ip.BounceOnEdge == nil || !*ip.BounceOnEdge {
			t.Fatalf("primary %+v", ip)
		}
		if ip := byName["hosting"]; ip.OnEdge == nil || *ip.OnEdge || ip.IsPrimary || *ip.BounceOnEdge || ip.AppCount != 1 {
			t.Fatalf("hosting %+v", ip)
		}
		if len(list.UnnamedEdgeAddresses) != 1 || list.UnnamedEdgeAddresses[0] != thirdAddr {
			t.Fatal("unnamed addresses", list.UnnamedEdgeAddresses)
		}
		// An older worker (no sources) makes everything unknown again.
		if err = repo.RecordEdgeSources(ctx, lease, delivery.EdgeCapabilities{}); err != nil {
			t.Fatal(err)
		}
		list, err = repo.ListSendingIPs(ctx)
		if err != nil || list.EdgeReportsSources || list.IPs[0].OnEdge != nil || list.EdgeReportedAt == nil {
			t.Fatal("legacy report", list, err)
		}
		// A stale epoch cannot overwrite a newer report.
		newer, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err = repo.RecordEdgeSources(ctx, newer, caps); err != nil {
			t.Fatal(err)
		}
		lease = newer
		var epoch uint64
		if err = owner.QueryRowContext(ctx, `SELECT epoch FROM edge_source_reports WHERE worker_id='worker'`).Scan(&epoch); err != nil || epoch != newer.Epoch {
			t.Fatal("epoch", epoch, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := repo.DeleteSendingIP(ctx, "operator", id()); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown", err)
		}
		exec(`UPDATE apps SET sending_ip_id=NULL`)
		if err := repo.DeleteSendingIP(ctx, "operator", hostingRow.ID); err != nil {
			t.Fatal("unused IP", err)
		}
		var rows int
		owner.QueryRowContext(ctx, `SELECT count(*) FROM sending_ips`).Scan(&rows)
		if rows != 1 {
			t.Fatal("row not deleted")
		}
		var audits int
		owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action='sending_ip.deleted' AND app_id IS NULL`).Scan(&audits)
		if audits != 1 {
			t.Fatal("delete not audited")
		}
	})

	t.Run("an assignment racing a delete is refused", func(t *testing.T) {
		row, err := repo.CreateSendingIP(ctx, "operator", admin.SendingIPInput{Name: "race", Address: "45.0.0.20", HeloName: "smtp.race.example.org", BounceDomain: "race.example.org"})
		if err != nil {
			t.Fatal(err)
		}
		racer := app("Racer")
		results := make(chan error, 2)
		go func() { _, err := repo.SetAppSendingIP(ctx, "operator", racer, &row.ID); results <- err }()
		go func() { results <- repo.DeleteSendingIP(ctx, "operator", row.ID) }()
		first, second := <-results, <-results
		var pinned bool
		owner.QueryRowContext(ctx, `SELECT sending_ip_id IS NOT NULL FROM apps WHERE id=$1`, racer).Scan(&pinned)
		var exists bool
		owner.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sending_ips WHERE id=$1)`, row.ID).Scan(&exists)
		// Either the assignment won (delete refused, row kept) or the delete won
		// (assignment refused as unknown); never a dangling reference.
		if pinned && !exists || pinned && !(errors.Is(first, admin.ErrConflict) || errors.Is(second, admin.ErrConflict)) || !pinned && exists && first != nil && second != nil {
			t.Fatal("inconsistent race outcome", pinned, exists, first, second)
		}
	})
}

func ptr(value string) *string { return &value }
