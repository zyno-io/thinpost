#!/bin/sh
# Sets one TXT record in the quickstart zone and bumps the SOA serial so
# CoreDNS reloads it. Usage: zone-add.sh <name> <value>
# Pass the value in single quotes: a DKIM value has spaces and semicolons.
# The name may be relative (thinpost1._domainkey) or the full name the console
# shows (thinpost1._domainkey.quickstart.example), with or without a trailing
# dot. A value longer than 255 characters (a DKIM record is about 410) is
# written as several 255-character strings, which resolvers join again, the
# same split a DNS provider needs. Pass the value unsplit; the script does the
# splitting. Any TXT record already at the name is replaced, so a repeated run
# never leaves two records at one name (which verification refuses).
set -eu
file=/zone/quickstart.example.zone
name="${1%.}"
name="${name%.quickstart.example}"
value="$2"
[ -n "$name" ] && [ "$name" != quickstart.example ] || { echo "usage: zone-add.sh <name> <value>" >&2; exit 2; }
# fold -w is bytes; the value is ASCII.
strings="$(printf '%s' "$value" | fold -w 255 | sed 's/.*/"&"/' | tr '\n' ' ')"
line="$name IN TXT ${strings% }"
serial="$(awk '$2=="IN" && $3=="SOA" {print $6}' "$file")"
next=$((serial + 1))
# Rewrite in place (the file is bind-mounted into CoreDNS read-only; this is a
# separate writer on the same volume).
tmp="$(mktemp)"
sed "s/^\(@ IN SOA [^ ]* [^ ]* \)$serial /\1$next /" "$file" | awk -v n="$name" '!($1==n && $2=="IN" && $3=="TXT")' >"$tmp"
printf '%s\n' "$line" >>"$tmp"
cat "$tmp" >"$file"
rm -f "$tmp"
echo "zone serial $next"
