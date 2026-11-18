package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestBootstrapDedicatedDatabase(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL server with role and database creation")
	}
	ctx := context.Background()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(random[:])
	database, owner, runtime, pooler := "tp_db_"+suffix, "tp_owner_"+suffix, "tp_runtime_"+suffix, "tp_pooler_"+suffix
	for _, role := range []string{owner, runtime, pooler} {
		if _, err = root.ExecContext(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD 'disposable-bootstrap-test'"); err != nil {
			t.Fatal(err)
		}
		defer func(role string) {
			if _, err := root.ExecContext(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		}(role)
	}
	if _, err = root.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := root.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	rootDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rootDB.Close()
	u.User = url.UserPassword(owner, "disposable-bootstrap-test")
	ownerDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer ownerDB.Close()
	input := DatabaseBootstrap{Database: database, Schema: "thinpost", RuntimeRole: runtime, PoolerAuthRole: pooler}
	if err = BootstrapDatabase(ctx, rootDB, input); err == nil {
		t.Fatal("accepted superuser instead of the authenticated database owner")
	}
	wrong := input
	wrong.Database = "another_database"
	if err = BootstrapDatabase(ctx, ownerDB, wrong); err == nil {
		t.Fatal("accepted a different database")
	}
	if _, err = rootDB.ExecContext(ctx, `CREATE SCHEMA foreign_schema`); err != nil {
		t.Fatal(err)
	}
	wrong = input
	wrong.Schema = "foreign_schema"
	if err = BootstrapDatabase(ctx, ownerDB, wrong); err == nil {
		t.Fatal("accepted schema owned by another role")
	}
	var publicConnect bool
	if err = rootDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) a WHERE d.datname=current_database() AND a.grantee=0 AND a.privilege_type='CONNECT')`).Scan(&publicConnect); err != nil || !publicConnect {
		t.Fatal("refused bootstrap changed database privileges", err)
	}
	for i := 0; i < 2; i++ {
		if err = BootstrapDatabase(ctx, ownerDB, input); err != nil {
			t.Fatal("bootstrap failed", err)
		}
	}
	var runtimeConnect, poolerConnect, runtimeTemp bool
	err = rootDB.QueryRowContext(ctx, `SELECT has_database_privilege($1,current_database(),'CONNECT'), has_database_privilege($2,current_database(),'CONNECT'), has_database_privilege($1,current_database(),'TEMPORARY')`, runtime, pooler).Scan(&runtimeConnect, &poolerConnect, &runtimeTemp)
	if err != nil || !runtimeConnect || !poolerConnect || runtimeTemp {
		t.Fatal("wrong database access", runtimeConnect, poolerConnect, runtimeTemp, err)
	}
	query := u.Query()
	query.Set("search_path", input.Schema)
	u.RawQuery = query.Encode()
	appOwner, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer appOwner.Close()
	if err = Migrate(ctx, appOwner); err != nil {
		t.Fatal(err)
	}
	if err = GrantRuntimeRole(ctx, appOwner, runtime); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(runtime, "disposable-bootstrap-test")
	appRuntime, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer appRuntime.Close()
	if err = CheckRuntimeGatePrivileges(ctx, appRuntime); err != nil {
		t.Fatal("bootstrapped runtime privileges rejected", err)
	}
}
