#!/usr/bin/env python3
"""Run the pipeline against disposable PostgreSQL and a local S3 emulator."""

import argparse
import os
import subprocess
import time
import urllib.error
import urllib.request
import uuid


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def port(name, container_port):
    result = run(
        "docker", "port", name, f"{container_port}/tcp", capture_output=True, text=True
    )
    lines = result.stdout.strip().splitlines()
    if len(lines) != 1 or not lines[0].startswith("127.0.0.1:"):
        raise RuntimeError("unexpected test service port binding")
    return lines[0].rsplit(":", 1)[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--postgres-version", choices=("17", "18"), default="17")
    args = parser.parse_args()
    suffix = uuid.uuid4().hex[:12]
    pg = "thinpost-test-postgres-" + suffix
    s3 = "thinpost-test-s3-" + suffix
    try:
        run(
            "docker", "run", "-d", "--name", pg, "-e", "POSTGRES_PASSWORD=fixture-only", "-e", "POSTGRES_DB=thinpost_fixture",
            "-p", "127.0.0.1::5432", f"postgres:{args.postgres_version}-alpine", stdout=subprocess.DEVNULL,
        )
        run(
            "docker", "run", "-d", "--name", s3, "-e", "SERVICES=s3",
            "-p", "127.0.0.1::4566", "ministackorg/ministack:1.5.7", stdout=subprocess.DEVNULL,
        )
        pg_port, s3_port = port(pg, 5432), port(s3, 4566)
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            postgres_ready = subprocess.run(
                ["docker", "exec", pg, "pg_isready", "-U", "postgres"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            ).returncode == 0
            try:
                with urllib.request.urlopen(
                    f"http://127.0.0.1:{s3_port}/_ministack/health", timeout=1
                ) as response:
                    s3_ready = response.status == 200
            except (OSError, urllib.error.URLError):
                s3_ready = False
            if postgres_ready and s3_ready:
                break
            time.sleep(1)
        else:
            raise RuntimeError("disposable test services did not become ready")
        env = os.environ.copy()
        env["THINPOST_TEST_DATABASE_URL"] = (
            f"postgres://postgres:fixture-only@127.0.0.1:{pg_port}/thinpost_fixture?sslmode=disable"
        )
        env["THINPOST_TEST_DISPOSABLE_DB"] = "1"
        env["THINPOST_TEST_S3_ENDPOINT"] = f"127.0.0.1:{s3_port}"
        root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        run(
            "go", "test", "-race", "./internal/controlsession", "-run",
            "^(TestDevelopmentMailPipelineWithPostgres|TestCommittedPermitLostBeforeEdgeTimeout)$",
            "-count=1", cwd=root, env=env,
        )
        run(
            "go", "test", "-race", "./internal/controlsession", "-run",
            "^(TestSubmissionPipelineWithPostgres|TestSubmissionRecipientAllowlistIsRefusedAtData|TestSubmissionPauseAfterLostAcknowledgement|TestSubmissionDisconnectCases|TestSubmissionTakeoverDuringBlockedCommit|TestNewEdgeWithOldControlAnswers454)$",
            "-count=1", cwd=root, env=env,
        )
        run(
            "go", "test", "-race", "./internal/postgres", "-run",
            "^(TestRecoveryObjectInventory|TestRecoveryGateAndQuarantine|TestRecoveryHoldHistoryMigration|TestRecoveryMigrateRestoredSchemas|TestMigrationsSerializeAndDetectDrift|TestJournalIdentityMigrationPreservesLegacyAttempts|TestMessageMetadataSchemaConstraints|TestMigrationsInstallNoExecutableRoutines)$", "-count=1", cwd=root, env=env,
        )
        run(
            "go", "test", "-race", "./internal/postgres", "-run",
            "^(TestPostgresAcceptanceIntegration|TestTrackingPostgresAtomicReferencesAndBoundedEvents|TestTrackedDeliveryHoldsAndRechecksBeforePermit|TestResendBatchPostgresAtomicityAndIdempotency|TestMessageSearchAndMasking|TestWebhookEndpointsPostgres|TestWebhookEndpointsMigration|TestWebhookFormatsPostgres|TestWebhookFormatsMigration|TestWebhookDeleteDuringClaimPostgres)$", "-count=1", cwd=root, env=env,
        )
        run(
            "go", "test", "-race", "./cmd/thinpost-control", "-run",
            "^(TestRecoveryObjectsCommand|TestRetryPolicyCommand)$", "-count=1", cwd=root, env=env,
        )
    finally:
        for name in (s3, pg):
            subprocess.run(
                ["docker", "rm", "-f", name], stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL, check=False,
            )


if __name__ == "__main__":
    main()
