#!/usr/bin/env python3
"""Exercise an Edge image with synthetic identities on an internal Docker network.

This never sends mail to the Internet or reads deployment credentials. It
does not test a Kubernetes deployment, real DNS, a database, or IP fencing.
"""
import argparse
import os
from pathlib import Path
import platform
import re
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
# The source and network are deliberately not RFC 5737/3849 documentation addresses:
# the Edge rejects documentation ranges as sources, so this
# --internal Docker network uses an ordinary public-looking /30 that is never routed.
SOURCE = "11.42.51.2"
NETWORK = "11.42.51.0/30"


def run(args, *, env=None, timeout=120, check=True):
    result = subprocess.run(args, cwd=ROOT, env=env, text=True, capture_output=True, timeout=timeout)
    if check and result.returncode:
        # All data in this fixture is synthetic, but do not dump commands or
        # certificate files when an external tool fails.
        raise RuntimeError(f"{args[0]} failed ({result.returncode}): {result.stderr[-1200:]}")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="already-pulled Thinpost Edge image")
    parser.add_argument("--protocol", choices=("6", "7"), default="6", help="control protocol supported by the image (default: 6)")
    args = parser.parse_args()
    run(["docker", "image", "inspect", args.image])
    suffix = uuid.uuid4().hex[:12]
    name = "thinpost-edge-image-" + suffix
    network_created = False
    worker_created = False
    client_created = False
    volume_created = False
    holder_created = False
    holder = name + "-holder"
    # Docker Desktop/Colima commonly share the user's home but not /tmp.
    # The directory contains synthetic keys only and is removed on exit.
    with tempfile.TemporaryDirectory(prefix=".thinpost-image-", dir=Path.home()) as temp:
        root = Path(temp)
        os.chmod(root, 0o755)
        env = os.environ.copy()
        env["THINPOST_TEST_EDGE_IMAGE_GENERATE"] = str(root)
        run(["go", "test", "./internal/controlsession", "-run", "^TestEdgeImageGenerateFixtureCertificates$", "-count=1"], env=env)
        # These are short-lived synthetic keys. The unprivileged fixture
        # client updates the pairs while the worker sees a read-only mount.
        for fixture_file in ("edge.crt", "edge.key", "smtp.crt", "smtp.key"):
            os.chmod(root / fixture_file, 0o666)
        arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(platform.machine().lower())
        if arch is None:
            raise RuntimeError("unsupported local architecture")
        build_env = os.environ.copy()
        build_env.update({"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"})
        run(["go", "test", "-c", "-ldflags=-s -w", "-o", str(root / "client.test"), "./internal/controlsession"], env=build_env)
        os.chmod(root / "client.test", 0o755)
        try:
            # Keep the synthetic tmpfs volume mounted while the initialization
            # and worker containers run. Every process remains non-root.
            volume_created = True
            run(["docker", "volume", "create", "--driver", "local", "--opt", "type=tmpfs", "--opt", "device=tmpfs", "--opt", "o=uid=65532,gid=65532,mode=0700,size=32m", name])
            mount = f"type=volume,src={name},dst=/journal"
            holder_created = True
            run(["docker", "run", "-d", "--name", holder, "--network", "none", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only", "--mount", mount, "python:3.13-alpine", "python3", "-c", "import time; time.sleep(300)"])
            base = ["docker", "run", "--rm", "--network", "none", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only", "--mount", mount]
            identity = run([*base, args.image, "journal-id", "/journal/attempts.db"]).stdout.strip()
            if not re.fullmatch("[0-9a-f]{32}", identity):
                raise RuntimeError("image did not produce a valid synthetic journal identity")
            network_created = True
            run(["docker", "network", "create", "--internal", "--subnet", NETWORK, name])
            if run(["docker", "network", "inspect", "-f", "{{.Internal}}", name]).stdout.strip() != "true":
                raise RuntimeError("fixture network is not internal")
            settings = {
                "THINPOST_EDGE_WORKER_ID": "fixture-worker",
                "THINPOST_EDGE_SMTP_HOSTNAME": "smtp.fixture.invalid",
                "THINPOST_EDGE_BOUNCE_DOMAIN": "bounce.fixture.invalid",
                "THINPOST_EDGE_CONTROL_LISTEN": "127.0.0.1:7443",
                "THINPOST_EDGE_SMTP_LISTEN": SOURCE + ":25",
                "THINPOST_EDGE_CONTROLLER_IDENTITIES": "spiffe://thinpost.fixture/control",
                "THINPOST_EDGE_CONTROL_CA": "/fixture/ca.crt",
                "THINPOST_EDGE_CONTROL_CERT": "/fixture/edge.crt",
                "THINPOST_EDGE_CONTROL_KEY": "/fixture/edge.key",
                "THINPOST_EDGE_SMTP_CERT": "/fixture/smtp.crt",
                "THINPOST_EDGE_SMTP_KEY": "/fixture/smtp.key",
                "THINPOST_EDGE_JOURNAL_PATH": "/journal/attempts.db",
                "THINPOST_EDGE_JOURNAL_CAPACITY": "20",
                "THINPOST_EDGE_JOURNAL_RESERVE_BYTES": "1048576",
                "THINPOST_EDGE_CLOCK_MARGIN": "1s",
            }
            flags = []
            for key, value in settings.items():
                flags.extend(["--env", key + "=" + value])
            worker_args = ["docker", "run", "-d", "--name", name, "--network", name, "--ip", SOURCE,
                "--sysctl", "net.ipv4.ip_unprivileged_port_start=0", "--user", "65532:65532", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges", "--read-only", "--mount", mount,
                "--mount", f"type=bind,src={root},dst=/fixture,readonly",
                "--mount", f"type=bind,src={root / 'ca.crt'},dst=/etc/ssl/certs/ca-certificates.crt,readonly"]
            worker_created = True
            run([*worker_args, *flags, args.image])
            for _ in range(60):
                health = run(["docker", "exec", name, "/usr/local/bin/thinpost-edge", "healthcheck"], check=False, timeout=5)
                if health.returncode == 0:
                    break
                state = run(["docker", "inspect", "-f", "{{.State.Status}}", name]).stdout.strip()
                if state != "running":
                    logs = run(["docker", "logs", name], check=False).stderr.strip()
                    raise RuntimeError("edge fixture stopped: " + logs[-1000:])
                time.sleep(0.5)
            else:
                raise RuntimeError("edge fixture did not become healthy")
            client_args = ["docker", "run", "--rm", "--name", name + "-client", "--network", "container:" + name, "--user", "65532:65532",
                "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
                "--mount", f"type=bind,src={root},dst=/fixture",
                "--env", "THINPOST_TEST_EDGE_IMAGE_FIXTURE=/fixture", "--env", "THINPOST_TEST_EDGE_IMAGE_JOURNAL=" + identity,
                "--env", "THINPOST_TEST_EDGE_IMAGE_PROTOCOL=" + args.protocol,
                "--entrypoint", "/fixture/client.test", "python:3.13-alpine", "-test.v", "-test.timeout=75s", "-test.run", "^TestEdgeImageControlAndBounce$"]
            client_created = True
            result = run(client_args, timeout=90, check=False)
            if result.returncode:
                raise RuntimeError("synthetic client fixture failed: " + result.stdout[-2000:] + result.stderr[-1000:])
            if "--- PASS: TestEdgeImageControlAndBounce" not in result.stdout:
                raise RuntimeError("fixture did not report a passing integration test")
            print("isolated Edge image control and bounce fixture passed")
        finally:
            failures = []
            if client_created and run(["docker", "container", "inspect", name + "-client"], check=False).returncode == 0:
                run(["docker", "stop", "--time", "3", name + "-client"], check=False, timeout=10)
                if run(["docker", "container", "inspect", name + "-client"], check=False).returncode == 0:
                    if run(["docker", "rm", name + "-client"], check=False, timeout=15).returncode:
                        failures.append("client " + name)
            if worker_created and run(["docker", "container", "inspect", name], check=False).returncode == 0:
                run(["docker", "stop", "--time", "3", name], check=False, timeout=10)
                if run(["docker", "rm", name], check=False, timeout=15).returncode:
                    failures.append("Edge container " + name)
            if network_created and run(["docker", "network", "inspect", name], check=False).returncode == 0 and run(["docker", "network", "rm", name], check=False, timeout=15).returncode:
                failures.append("network " + name)
            if holder_created and run(["docker", "container", "inspect", holder], check=False).returncode == 0:
                run(["docker", "stop", "--time", "3", holder], check=False, timeout=10)
                if run(["docker", "rm", holder], check=False, timeout=15).returncode:
                    failures.append("volume holder " + holder)
            if volume_created and run(["docker", "volume", "inspect", name], check=False).returncode == 0 and run(["docker", "volume", "rm", name], check=False, timeout=15).returncode:
                failures.append("volume " + name)
            if failures:
                raise RuntimeError("could not clean up synthetic " + ", ".join(failures))


if __name__ == "__main__":
    main()
