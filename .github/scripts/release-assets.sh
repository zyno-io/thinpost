#!/usr/bin/env bash
# Create a GitHub release with the given assets, or reconcile an existing one.
# Re-runs never overwrite: an asset that already exists must be byte-identical,
# and a release that exists without some assets gets only the missing ones.
#
#   release-assets.sh FILE...
#
# Environment: GH_TOKEN, REPO, TAG, TITLE, NOTES (empty = generated notes),
# PRERELEASE (true|false), VERIFY_TAG (true|false).
set -euo pipefail

: "${GH_TOKEN:?}" "${REPO:?}" "${TAG:?}" "${TITLE:?}"
[[ $# -gt 0 ]] || { echo "no assets given" >&2; exit 1; }

if ! gh release view "$TAG" --repo "$REPO" > /dev/null 2>&1; then
  flags=()
  [[ "${PRERELEASE:-false}" == "true" ]] && flags+=(--prerelease)
  [[ "${VERIFY_TAG:-false}" == "true" ]] && flags+=(--verify-tag)
  if [[ -n "${NOTES:-}" ]]; then flags+=(--notes "$NOTES"); else flags+=(--generate-notes); fi
  gh release create "$TAG" "$@" --repo "$REPO" --title "$TITLE" "${flags[@]}"
  echo "Created $REPO release $TAG"
  exit 0
fi

existing="$(mktemp -d)"
trap 'rm -rf "$existing"' EXIT
mapfile -t have < <(gh release view "$TAG" --repo "$REPO" --json assets --jq '.assets[].name')
missing=()
for file in "$@"; do
  name="$(basename "$file")"
  if printf '%s\n' "${have[@]}" | grep -Fxq -- "$name"; then
    gh release download "$TAG" --repo "$REPO" --pattern "$name" --dir "$existing"
    if ! cmp -s "$file" "$existing/$name"; then
      echo "$REPO release $TAG already has a different $name; releases are immutable." >&2
      exit 1
    fi
  else
    missing+=("$file")
  fi
done
if [[ ${#missing[@]} -gt 0 ]]; then
  gh release upload "$TAG" "${missing[@]}" --repo "$REPO"
  echo "Uploaded ${#missing[@]} missing asset(s) to $REPO release $TAG"
else
  echo "$REPO release $TAG already contains identical assets."
fi
