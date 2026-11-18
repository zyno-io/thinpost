package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/postgres"
)

// Recovery commands require a maintenance database role. They do not fence
// workers, isolate a restored database, or verify an operator's attestations.
func recovery(args []string) error {
	if len(args) == 0 {
		return errors.New("missing recovery operation")
	}
	operation := args[0]
	flags := flag.NewFlagSet("recovery "+operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	generation := flags.String("generation", "", "new independent deployment generation")
	run := flags.String("recovery-id", "", "recovery identifier")
	actor := flags.String("actor", "", "responsible operator")
	revision := flags.Int64("expected-revision", 0, "expected gate revision")
	fenced := flags.Bool("workers-fenced", false, "old edge workers, controllers, webhook dispatchers and collectors externally fenced")
	isolated := flags.Bool("database-isolated", false, "restored database isolated from old runtimes")
	reviewed := flags.Bool("evidence-reviewed", false, "recovery evidence reviewed")
	objectsFrozen := flags.Bool("objects-frozen", false, "restored bucket and keyring are stable and collectors fenced")
	afterKind := flags.String("after-kind", "", "report cursor kind")
	afterID := flags.String("after-id", "", "report cursor entity ID")
	afterSecondary := flags.String("after-secondary", "", "report cursor secondary ID")
	kind := flags.String("kind", "", "held entity kind")
	entityID := flags.String("entity-id", "", "held entity ID")
	secondaryID := flags.String("secondary-id", "", "held entity secondary ID")
	afterRecoveryID := flags.String("after-recovery-id", "", "hold history cursor")
	limit := flags.Int("limit", 100, "maximum holds per report page")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return errors.New("invalid recovery arguments")
	}
	if operation != "inspect" && operation != "begin" && operation != "quarantine" && operation != "open" && operation != "report" && operation != "objects" && operation != "hold-history" && operation != "migrate-restored-schema" {
		return errors.New("unknown recovery operation")
	}
	if operation == "begin" && (!*fenced || !*isolated) {
		return errors.New("recovery begin requires worker fencing and database isolation attestations")
	}
	if operation == "open" && (!*fenced || !*isolated || !*reviewed) {
		return errors.New("recovery open requires fencing, isolation and evidence review attestations")
	}
	if operation == "migrate-restored-schema" && (!*fenced || !*isolated || *generation == "" || *run == "" || *actor == "" || *revision <= 0) {
		return errors.New("recovery migrate-restored-schema requires fencing, isolation, generation, recovery ID, actor and revision")
	}
	if operation == "report" && (*generation == "" || *run == "" || *revision <= 0 || *limit < 1 || *limit > 500) {
		return errors.New("recovery report requires generation, recovery ID, revision and a limit from 1 to 500")
	}
	if operation == "objects" && (!*fenced || !*isolated || !*objectsFrozen || *generation == "" || *run == "" || *revision <= 0 || *limit < 1 || *limit > 100 || *afterSecondary != "") {
		return errors.New("recovery objects requires fencing, isolation and frozen objects attestations, generation, recovery ID, revision and a limit from 1 to 100")
	}
	if operation == "hold-history" && (*generation == "" || *run == "" || *revision <= 0 || *kind == "" || *entityID == "" || *limit < 1 || *limit > 100 || *afterKind != "" || *afterID != "" || *afterSecondary != "") {
		return errors.New("recovery hold-history requires generation, recovery ID, revision, entity and a limit from 1 to 100")
	}
	dsn, err := controlapp.DatabaseURL(os.Getenv)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("database configuration failed")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if operation == "inspect" || operation == "begin" || operation == "migrate-restored-schema" {
		err = postgres.CheckRecoveryBootstrapSchema(ctx, db)
	} else {
		err = postgres.CheckRecoveryTargetSchema(ctx, db)
	}
	if err != nil {
		return errors.New("database schema check failed")
	}
	if operation == "report" {
		page, reportErr := postgres.RecoveryReportPage(ctx, db, *generation, *run, *revision, postgres.RecoveryCursor{Kind: *afterKind, EntityID: *afterID, SecondaryID: *afterSecondary}, *limit)
		if reportErr != nil {
			return errors.New("recovery report failed")
		}
		return json.NewEncoder(os.Stdout).Encode(page)
	}
	if operation == "hold-history" {
		entity := postgres.RecoveryCursor{Kind: *kind, EntityID: *entityID, SecondaryID: *secondaryID}
		page, historyErr := postgres.RecoveryHoldHistoryPage(ctx, db, *generation, *run, *revision, entity, *afterRecoveryID, *limit)
		if historyErr != nil {
			return errors.New("recovery hold history failed")
		}
		return json.NewEncoder(os.Stdout).Encode(page)
	}
	if operation == "objects" {
		config, configErr := controlapp.ObjectStorageConfig(os.Getenv, dsn)
		if configErr != nil || config == nil {
			return errors.New("object storage configuration failed")
		}
		client, clientErr := minio.New(config.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""), Secure: config.Secure, Region: config.Region})
		if clientErr != nil {
			return errors.New("object storage configuration failed")
		}
		keys, keyErr := objectstore.LoadKeyring(config.KeyringFile)
		if keyErr != nil {
			return errors.New("object encryption keyring initialization failed")
		}
		store, storeErr := objectstore.NewEncrypted(&objectstore.Store{Client: client, Bucket: config.Bucket}, keys)
		if storeErr != nil {
			return errors.New("object encryption initialization failed")
		}
		page, inventoryErr := postgres.RecoveryObjectInventoryPage(ctx, db, *generation, *run, *revision, postgres.RecoveryObjectCursor{Kind: *afterKind, ID: *afterID}, *limit, func(ctx context.Context, key string) error {
			body, readErr := store.Get(ctx, key, 16<<20)
			clear(body)
			return readErr
		})
		if inventoryErr != nil {
			return errors.New("recovery object inventory failed")
		}
		if err = json.NewEncoder(os.Stdout).Encode(page); err != nil {
			return err
		}
		if len(page.Failed) != 0 {
			return errors.New("recovery object inventory has failed references")
		}
		return nil
	}
	switch operation {
	case "begin":
		err = postgres.RecoveryBegin(ctx, db, *revision, *generation, *run, *actor)
	case "migrate-restored-schema":
		err = postgres.RecoveryMigrateRestoredSchema(ctx, db, *generation, *run, *actor, *revision)
	case "quarantine":
		for {
			var complete bool
			complete, err = postgres.RecoveryQuarantineBatch(ctx, db, *generation, *run, *actor)
			if err != nil || complete {
				break
			}
		}
	case "open":
		err = postgres.RecoveryOpen(ctx, db, *generation, *run, *actor, *revision)
	}
	if err != nil {
		// Database error text can quote input, so only fixed text is returned.
		if errors.Is(err, postgres.ErrRecoveryConflict) {
			return fmt.Errorf("recovery %s failed: %w", operation, postgres.ErrRecoveryConflict)
		}
		return errors.New("recovery " + operation + " failed")
	}
	status, err := postgres.RecoveryInspect(ctx, db)
	if err != nil {
		return errors.New("recovery inspection failed")
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
