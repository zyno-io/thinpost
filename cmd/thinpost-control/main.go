// thinpost-control currently serves the operator API and, when configured,
// the built operator console on the same origin. It must not be deployed for mail delivery until the remaining readiness gates are complete.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/controlbridge"
	"github.com/zyno-io/thinpost/internal/envguard"
	"github.com/zyno-io/thinpost/internal/httpapi"
	"github.com/zyno-io/thinpost/internal/messagebody"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/operatorauth"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/recipientpolicy"
	"github.com/zyno-io/thinpost/internal/signing"
	"github.com/zyno-io/thinpost/internal/smtpintake"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
	"github.com/zyno-io/thinpost/internal/webhook"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if err := envguard.RefuseGRPCBinaryLog(os.Getenv); err != nil {
		return err
	}
	if len(os.Args) > 1 {
		if len(os.Args) == 2 && os.Args[1] == "migrate" {
			return migrate()
		}
		if os.Args[1] == "recovery" {
			return recovery(os.Args[2:])
		}
		if os.Args[1] == "retry-policy" {
			return retryPolicy(os.Args[2:])
		}
		if os.Args[1] == "deploy-migrate" {
			return deployMigrate(os.Args[2:])
		}
		if os.Args[1] == "deploy-reopen" {
			return deployReopen(os.Args[2:])
		}
		if os.Args[1] == "gate" {
			return gate(os.Args[2:])
		}
		if os.Args[1] == "grant-runtime" {
			return grantRuntime(os.Args[2:])
		}
		if os.Args[1] == "grant-collector" {
			return grantCollector(os.Args[2:])
		}
		if os.Args[1] == "grant-tracking-retirement" {
			return grantTrackingRetirement(os.Args[2:])
		}
		if os.Args[1] == "retire-tracking" {
			return retireTracking(os.Args[2:])
		}
		if os.Args[1] == "bootstrap-database" {
			return bootstrapDatabase(os.Args[2:])
		}
		return errors.New("unknown command")
	}
	config, err := controlapp.Load(os.Getenv)
	if err != nil {
		return err
	}
	var tlsCerts *controlapp.CertReloader
	if config.TLSListen != "" {
		if tlsCerts, err = controlapp.NewCertReloader(config.TLSCertFile, config.TLSKeyFile); err != nil {
			return err
		}
	}
	var console http.Handler
	if config.UIDir != "" {
		ui, err := controlapp.NewConsole(config.UIDir, controlapp.ConsolePolicy{ConnectOrigins: append([]string{controlapp.OriginOf(config.PublicAuth.Issuer)}, config.ConsoleConnectOrigins...)})
		if err != nil {
			return err
		}
		defer ui.Close()
		console = ui
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// CA rotation relies on pgx re-reading sslrootcert for every new connection (stdlib parses the DSN per connect); a once-parsed config or pgxpool needs a BeforeConnect reload.
	db, err := sql.Open("pgx", config.DatabaseURL)
	if err != nil {
		return errors.New("database configuration failed")
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = db.PingContext(checkCtx)
	cancel()
	if err != nil {
		return errors.New("database connection failed")
	}
	checkCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	err = postgres.CheckSchema(checkCtx, db)
	cancel()
	if err != nil {
		return errors.New("database schema check failed")
	}
	checkCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	err = postgres.CheckRuntimeGatePrivileges(checkCtx, db)
	cancel()
	if err != nil {
		return errors.New("runtime database role has invalid recovery privileges")
	}
	auth, err := operatorauth.New(ctx, config.Auth)
	if err != nil {
		return errors.New("operator identity initialization failed")
	}
	repository := &postgres.Repository{DB: db, Generation: config.Generation, WebhookDestinations: config.WebhookDestinations}
	// The signing resolver is built first: the operator API asks it whether a
	// domain would be signed, and message preparation uses it below.
	catalogPath := os.Getenv("THINPOST_CONTROL_SIGNING_CATALOG")
	var keys *signing.Resolver
	if catalogPath != "" {
		entries, err := signing.LoadCatalog(catalogPath)
		if err != nil {
			return err
		}
		keys, err = signing.New(repository, net.DefaultResolver, entries)
		if err != nil {
			return err
		}
	}
	operator := &admin.Handler{Authenticator: auth, Store: repository, WebhookHistory: repository, PolicyHolds: repository, TrackingHolds: repository, AppStatus: repository, Suppressions: repository, Operations: repository, AuthConfig: &config.PublicAuth, DNS: net.DefaultResolver, MX: net.DefaultResolver, ReverseDNS: net.DefaultResolver, WebhookDestinations: config.WebhookDestinations}
	if keys != nil {
		operator.SigningReadiness = keys
	}
	stopCleanup := startIdempotencyCleanup(ctx, repository)
	defer stopCleanup()
	stopHoldExpiry := startHoldExpiry(ctx, repository)
	defer stopHoldExpiry()
	var objects *objectstore.EncryptedStore
	var objectKeyring *objectstore.Keyring
	var webhookKeys webhook.Keys
	if config.S3 != nil {
		client, clientErr := minio.New(config.S3.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(config.S3.AccessKey, config.S3.SecretKey, ""), Secure: config.S3.Secure, Region: config.S3.Region})
		if clientErr != nil {
			return errors.New("object storage configuration failed")
		}
		keyring, keyErr := objectstore.LoadKeyring(config.S3.KeyringFile)
		if keyErr != nil {
			return errors.New("object encryption keyring initialization failed")
		}
		objects, err = objectstore.NewEncrypted(&objectstore.Store{Client: client, Bucket: config.S3.Bucket}, keyring)
		if err != nil {
			return errors.New("object encryption initialization failed")
		}
		webhookKeys, objectKeyring = keyring, keyring
		operator.Webhooks = &webhook.Service{Store: repository, Keys: keyring, Sender: &webhook.Sender{Destinations: config.WebhookDestinations}}
		repository.Objects = objects
		operator.ContentReader = &messagebody.Service{Repository: repository, Objects: objects}
		operator.InboundContent = messagebody.NewInboundService(repository, consoleInboundObjects{objects})
	}
	stopWebhooks := startWebhooks(ctx, repository, webhookKeys, objects, config.WebhookDestinations)
	defer stopWebhooks()
	bridgeConfig, err := controlbridge.FromEnvironment(os.Getenv)
	if err != nil {
		return err
	}
	var recipients *recipientpolicy.Policy
	if bridgeConfig != nil {
		if objects == nil {
			return errors.New("control bridge requires object storage")
		}
		recipients, err = recipientpolicy.Load(os.Getenv)
		if err != nil {
			return errors.New("control bridge requires an explicit recipient policy")
		}
		bridgeConfig.Recipients = recipients
	}
	// Acceptance and SMTP intake are built before the bridge, which hands the
	// intake to its control session (the submission/1 capability). An edge that
	// does not enable submission never negotiates it, so this is inert until the
	// edge's SMTP listeners are switched on.
	fingerprintKeys, err := controlapp.SubmissionFingerprintKeys(os.Getenv)
	if err != nil {
		return err
	}
	var acceptanceService *acceptance.Service
	if fingerprintKeys != nil {
		if objects == nil || bridgeConfig == nil || catalogPath == "" {
			return errors.New("submission requires storage, signing and edge bridge")
		}
		acceptanceService, err = acceptance.NewWithFingerprintKeys(repository, objects, fingerprintKeys, recipients)
		if err != nil {
			return errors.New("invalid submission configuration")
		}
		// Same source as the HTTP tracking option and trackingPreparation.
		acceptanceService.ApplyTrackingDefaults = config.Tracking != nil
	}
	if bridgeConfig != nil {
		if acceptanceService != nil && keys != nil && objectKeyring != nil {
			bridgeConfig.Submission = &smtpintake.Service{Repository: repository, Acceptance: acceptanceService, Signing: keys, Replay: &smtpintake.ReplaySecret{Store: repository, Keys: objectKeyring}}
		}
		bridge, err := controlbridge.New(*bridgeConfig, repository, objects)
		if err != nil {
			return err
		}
		stopBridge := startBridge(ctx, bridge, 12*time.Second)
		defer stopBridge()
	}
	var trackingPreparation *preparation.TrackingConfig
	var trackingHandler http.Handler
	if config.Tracking != nil {
		if objects == nil || catalogPath == "" || bridgeConfig == nil {
			return errors.New("tracking requires encrypted storage, signing and edge bridge")
		}
		protected, trackingErr := tracking.NewProtectedStore(objects)
		if trackingErr != nil {
			return trackingErr
		}
		trackingPreparation = &preparation.TrackingConfig{Origin: config.Tracking.Origin, Links: config.Tracking.Links, EventLimit: config.Tracking.EventLimit, Objects: protected}
		trackingHandler = &tracking.Handler{Origin: config.Tracking.Origin, Links: config.Tracking.Links, Visitor: &tracking.Service{Repository: repository, Objects: protected}, Unsubscribes: repository}
	}
	if catalogPath != "" {
		if objects == nil {
			return errors.New("message preparation requires object storage")
		}
		preparer := &preparation.Service{Repository: repository, Objects: objects, Keys: keys, Tracking: trackingPreparation}
		prepareCtx, prepareCancel := context.WithCancel(ctx)
		prepareDone := make(chan struct{})
		go func() {
			defer close(prepareDone)
			for prepareCtx.Err() == nil {
				err := preparer.PrepareNext(prepareCtx)
				if err == nil {
					continue
				}
				if prepareCtx.Err() != nil {
					return
				}
				if !errors.Is(err, preparation.ErrNoWork) {
					log.Print("message preparation unavailable; retrying")
				}
				timer := time.NewTimer(3 * time.Second)
				select {
				case <-timer.C:
				case <-prepareCtx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return
				}
			}
		}()
		defer func() { prepareCancel(); <-prepareDone }()
	}
	var handler, appReads http.Handler
	if acceptanceService != nil {
		service := acceptanceService
		server := submissionHandler(service, config, repository)
		handler = server
		// Operator test sends use the same acceptance service as the app API.
		operator.TestMessages = server
		// App-key reads share the submission service's key authentication and the
		// operator API's store and content reader (nil without object storage).
		appReads = &admin.AppReadHandler{Service: service, Store: repository, ContentReader: operator.ContentReader, Holds: repository}
	}
	// SMTPSubmission is true when this replica has the SMTP intake wired (the
	// edge bridge, signing and acceptance are all configured), so the console
	// shows the SMTP tab. Whether an edge currently listens is not part of this
	// startup flag: the tab reports each port's observed availability from the
	// edge's own report (Ready.submission_ports), and an edge that does not
	// enable submission never negotiates submission/1.
	smtpIntake := bridgeConfig != nil && bridgeConfig.Submission != nil
	runtimeInfo := &admin.RuntimeInfo{Features: admin.RuntimeFeatures{Submission: handler != nil, Tracking: config.Tracking != nil, SMTPSubmission: smtpIntake}}
	if config.Tracking != nil {
		runtimeInfo.Tracking = &admin.RuntimeTracking{Origin: config.Tracking.Origin, LinkEventLimit: config.Tracking.EventLimit}
	}
	runtimeInfo.Features.RecipientPolicy = recipients.Mode()
	operator.Runtime = runtimeInfo
	operatorHealth := controlapp.StartOperatorHealth(ctx, db.PingContext)
	defer operatorHealth.Close()
	mailRuntime := controlapp.MailRuntime{Tracking: trackingHandler, App: appReads, AdminHost: config.AdminHost}
	if handler != nil {
		admissionHealth := controlapp.StartOperatorHealth(ctx, repository.CheckAdmission)
		defer admissionHealth.Close()
		mailRuntime.Ready = admissionHealth.Ready
	}
	routes := controlapp.RouterWithRuntime(operator, handler, console, operatorHealth.Ready, mailRuntime)
	server := &http.Server{Addr: config.Listen, Handler: routes, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	servers := []*http.Server{server}
	result := make(chan error, 2)
	go func() { result <- server.ListenAndServe() }()
	if tlsCerts != nil {
		tlsServer := controlapp.NewTLSServer(config.TLSListen, routes, tlsCerts)
		servers = append(servers, tlsServer)
		go func() { result <- tlsServer.ListenAndServeTLS("", "") }()
	}
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("operator HTTP server failed")
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Drain both listeners concurrently so one draining does not leave the
		// other accepting new connections or starve it of the shared deadline.
		var wg sync.WaitGroup
		var failed atomic.Bool
		for _, s := range servers {
			wg.Add(1)
			go func(s *http.Server) {
				defer wg.Done()
				if err := s.Shutdown(shutdownCtx); err != nil {
					failed.Store(true)
				}
			}(s)
		}
		wg.Wait()
		if failed.Load() {
			return errors.New("operator HTTP shutdown failed")
		}
		return nil
	}
}

func submissionHandler(service submission.Service, config controlapp.Config, suppressions submission.SuppressionStore) *httpapi.Server {
	return httpapi.NewWithOptions(service, httpapi.Options{MaxAttachmentBytes: config.MaxAttachmentBytes, Tracking: config.Tracking != nil, Suppressions: suppressions})
}

func migrate() error {
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
	if err = postgres.Migrate(ctx, db); err != nil {
		return errors.New("database migration failed")
	}
	return nil
}
