#!/bin/sh
# Generates every quickstart secret, certificate and key into the pki volume,
# the DNS zone into the zone volume, and prepares the Edge journal. Idempotent:
# a completed volume is left untouched. Nothing generated here is committed.
set -eu

PKI=/pki
ZONE=/zone
DOMAIN=quickstart.example
APP_ID=5ab1e0a9f1c64d2e8b3c7d0e1f2a3b4c
SELECTOR=mail1

if [ -f "$PKI/.complete" ]; then
  echo "pki-init: already complete"
  exit 0
fi
cd "$PKI"
umask 022

hex() { openssl rand -hex "$1"; }
b64key() { openssl rand 32 | openssl base64 -A; }

# Passwords and tokens (hex, so they are safe inside URLs).
for name in pg-admin-password pg-owner-password pg-runtime-password s3-secret automation-secret; do
  hex 24 >"$name"
done

# Private CA and leaf certificates (ECDSA P-256).
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out ca.key 2>/dev/null
openssl req -new -x509 -key ca.key -sha256 -days 3650 -subj "/CN=Thinpost quickstart dev CA" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -out ca.crt

issue() { # name common-name subjectAltName extendedKeyUsage
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1.key" 2>/dev/null
  openssl req -new -key "$1.key" -subj "/CN=$2" -out "$1.csr"
  printf 'subjectAltName=%s\nextendedKeyUsage=%s\nkeyUsage=digitalSignature\nbasicConstraints=CA:FALSE\n' "$3" "$4" >"$1.ext"
  openssl x509 -req -in "$1.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 365 -sha256 -extfile "$1.ext" -out "$1.crt" 2>/dev/null
  rm -f "$1.csr" "$1.ext"
}
issue edge-control edge.$DOMAIN "DNS:edge.$DOMAIN,URI:spiffe://$DOMAIN/edge" serverAuth
issue control-client control "URI:spiffe://$DOMAIN/control" clientAuth
issue smtp smtp.$DOMAIN "DNS:smtp.$DOMAIN" serverAuth
issue mx mx.$DOMAIN "DNS:mx.$DOMAIN" serverAuth
rm -f ca.srl

# Keyrings: object encryption, submission fingerprints, bounce addresses.
printf '{"active":"k1","keys":{"k1":"%s"}}\n' "$(b64key)" >object-keyring.json
printf '["%s"]\n' "$(b64key)" >fingerprint-keys.json
printf '{"v1":"%s"}\n' "$(b64key)" >bounce-keys.json

# DKIM signing key for the sender domain, and the signing catalog entry. The
# app itself is created by db-init with this fixed ID, because the catalog is
# read at control start-up.
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out dkim.pem 2>/dev/null
printf '[{"app_id":"%s","domain":"%s","selector":"%s","key_path":"/pki/dkim.pem"}]\n' "$APP_ID" "$DOMAIN" "$SELECTOR" >signing-catalog.json
echo "$APP_ID" >app-id

# Development issuer signing key.
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out idp-signing.pem 2>/dev/null

# Static busybox so the shell-less release control image can run its wrapper.
cp /bin/busybox.static busybox
chmod 755 busybox

# Edge journal identity: the control plane must be given it up front.
mkdir -p /journal
chown 65532:65532 /journal
chmod 700 /journal
journal_id="$(su-exec 65532:65532 /usr/local/bin/thinpost-edge journal-id /journal/attempts.db)"
printf 'THINPOST_CONTROL_JOURNAL_ID=%s\n' "$journal_id" >runtime.env

# DNS zone: MX, SPF, DKIM and DMARC for the sender domain.
dkim_p="$(openssl pkey -in dkim.pem -pubout -outform DER 2>/dev/null | openssl base64 -A)"
dkim_txt="$(printf 'v=DKIM1; k=rsa; p=%s' "$dkim_p" | fold -w 200 | sed 's/.*/"&"/' | tr '\n' ' ')"
mkdir -p "$ZONE"
cat >"$ZONE/$DOMAIN.zone" <<ZONE
\$ORIGIN $DOMAIN.
\$TTL 5
@ IN SOA ns.$DOMAIN. hostmaster.$DOMAIN. $(date +%s) 5 5 60 5
@ IN NS ns
ns IN A 10.99.50.53
@ IN MX 10 mx
mx IN A 11.42.51.25
smtp IN A 11.42.51.10
bounce IN MX 10 smtp
@ IN TXT "v=spf1 ip4:11.42.51.10 -all"
$SELECTOR._domainkey IN TXT ( $dkim_txt )
_dmarc IN TXT "v=DMARC1; p=none"
ZONE

chmod 644 "$PKI"/* "$ZONE"/*
chmod 755 "$PKI/busybox"
touch "$PKI/.complete"
echo "pki-init: generated secrets, certificates, keys and zone"
