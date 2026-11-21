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
			`INSERT INTO sending_ips(id,address,helo_name) VALUES ('` + strings.Repeat("a", 32) + `','198.51.100.7','smtp.grant.example')`,
			`UPDATE sending_ips SET helo_name='smtp.grant2.example' WHERE false`,
			`DELETE FROM sending_ips WHERE helo_name='smtp.grant.example'`,
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

	t.Run("the name, bounce domain and report-name columns are gone", func(t *testing.T) {
		var columns int
		if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND
			((table_name='sending_ips' AND column_name IN ('name','bounce_domain')) OR (table_name='edge_source_reports' AND column_name='bounce_domains'))`, schema).Scan(&columns); err != nil || columns != 0 {
			t.Fatal("removed columns still exist", columns, err)
		}
	})

	const primaryAddr, hostingAddr, thirdAddr = "45.0.1.22", "45.0.0.10", "45.0.0.11"
	const primaryName, hostingName, thirdName = "mx.example.net", "smtp.hosting.example.org", "smtp.third.example.net"
	prim, host, third := netip.MustParseAddr(primaryAddr), netip.MustParseAddr(hostingAddr), netip.MustParseAddr(thirdAddr)
	primarySource, hostingSource, thirdSource := delivery.Source{Address: prim, Hostname: primaryName}, delivery.Source{Address: host, Hostname: hostingName}, delivery.Source{Address: third, Hostname: thirdName}
	// reported is what a worker reports in Ready: each address with its name.
	reported := func(sources ...delivery.Source) delivery.EdgeCapabilities {
		return delivery.EdgeCapabilities{Sources: sources}
	}
	lease, err := repo.AcquireControllerLease(ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	record := func(sources ...delivery.Source) {
		t.Helper()
		if err := repo.RecordEdgeSources(ctx, lease, reported(sources...)); err != nil {
			t.Fatal(err)
		}
	}
	// rowID is the id of the row for an address ("" when there is none).
	rowID := func(address string) string {
		var rid string
		if err := owner.QueryRowContext(ctx, `SELECT COALESCE((SELECT id FROM sending_ips WHERE address=$1::text::inet),'')`, address).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		return rid
	}
	rowCount := func() (n int) {
		if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM sending_ips`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	app := func(name string) string {
		a := id()
		exec(`INSERT INTO apps(id,name) VALUES ($1,$2)`, a, name)
		return a
	}
	appNull, appHosting, appPrimary := app("Null"), app("Hosting"), app("Primary")

	t.Run("before any report there are no rows and nothing to pin", func(t *testing.T) {
		list, err := repo.ListSendingIPs(ctx)
		if err != nil || list.EdgeReportedAt != nil || list.EdgeReportsIdentity || len(list.IPs) != 0 || list.UnpinnedAppCount != 3 {
			t.Fatal("before any report", list, err)
		}
		unknown := id()
		if _, err = repo.SetAppSendingIP(ctx, "operator", appNull, &unknown); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown sending ip", err)
		}
	})

	t.Run("a report writes a row per address, the main IP first", func(t *testing.T) {
		if err := repo.RecordEdgeSources(ctx, ControllerLease{WorkerID: "worker", ControllerID: "controller", Epoch: lease.Epoch + 1}, reported(primarySource)); !errors.Is(err, delivery.ErrLeaseInvalid) {
			t.Fatal("a non-live lease wrote a report", err)
		}
		if rowCount() != 0 {
			t.Fatal("a refused report wrote rows")
		}
		record(hostingSource, primarySource)
		list, err := repo.ListSendingIPs(ctx)
		if err != nil || list.EdgeReportedAt == nil || !list.EdgeReportsIdentity || len(list.IPs) != 2 || list.EdgePrimaryAddress == nil || *list.EdgePrimaryAddress != hostingAddr {
			t.Fatal("after report", list, err)
		}
		if first, second := list.IPs[0], list.IPs[1]; first.Address != hostingAddr || first.HeloName != hostingName || !first.IsPrimary || first.OnEdge == nil || !*first.OnEdge ||
			second.Address != primaryAddr || second.HeloName != primaryName || second.IsPrimary || second.OnEdge == nil || !*second.OnEdge {
			t.Fatalf("rows %+v", list.IPs)
		}
		// Reporting the same set again changes nothing.
		before := rowID(hostingAddr)
		record(hostingSource, primarySource)
		if rowCount() != 2 || rowID(hostingAddr) != before {
			t.Fatal("a repeated report changed the rows")
		}
	})

	t.Run("a stale epoch writes neither the report nor rows", func(t *testing.T) {
		exec(`UPDATE edge_source_reports SET epoch=$1 WHERE worker_id='worker'`, lease.Epoch+10)
		record(primarySource, thirdSource)
		if rowID(thirdAddr) != "" {
			t.Fatal("a stale report wrote a row")
		}
		var epoch uint64
		if err := owner.QueryRowContext(ctx, `SELECT epoch FROM edge_source_reports WHERE worker_id='worker'`).Scan(&epoch); err != nil || epoch != lease.Epoch+10 {
			t.Fatal("a stale report overwrote a newer one", epoch, err)
		}
		exec(`UPDATE edge_source_reports SET epoch=$1 WHERE worker_id='worker'`, lease.Epoch)
	})

	t.Run("assigning checks the Edge is reporting the IP", func(t *testing.T) {
		hostingRow, primaryRow := rowID(hostingAddr), rowID(primaryAddr)
		got, err := repo.GetAppSendingIP(ctx, appNull)
		if err != nil || got.SendingIPID != nil || got.Address != nil || got.HeloName != nil {
			t.Fatal("default assignment", got, err)
		}
		set, err := repo.SetAppSendingIP(ctx, "operator", appHosting, &hostingRow)
		if err != nil || set.SendingIPID == nil || *set.Address != hostingAddr || *set.HeloName != hostingName {
			t.Fatal("assign", set, err)
		}
		if _, err = repo.SetAppSendingIP(ctx, "operator", appHosting, &hostingRow); err != nil {
			t.Fatal(err)
		}
		var audits int
		owner.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action='app.sending_ip.updated' AND app_id=$1`, appHosting).Scan(&audits)
		if audits != 1 {
			t.Fatal("identical assignment was audited", audits)
		}
		if _, err = repo.SetAppSendingIP(ctx, "operator", appPrimary, &primaryRow); err != nil {
			t.Fatal(err)
		}
		if _, err = repo.SetAppSendingIP(ctx, "operator", id(), &hostingRow); !errors.Is(err, admin.ErrNotFound) {
			t.Fatal("unknown app", err)
		}
		created, err := repo.CreateAppWithSendingIP(ctx, "operator", "Created", &hostingRow)
		if err != nil || created.SendingIPID == nil || *created.SendingIPID != hostingRow {
			t.Fatal("create with IP", created, err)
		}
		apps, err := repo.ListApps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listed := 0
		for _, a := range apps {
			if a.ID == appHosting && a.SendingIPID != nil && *a.SendingIPID == hostingRow || a.ID == appNull && a.SendingIPID == nil {
				listed++
			}
		}
		if listed != 2 {
			t.Fatal("ListApps does not report the assignment")
		}
		// A row the Edge stopped reporting (kept because an app is pinned to it)
		// cannot be chosen again, for an existing app or a new one.
		record(primarySource)
		if rowID(hostingAddr) != hostingRow {
			t.Fatal("a pinned row was pruned")
		}
		if _, err = repo.SetAppSendingIP(ctx, "operator", appNull, &hostingRow); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("pinned to an IP the Edge does not report", err)
		}
		if _, err = repo.CreateAppWithSendingIP(ctx, "operator", "Nope", &hostingRow); !errors.Is(err, admin.ErrConflict) {
			t.Fatal("created on an IP the Edge does not report", err)
		}
		// The identical choice is still accepted, and unpinning is always allowed.
		if _, err = repo.SetAppSendingIP(ctx, "operator", appHosting, &hostingRow); err != nil {
			t.Fatal("identical choice refused", err)
		}
		back, err := repo.SetAppSendingIP(ctx, "operator", created.ID, nil)
		if err != nil || back.SendingIPID != nil {
			t.Fatal("unassign", back, err)
		}
		exec(`DELETE FROM audit_events WHERE app_id=$1`, created.ID)
		exec(`DELETE FROM apps WHERE id=$1`, created.ID)
		list, err := repo.ListSendingIPs(ctx)
		if err != nil || len(list.IPs) != 2 {
			t.Fatal("list", list, err)
		}
		if first, second := list.IPs[0], list.IPs[1]; !first.IsPrimary || first.Address != primaryAddr || first.OnEdge == nil || !*first.OnEdge || first.AppCount != 1 ||
			second.Address != hostingAddr || second.OnEdge == nil || *second.OnEdge || second.IsPrimary || second.AppCount != 1 {
			t.Fatalf("rows after the Edge dropped an IP: %+v", list.IPs)
		}
		record(primarySource, hostingSource)
	})

	t.Run("reports refresh names and prune only unpinned unreported rows", func(t *testing.T) {
		hostingRow, primaryRow := rowID(hostingAddr), rowID(primaryAddr)
		record(primarySource, thirdSource)
		if rowID(thirdAddr) == "" {
			t.Fatal("a new address was not recorded")
		}
		// Renaming and reordering in the report never move an app or change an address.
		record(delivery.Source{Address: host, Hostname: "smtp.renamed.example.org"}, thirdSource, primarySource)
		var name string
		if err := owner.QueryRowContext(ctx, `SELECT helo_name FROM sending_ips WHERE id=$1`, hostingRow).Scan(&name); err != nil || name != "smtp.renamed.example.org" {
			t.Fatal("name not refreshed", name, err)
		}
		if rowID(hostingAddr) != hostingRow || rowID(primaryAddr) != primaryRow {
			t.Fatal("a row changed id")
		}
		pins := func() map[string]string {
			out := map[string]string{}
			rows, err := owner.QueryContext(ctx, `SELECT id,COALESCE(sending_ip_id,'') FROM apps WHERE id=ANY($1)`, "{"+appNull+","+appHosting+","+appPrimary+"}")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var a, p string
				if err = rows.Scan(&a, &p); err != nil {
					t.Fatal(err)
				}
				out[a] = p
			}
			return out
		}
		if p := pins(); p[appHosting] != hostingRow || p[appPrimary] != primaryRow || p[appNull] != "" {
			t.Fatal("a report moved an app", p)
		}
		// The unpinned third row goes when it is no longer reported; the pinned
		// hosting row stays; the apps are untouched.
		record(primarySource)
		if rowID(thirdAddr) != "" || rowID(hostingAddr) != hostingRow || rowCount() != 2 {
			t.Fatal("prune", rowCount())
		}
		if p := pins(); p[appHosting] != hostingRow || p[appPrimary] != primaryRow || p[appNull] != "" {
			t.Fatal("a prune moved an app", p)
		}
		// A report with nothing in it prunes every unpinned row and keeps the pinned ones.
		record(primarySource, thirdSource)
		record()
		if rowID(thirdAddr) != "" || rowCount() != 2 {
			t.Fatal("an empty report", rowCount())
		}
		list, err := repo.ListSendingIPs(ctx)
		if err != nil || list.EdgeReportsIdentity || len(list.IPs) != 2 || list.IPs[0].OnEdge != nil || list.EdgeReportedAt == nil {
			t.Fatal("empty report list", list, err)
		}
		record(primarySource, hostingSource)
		var rename string
		owner.QueryRowContext(ctx, `SELECT helo_name FROM sending_ips WHERE id=$1`, hostingRow).Scan(&rename)
		if rename != hostingName {
			t.Fatal("name not refreshed back", rename)
		}
	})

	t.Run("a pin that wins the race with a prune keeps its row and the report still commits", func(t *testing.T) {
		record(primarySource, thirdSource)
		thirdRow := rowID(thirdAddr)
		racer := app("Racer")
		// The pin is open (uncommitted) when the report's prune reaches the row.
		pin, err := owner.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pin.ExecContext(ctx, `UPDATE apps SET sending_ip_id=$2 WHERE id=$1`, racer, thirdRow); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- repo.RecordEdgeSources(ctx, lease, reported(primarySource)) }()
		select {
		case err = <-done:
			t.Fatal("the report did not wait for the open pin", err)
		case <-time.After(500 * time.Millisecond):
		}
		if err = pin.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal("the report failed when a pin won the race", err)
		}
		var pinned string
		if err = owner.QueryRowContext(ctx, `SELECT sending_ip_id FROM apps WHERE id=$1`, racer).Scan(&pinned); err != nil || pinned != thirdRow || rowID(thirdAddr) != thirdRow {
			t.Fatal("the pin or its row was lost", pinned, err)
		}
		var reportedAddresses int
		if err = owner.QueryRowContext(ctx, `SELECT cardinality(addresses) FROM edge_source_reports WHERE worker_id='worker'`).Scan(&reportedAddresses); err != nil || reportedAddresses != 1 {
			t.Fatal("the report was not stored", reportedAddresses, err)
		}
		// Unpinned again, the next report prunes it.
		exec(`UPDATE apps SET sending_ip_id=NULL WHERE id=$1`, racer)
		record(primarySource)
		if rowID(thirdAddr) != "" {
			t.Fatal("the next report did not prune the row")
		}
		record(primarySource, hostingSource)
	})

	t.Run("an assignment racing a prune is never dangling", func(t *testing.T) {
		record(primarySource, thirdSource)
		row := rowID(thirdAddr)
		racer := app("Racer2")
		pinResult, reportResult := make(chan error, 1), make(chan error, 1)
		go func() { _, err := repo.SetAppSendingIP(ctx, "operator", racer, &row); pinResult <- err }()
		go func() { reportResult <- repo.RecordEdgeSources(ctx, lease, reported(primarySource)) }()
		pinErr, reportErr := <-pinResult, <-reportResult
		var pinned bool
		owner.QueryRowContext(ctx, `SELECT sending_ip_id IS NOT NULL FROM apps WHERE id=$1`, racer).Scan(&pinned)
		exists := rowID(thirdAddr) != ""
		// Either the pin won (row kept, report committed) or the prune won (the
		// pin is refused as unknown or unavailable); never a dangling reference,
		// and the report always commits.
		pinRefused := errors.Is(pinErr, admin.ErrNotFound) || errors.Is(pinErr, admin.ErrConflict)
		if reportErr != nil || pinErr == nil && (!pinned || !exists) || pinErr != nil && (!pinRefused || pinned) {
			t.Fatal("inconsistent race outcome", pinned, exists, pinErr, reportErr)
		}
		exec(`UPDATE apps SET sending_ip_id=NULL WHERE id=$1`, racer)
		record(primarySource, hostingSource)
	})

	t.Run("a report holds at most the sixteen addresses an Edge may advertise", func(t *testing.T) {
		sources := make([]delivery.Source, 0, 16)
		for i := 0; i < 16; i++ {
			sources = append(sources, delivery.Source{Address: netip.MustParseAddr(fmt.Sprintf("45.0.2.%d", i+1)), Hostname: fmt.Sprintf("smtp-%d.example.net", i)})
		}
		record(sources...)
		list, err := repo.ListSendingIPs(ctx)
		// The sixteen reported rows, plus the primary and hosting rows, which are
		// no longer reported but stay because apps are pinned to them.
		if err != nil || len(list.IPs) != 18 {
			t.Fatal("rows", len(list.IPs), err)
		}
		record(primarySource, hostingSource)
	})

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
		// The worker reports only its main IP: the hosting app's delivery must
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
		if err != nil || claim.DeliveryID != pinned || claim.SourceAddress != hostingAddr || claim.HeloName != hostingName || claim.BounceDomain != hostingName || claim.ResolvedAddress != hostingAddr || claim.SourceName != hostingName {
			t.Fatalf("pinned claim %+v %v", claim, err)
		}
		if source, ok := attemptSource(pinned); !ok || source.String != hostingAddr {
			t.Fatal("the attempt did not record its source", source)
		}
	})

	t.Run("the HELO and bounce domain are the reported name, not the stored row", func(t *testing.T) {
		reset()
		// The row stores the name from the last report; the live session's own
		// report wins.
		d := queue(appHosting)
		claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, delivery.Source{Address: host, Hostname: "smtp.reported.example.org"}))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != hostingAddr || claim.HeloName != "smtp.reported.example.org" || claim.BounceDomain != "smtp.reported.example.org" || claim.SourceName != "smtp.reported.example.org" {
			t.Fatalf("reported name %+v %v", claim, err)
		}
	})

	t.Run("an unpinned app is addressed explicitly from the main IP", func(t *testing.T) {
		reset()
		d := queue(appNull)
		claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, hostingSource))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != primaryAddr || claim.HeloName != primaryName || claim.BounceDomain != primaryName || claim.SourceName != primaryName || claim.ResolvedAddress != "" {
			t.Fatalf("unassigned claim %+v %v", claim, err)
		}
		if source, _ := attemptSource(d); source.String != primaryAddr {
			t.Fatal("an unassigned app's attempt did not record the main IP", source)
		}
		// Reordering the report changes which IP is the main one, and so where
		// unpinned apps send from; pinned apps do not move.
		reset()
		d = queue(appNull)
		claim, err = repo.ClaimDelivery(ctx, lease, id(), reported(hostingSource, primarySource))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != hostingAddr || claim.HeloName != hostingName {
			t.Fatalf("reordered claim %+v %v", claim, err)
		}
	})

	t.Run("an app pinned to the main IP is sent from it, by name", func(t *testing.T) {
		reset()
		d := queue(appPrimary)
		claim, err := repo.ClaimDelivery(ctx, lease, id(), reported(primarySource, hostingSource))
		if err != nil || claim.DeliveryID != d || claim.SourceAddress != primaryAddr || claim.HeloName != primaryName || claim.BounceDomain != primaryName || claim.SourceName != primaryName {
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

	t.Run("the commit reports the HELO name, never the address", func(t *testing.T) {
		reset()
		queue(appHosting)
		attempt := id()
		claim, err := repo.ClaimDelivery(ctx, lease, attempt, reported(primarySource, hostingSource))
		if err != nil {
			t.Fatal(err)
		}
		committed, err := repo.CommitDeliveryResultIdentified(ctx, delivery.Result{AttemptID: attempt, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"})
		if err != nil || committed.SourceName != hostingName {
			t.Fatal("committed", committed, err)
		}
		// An address whose row is gone is reported as removed.
		reset()
		queue(appNull)
		attempt = id()
		claim, err = repo.ClaimDelivery(ctx, lease, attempt, reported(primarySource))
		if err != nil || claim.SourceAddress != primaryAddr {
			t.Fatal(claim, err)
		}
		exec(`UPDATE apps SET sending_ip_id=NULL WHERE id=$1`, appPrimary)
		exec(`DELETE FROM sending_ips WHERE address=$1::text::inet`, primaryAddr)
		committed, err = repo.CommitDeliveryResultIdentified(ctx, delivery.Result{AttemptID: attempt, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"})
		if err != nil || committed.SourceName != "removed" {
			t.Fatal("removed", committed, err)
		}
		record(primarySource, hostingSource)
		// An attempt from before schema 45, with no recorded source, logs "primary".
		reset()
		queue(appNull)
		attempt = id()
		if _, err = repo.ClaimDelivery(ctx, lease, attempt, reported(primarySource)); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE delivery_attempts SET source_address=NULL WHERE id=$1`, attempt)
		var deliveryID string
		owner.QueryRowContext(ctx, `SELECT delivery_id FROM delivery_attempts WHERE id=$1`, attempt).Scan(&deliveryID)
		committed, err = repo.CommitDeliveryResultIdentified(ctx, delivery.Result{AttemptID: attempt, DeliveryID: deliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch, Outcome: "rejected"})
		if err != nil || committed.SourceName != "primary" {
			t.Fatal("primary", committed, err)
		}
	})
	reset()
}
