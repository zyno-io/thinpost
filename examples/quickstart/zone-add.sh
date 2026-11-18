#!/bin/sh
# Appends one TXT record to the quickstart zone and bumps the SOA serial so
# CoreDNS reloads it. Usage: zone-add.sh <name> <value>
# The name may be relative (_thinpost) or the full name the console shows
# (_thinpost.quickstart.example), with or without a trailing dot.
set -eu
file=/zone/quickstart.example.zone
name="${1%.}"
name="${name%.quickstart.example}"
value="$2"
[ -n "$name" ] && [ "$name" != quickstart.example ] || { echo "usage: zone-add.sh <name> <value>" >&2; exit 2; }
serial="$(awk '$2=="IN" && $3=="SOA" {print $6}' "$file")"
next=$((serial + 1))
# Rewrite in place (the file is bind-mounted into CoreDNS read-only; this is a
# separate writer on the same volume).
tmp="$(mktemp)"
sed "s/^\(@ IN SOA [^ ]* [^ ]* \)$serial /\1$next /" "$file" >"$tmp"
printf '%s IN TXT "%s"\n' "$name" "$value" >>"$tmp"
cat "$tmp" >"$file"
rm -f "$tmp"
echo "zone serial $next"
