package deployhook

import (
	"context"
	"time"
)

// PostConfig configures the post-upgrade hook.
type PostConfig struct {
	Actor       string
	Generation  string
	RuntimeRole string
	Namespace   string
	Selector    string
	Container   string
	// Image is the exact image string the Deployment renders.
	Image    string
	Replicas int
	// RolloutTimeout bounds the wait for the rollout to finish.
	RolloutTimeout time.Duration
	Poll           time.Duration
	Log            func(format string, args ...any)
}

// PostResult is the JSON summary printed by the hook.
type PostResult struct {
	Action   string `json:"action"`
	Schema   int    `json:"schema"`
	Revision int64  `json:"revision"`
}

// RunPost reopens a gate this release closed, but only after the rollout has
// finished with every pod on the new image. A gate it does not own is left
// alone.
func RunPost(ctx context.Context, db Database, kube Kube, cfg PostConfig) (PostResult, error) {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if cfg.Actor == "" || cfg.Generation == "" || cfg.RuntimeRole == "" || cfg.Namespace == "" || cfg.Selector == "" || cfg.Container == "" || cfg.Image == "" || cfg.Replicas < 1 {
		return PostResult{}, &Failure{Reason: "invalid_configuration"}
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	target, err := db.Target()
	if err != nil {
		return PostResult{}, fail("migrations_not_reviewed", err)
	}
	current, err := db.SchemaVersion(ctx)
	if err != nil {
		return PostResult{}, fail("schema_check_failed", err)
	}
	if current != target {
		return PostResult{}, &Failure{Reason: "schema_not_current"}
	}
	status, err := db.Inspect(ctx)
	if err != nil {
		return PostResult{}, fail("gate_inspect_failed", err)
	}
	result := PostResult{Schema: current, Revision: status.Revision}
	if status.Mode != "closed" || status.ChangedBy != cfg.Actor {
		result.Action = "not_owned"
		logf("the gate is not closed by this deploy; nothing to reopen")
		return result, nil
	}
	if status.Generation == "" || status.Generation != cfg.Generation {
		return result, &Failure{Reason: "generation_mismatch"}
	}
	if reason := waitForRollout(ctx, kube, cfg); reason != "" {
		return result, &Failure{Reason: reason}
	}
	if err = db.CheckRole(ctx, cfg.RuntimeRole); err != nil {
		return result, fail("runtime_privileges_invalid", err)
	}
	if err = db.Reopen(ctx, cfg.Generation, cfg.Actor, cfg.RuntimeRole, status.Revision, target); err != nil {
		return result, fail("gate_reopen_failed", err)
	}
	result.Action, result.Revision = "reopened", status.Revision+1
	logf("rollout complete; the gate was reopened")
	return result, nil
}

// waitForRollout returns "" when exactly cfg.Replicas pods are Running and
// Ready on cfg.Image and no other live pod remains.
func waitForRollout(ctx context.Context, kube Kube, cfg PostConfig) string {
	deadline := time.Now().Add(cfg.RolloutTimeout)
	reason := "rollout_timeout"
	for {
		pods, err := kube.ListPods(ctx, cfg.Namespace, cfg.Selector)
		if err != nil {
			reason = "kubernetes_unavailable"
		} else if RolloutComplete(pods, cfg.Container, cfg.Image, cfg.Replicas) {
			return ""
		} else {
			reason = "rollout_timeout"
		}
		if !time.Now().Add(cfg.Poll).Before(deadline) {
			return reason
		}
		if sleepContext(ctx, cfg.Poll) != nil {
			return "interrupted"
		}
	}
}

// RolloutComplete is true when no pod is terminating or in the Unknown phase,
// every non-terminal pod runs exactly image in the named container and is
// Running and Ready, and their number equals replicas.
func RolloutComplete(pods []Pod, container, image string, replicas int) bool {
	ready := 0
	for _, pod := range pods {
		if pod.Deleting || pod.Phase == "Unknown" {
			return false
		}
		if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
			continue
		}
		if pod.Images[container] != image {
			return false
		}
		if pod.Phase != "Running" || !pod.Ready {
			return false
		}
		ready++
	}
	return ready == replicas
}
