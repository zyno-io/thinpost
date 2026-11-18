package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/deployhook"
)

// terminationLog is where Kubernetes reads a container's last words. Only a
// fixed reason code is ever written to it.
var terminationLog = "/dev/termination-log"

func writeTermination(err error) {
	var failure *deployhook.Failure
	reason := "internal_error"
	if errors.As(err, &failure) {
		reason = failure.Reason
		if failure.Detail != "" {
			reason += " (" + failure.Detail + ")"
		}
	}
	_ = os.WriteFile(terminationLog, []byte(reason), 0o644)
}

// deployDatabase opens the schema owner's connection from the validated
// environment. The application name makes the sessions identifiable.
func deployDatabase(name string) (*sql.DB, error) {
	dsn, err := controlapp.DatabaseURL(os.Getenv)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil, errors.New("database configuration failed")
	}
	query := parsed.Query()
	query.Set("application_name", name)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		return nil, errors.New("database configuration failed")
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

func deployContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, func() { cancel(); stop() }
}

func deployLog(format string, args ...any) { log.Printf(format, args...) }

func deployMigrate(args []string) (err error) {
	defer func() {
		if err != nil {
			writeTermination(err)
		}
	}()
	flags := flag.NewFlagSet("deploy-migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	actor := flags.String("actor", "", "deploy actor, deploy:<namespace>/<release>:r<revision>")
	drain := flags.Duration("drain-timeout", 2*time.Minute, "wait for queued messages before closing the gate")
	timeout := flags.Duration("timeout", 8*time.Minute, "overall timeout")
	check := flags.Bool("check", false, "report the plan without changing anything")
	if parseErr := flags.Parse(args); parseErr != nil || len(flags.Args()) != 0 || *actor == "" || *drain < 0 || *timeout <= 0 {
		return errors.New("deploy-migrate requires --actor")
	}
	role, generation := os.Getenv("THINPOST_RUNTIME_ROLE"), os.Getenv("THINPOST_DEPLOYMENT_GENERATION")
	if role == "" || generation == "" {
		return errors.New("deploy-migrate requires the runtime role and deployment generation")
	}
	db, err := deployDatabase("thinpost-deploy-migrate")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := deployContext(*timeout)
	defer cancel()
	result, err := deployhook.RunPre(ctx, deployhook.Postgres{DB: db}, deployhook.PreConfig{Actor: *actor, Generation: generation, RuntimeRole: role, DrainTimeout: *drain, Check: *check, Log: deployLog})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func deployReopen(args []string) (err error) {
	defer func() {
		if err != nil {
			writeTermination(err)
		}
	}()
	flags := flag.NewFlagSet("deploy-reopen", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	actor := flags.String("actor", "", "deploy actor, deploy:<namespace>/<release>:r<revision>")
	namespace := flags.String("namespace", "", "namespace of the control pods")
	selector := flags.String("selector", "", "label selector of the control pods")
	container := flags.String("container", "control", "control container name")
	image := flags.String("image", "", "exact control image the Deployment renders")
	replicas := flags.Int("replicas", 0, "expected number of Ready control pods")
	rollout := flags.Duration("rollout-timeout", 5*time.Minute, "wait for the rollout")
	kubeDir := flags.String("kube-dir", "/run/thinpost/kube", "service account token and CA directory")
	timeout := flags.Duration("timeout", 8*time.Minute, "overall timeout")
	if parseErr := flags.Parse(args); parseErr != nil || len(flags.Args()) != 0 || *actor == "" || *namespace == "" || *selector == "" || *container == "" || *image == "" || *replicas < 1 || *rollout <= 0 || *timeout <= 0 {
		return errors.New("deploy-reopen requires --actor, --namespace, --selector, --image and --replicas")
	}
	role, generation := os.Getenv("THINPOST_RUNTIME_ROLE"), os.Getenv("THINPOST_DEPLOYMENT_GENERATION")
	if role == "" || generation == "" {
		return errors.New("deploy-reopen requires the runtime role and deployment generation")
	}
	kube, err := deployhook.NewInCluster(*kubeDir, os.Getenv)
	if err != nil {
		return err
	}
	db, err := deployDatabase("thinpost-deploy-reopen")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := deployContext(*timeout)
	defer cancel()
	result, err := deployhook.RunPost(ctx, deployhook.Postgres{DB: db}, kube, deployhook.PostConfig{Actor: *actor, Generation: generation, RuntimeRole: role, Namespace: *namespace, Selector: *selector, Container: *container, Image: *image, Replicas: *replicas, RolloutTimeout: *rollout, Log: deployLog})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
