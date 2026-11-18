---
title: Releasing
description: How Thinpost releases are cut, how images are promoted by digest, where the Helm chart is published and how to verify a release.
---

# Releasing

A release is a tag on a commit that is already on `main`. Nothing is rebuilt for a release: the container images that CI built and attested for that commit on `main` are promoted under the release tags, so the release points at the exact bytes that were tested.

## Cutting a release

1. Merge to `main` and wait for CI to finish on the commit you want to release. CI builds both images for `linux/amd64` and `linux/arm64`, pushes them tagged `sha-<commit>` and `main`, and records a provenance attestation.
2. Tag that commit `vX.Y.Z`, or `vX.Y.Z-rc.1` for a prerelease, and push the tag.

The tag must be a SemVer version with a leading `v`. The release job rejects anything else, and rejects a tag whose commit is not an ancestor of `main`.

## What the release job does

1. **Runs the same checks.** The Go, hygiene, protobuf drift, Helm and console checks run again on the tagged commit. The release does not proceed unless they pass.
2. **Verifies and promotes the images.** For `ghcr.io/zyno-io/thinpost-control` and `ghcr.io/zyno-io/thinpost-edge`, it resolves the digest of `sha-<commit>`, verifies its build-provenance attestation with `gh attestation verify` (signer workflow, repository and source ref `main`), and then adds tags to that same digest with `docker buildx imagetools create`:
   - `vX.Y.Z` for every release;
   - `X.Y` and `latest` as well for a stable release (no prerelease suffix).
   If the `sha-<commit>` image does not exist yet, the job fails. Wait for the `main` build and re-run.
3. **Packages the chart.** `helm package` with `--version X.Y.Z` and `--app-version vX.Y.Z`, plus a `SHA256SUMS` file.
4. **Publishes the chart.** It creates a GitHub release named `thinpost-X.Y.Z` on the `zyno-io/charts` repository with the chart archive attached. If that release already exists, the step is skipped, so a re-run does not overwrite a published chart.
5. **Creates the GitHub release.** The release on `zyno-io/thinpost` has generated notes, the chart archive and `SHA256SUMS`, and is marked a prerelease for a prerelease tag. On a re-run, the job compares the assets with the existing release and fails on any difference instead of replacing them.

Release builds do not touch any deployment. Deployments are separate from release tags: run your own installation from a release tag, or from any image digest you choose.

## Notifying a deployer

To track `main` continuously, set a `DEPLOY_HOOK_URL` repository secret in your fork or mirror: a URL that accepts an empty `POST`, such as a CI pipeline trigger. After both `main` images are published and attested, the **Notify deployer** job posts to it once. The request carries no release data, so the deployer must resolve and verify the commit itself, the way [Verify a release](#verify-a-release) does. Without the secret the job does nothing. A failed notification shows as a warning, not a failed build, so a deployer should also poll.

## Verify a release

Pull the images by digest, or verify the attestation of what a tag points to:

```sh
docker buildx imagetools inspect ghcr.io/zyno-io/thinpost-control:vX.Y.Z
gh attestation verify oci://ghcr.io/zyno-io/thinpost-control@sha256:<digest> --repo zyno-io/thinpost
```

The chart is a release asset. Check its checksum against `SHA256SUMS` on the GitHub release.

## Installing a release

Reference the chart and images by the release version. Pin images by digest in production, and keep the control and Edge images on versions that speak the same [control protocol](/reference/control-protocol). A mixed-version rolling upgrade across protocol versions is not supported. See [Upgrades](/deploy/upgrades).

## Versioning

Thinpost is pre-1.0. APIs, configuration and the database schema can change between minor versions. The schema only moves forward: an image older than the database schema refuses to start, and there is no automatic downgrade. Read the release notes before upgrading, and keep a tested PITR backup.
