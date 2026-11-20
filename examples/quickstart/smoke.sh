#!/usr/bin/env bash
# End-to-end proof for the Compose quickstart. Run after
#   docker compose up -d --build --wait
# from anywhere. Needs docker compose, curl, jq and openssl on the host.
set -euo pipefail

cd "$(dirname "$0")/../.."
BASE=${THINPOST_BASE_URL:-https://localhost:8443}
MAILPIT=${MAILPIT_URL:-http://localhost:8025}
DOMAIN=quickstart.example
RECIPIENT=demo@quickstart.example
nonce="$(date +%s)-$RANDOM"
compose() { docker compose "$@"; }
step() { printf '\n==> %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  compose ps -a >&2 || true
  compose logs --no-color --tail=25 caddy control edge devidp coredns mailpit >&2 || true
  exit 1
}
api() { # method path [json body]; prints the body; returns 1 (and explains on stderr) on a non-2xx status
  local method=$1 path=$2 body=${3:-} out status
  out="$(mktemp)"
  if [ -n "$body" ]; then
    status="$(curl -sk -o "$out" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$body" "$BASE$path")" || status=000
  else
    status="$(curl -sk -o "$out" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $TOKEN" "$BASE$path")" || status=000
  fi
  if [ "${status:0:1}" != 2 ]; then
    printf '%s %s -> HTTP %s: %s\n' "$method" "$path" "$status" "$(head -c 300 "$out")" >&2
    rm -f "$out"; return 1
  fi
  cat "$out"; rm -f "$out"
}
json() { # what text: abort unless the text is JSON (a proxy error page is not)
  jq -e . >/dev/null 2>&1 <<<"$2" || fail "$1 did not return JSON: $(printf %s "$2" | head -c 300)"
}
# The seeded app is reused and capped at 10 active SMTP credentials, so the one
# the SMTP leg creates is always revoked, even when the run fails.
CID=""
rawfile=""
cleanup() {
  [ -z "$rawfile" ] || rm -f "$rawfile"
  [ -z "$CID" ] || api DELETE "/api/v1/apps/$APP/smtp/credentials/$CID" >/dev/null 2>&1 || true
}
trap cleanup EXIT
retry() { # seconds description command...
  local deadline=$((SECONDS + $1)) what=$2; shift 2
  until "$@"; do [ "$SECONDS" -lt "$deadline" ] || fail "timed out waiting for $what"; sleep 1; done
}

step "Console is served over HTTPS"
retry 60 "the console" curl -skf -o /dev/null "$BASE/"
ctype="$(curl -sk -o /dev/null -w '%{content_type}' "$BASE/")"
case "$ctype" in text/html*) echo "console: HTTP 200 $ctype";; *) fail "console returned $ctype";; esac
cfg="$(curl -sk "$BASE/api/v1/config")"; json "/api/v1/config" "$cfg"
jq -e '.oidc.issuer == "https://localhost:8443/idp"' >/dev/null <<<"$cfg" || fail "public OIDC config is wrong: $cfg"
disc="$(curl -sk "$BASE/idp/.well-known/openid-configuration")"; json "issuer discovery" "$disc"
jq -e '.jwks_uri' >/dev/null <<<"$disc" || fail "issuer discovery has no jwks_uri"
echo "issuer discovery and public config OK"

step "Operator token from the development issuer"
secret="$(compose run --rm --no-deps -T --entrypoint cat pki-init /pki/automation-secret)"
tok="$(curl -sk -d grant_type=client_credentials -d client_id=thinpost-quickstart-automation --data-urlencode "client_secret=$secret" "$BASE/idp/token")"; json "token endpoint" "$tok"
TOKEN="$(jq -r '.access_token // empty' <<<"$tok")"
[ -n "$TOKEN" ] || fail "no access token: $tok"
api GET /api/v1/apps >/dev/null || fail "operator API refused the automation token"
echo "operator API accepted the token (scope + roles claims)"

step "Console dev login (authorization code + PKCE), as the browser does it"
verifier="$(openssl rand -hex 32)"
challenge="$(printf %s "$verifier" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
redirect="$BASE/auth/callback"
authz=(--data-urlencode "response_type=code" --data-urlencode "client_id=thinpost-console" --data-urlencode "redirect_uri=$redirect"
  --data-urlencode "scope=openid profile thinpost-admin" --data-urlencode "state=smoke" --data-urlencode "nonce=smoke" --data-urlencode "code_challenge=$challenge" --data-urlencode "code_challenge_method=S256")
location="$(curl -sk -o /dev/null -D - "${authz[@]}" "$BASE/idp/authorize" | tr -d '\r' | awk 'tolower($1)=="location:"{print $2}')"
case "$location" in "$redirect?"*code=*) ;; *) fail "authorize did not redirect with a code: $location";; esac
code="$(sed -n 's/.*[?&]code=\([^&]*\).*/\1/p' <<<"$location")"
exch="$(curl -sk -d grant_type=authorization_code --data-urlencode "code=$code" -d client_id=thinpost-console --data-urlencode "redirect_uri=$redirect" --data-urlencode "code_verifier=$verifier" "$BASE/idp/token")"; json "code exchange" "$exch"
console_token="$(jq -r '.access_token // empty' <<<"$exch")"
[ -n "$console_token" ] || fail "code exchange failed: $exch"
status="$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $console_token" "$BASE/api/v1/apps")"
[ "$status" = 200 ] || fail "console login token was refused by the operator API (HTTP $status)"
echo "dev login produced an access token the operator API accepts"

step "App, API key and sender domain"
apps="$(api GET /api/v1/apps)" || fail "listing apps failed"; json "apps list" "$apps"
APP="$(jq -r '.apps[] | select(.name=="Quickstart") | .id' <<<"$apps")"
[ -n "$APP" ] || fail "seeded app not found"
echo "app $APP"
keyresp="$(api POST "/api/v1/apps/$APP/keys" "{\"name\":\"smoke-$nonce\"}")" || fail "issuing an API key failed"; json "key issue" "$keyresp"
KEY="$(jq -r '.secret // empty' <<<"$keyresp")"
[ -n "$KEY" ] || fail "no API key in: $keyresp"
echo "issued an API key"
doms="$(api GET "/api/v1/apps/$APP/domains")" || fail "listing domains failed"; json "domain list" "$doms"
verified="$(jq -r --arg d "$DOMAIN" '[.domains[] | select(.name==$d and .active)] | length' <<<"$doms")"
if [ "$verified" = 0 ]; then
  chal="$(api POST "/api/v1/apps/$APP/domains" "{\"domain\":\"$DOMAIN\"}")" || fail "requesting the domain challenge failed"; json "domain challenge" "$chal"
  name="$(jq -r .dns_name <<<"$chal")" value="$(jq -r .dns_value <<<"$chal")"
  step "Publishing the ownership TXT ($name) in the local zone"
  compose run --rm --no-deps -T --entrypoint sh pki-init /qs/zone-add.sh "$name" "$value" || fail "could not update the zone"
  # CoreDNS reloads the zone within a second or two; until then verify fails.
  verify_domain() { api POST "/api/v1/apps/$APP/domains/$DOMAIN/verify" >/dev/null 2>&1; }
  retry 60 "domain verification" verify_domain
fi
echo "sender domain $DOMAIN verified"

step "Sending through the Postmark-compatible API"
resp="$(curl -sk -X POST "$BASE/email" -H "X-Postmark-Server-Token: $KEY" -H 'Content-Type: application/json' -H 'Accept: application/json' \
  -d "{\"From\":\"Quickstart <hello@$DOMAIN>\",\"To\":\"$RECIPIENT\",\"Subject\":\"Quickstart $nonce\",\"TextBody\":\"Hello from the Thinpost quickstart.\"}")"
json "submission" "$resp"
jq -e '.ErrorCode == 0' <<<"$resp" >/dev/null || fail "submission rejected: $resp"
mid="$(jq -r '.MessageID | gsub("-";"")' <<<"$resp")"  # Postmark UUID form of the 32-hex message ID
echo "accepted, MessageID $mid"

step "Waiting for the message in the mail catcher"
mp_id=""
find_message() {
  local found
  found="$(curl -sf -G "$MAILPIT/api/v1/search" --data-urlencode "query=subject:\"Quickstart $nonce\"")" || return 1
  mp_id="$(jq -r '.messages[0].ID // empty' <<<"$found" 2>/dev/null)" || return 1
  [ -n "$mp_id" ]
}
retry 120 "delivery to Mailpit" find_message
rawfile="$(mktemp)"
curl -sf "$MAILPIT/api/v1/message/$mp_id/raw" -o "$rawfile" || fail "could not fetch the raw message from Mailpit"
dkim="$(tr -d '\r' <"$rawfile" | awk 'BEGIN{h=0} /^$/{exit} /^DKIM-Signature:/{h=1} h&&/^[ \t]/{print; next} h&&!/^DKIM-Signature:/{h=0} /^DKIM-Signature:/{print}')"
[ -n "$dkim" ] || fail "no DKIM-Signature header on the delivered message"
grep -q "d=$DOMAIN" <<<"$dkim" && grep -q 's=mail1' <<<"$dkim" || fail "unexpected DKIM-Signature: $dkim"
echo "DKIM-Signature present (d=$DOMAIN s=mail1)"

step "Verifying the DKIM signature cryptographically against the local DNS"
compose run --rm --no-deps -T --entrypoint dkimverify tools-dns <"$rawfile" || fail "DKIM verification failed"

step "Delivery state in the operator API"
delivered() {
  local list
  list="$(api GET "/api/v1/apps/$APP/messages" 2>/dev/null)" || return 1
  jq -e . >/dev/null 2>&1 <<<"$list" || return 1
  [ "$(jq -r --arg id "$mid" '[.messages[] | select(.id==$id)] | .[0].status // empty' <<<"$list")" = delivered ]
}
retry 120 "the operator API to show the message delivered" delivered
echo "operator API reports the message delivered"

step "SMTP submission (587 STARTTLS and 465 implicit TLS)"
# The Edge listens on the same sending IP and certificate as port 25. Port 587
# and 465 need the sending IP's row to name the host clients connect to.
SMTP_HOST=smtp.quickstart.example
smtp_info="$(api GET "/api/v1/apps/$APP/smtp")" || fail "reading the SMTP tab data failed"; json "SMTP connection info" "$smtp_info"
if jq -e '.connection.primary_unnamed == true' >/dev/null <<<"$smtp_info"; then
  api POST /api/v1/sending-ips "{\"name\":\"quickstart-primary\",\"address\":\"11.42.51.10\",\"helo_name\":\"$SMTP_HOST\",\"bounce_domain\":\"$SMTP_HOST\"}" >/dev/null || fail "naming the primary sending IP failed"
  smtp_info="$(api GET "/api/v1/apps/$APP/smtp")" || fail "reading the SMTP tab data failed"
fi
jq -e --arg h "$SMTP_HOST" '.connection | .host == $h' >/dev/null <<<"$smtp_info" || fail "unexpected SMTP host: $(jq -c .connection <<<"$smtp_info")"
echo "clients connect to $SMTP_HOST"
ports_reported() { # the Edge reports 465 and 587 when it negotiates submission/1
  jq -e '(.connection.observed_ports // []) | (index(465) != null and index(587) != null)' >/dev/null <<<"$(api GET "/api/v1/apps/$APP/smtp" 2>/dev/null)"
}
retry 60 "the Edge to report ports 465 and 587" ports_reported
echo "the Edge reports ports 465 and 587"

cred="$(api POST "/api/v1/apps/$APP/smtp/credentials" "{\"name\":\"smoke-$nonce\"}")" || fail "creating an SMTP credential failed"; json "SMTP credential" "$cred"
CID="$(jq -r '.credential | .id // empty' <<<"$cred")"
SMTP_USER="$(jq -r '.username // empty' <<<"$cred")"
SMTP_PASS="$(jq -r '.password // empty' <<<"$cred")"
[ -n "$CID" ] && [ -n "$SMTP_USER" ] && [ -n "$SMTP_PASS" ] || fail "incomplete SMTP credential response"
echo "created SMTP credential $CID"

smtp_message() { # subject: a raw message with headers the SMTP path must remove or replace
  printf '%s\r\n' "From: Quickstart <hello@$DOMAIN>" "To: $RECIPIENT" "Bcc: hidden@$DOMAIN" \
    "Received: from smtp-client.marker.example by relay.marker.example; Mon, 01 Jan 2024 00:00:00 +0000" \
    "Message-ID: <client-$nonce@client.example.net>" "Subject: $1" "MIME-Version: 1.0" "Content-Type: text/plain; charset=utf-8" "" "Hello over SMTP."
}
# curl runs inside the tools container on the Edge's public network, trusting
# the quickstart CA, and reaches the Edge's sending IP by name.
smtp_send() { # url port mechanism subject password; prints curl's verbose log
  smtp_message "$4" | compose run --rm --no-deps -T --entrypoint curl tools-smtp -sSv --ssl-reqd --cacert /pki/ca.crt \
    --resolve "$SMTP_HOST:$2:11.42.51.10" "$1" --mail-from "hello@$DOMAIN" --mail-rcpt "$RECIPIENT" \
    --user "$SMTP_USER:$5" --login-options "AUTH=$3" -T - 2>&1
}
for leg in "smtp://$SMTP_HOST:587 587 PLAIN starttls" "smtps://$SMTP_HOST:465 465 LOGIN tls"; do
  set -- $leg
  subject="Quickstart SMTP $4 $nonce"
  log="$(smtp_send "$1" "$2" "$3" "$subject" "$SMTP_PASS")" || fail "SMTP submission on port $2 failed: $(printf %s "$log" | tail -n 15)"
  smtp_id="$(sed -n 's/.*250 2\.0\.0 OK: queued as \([0-9a-f]\{32\}\).*/\1/p' <<<"$log" | tail -n 1)"
  [ -n "$smtp_id" ] || fail "port $2 did not answer 250 with a message ID: $(printf %s "$log" | tail -n 15)"
  echo "port $2 ($3): accepted, message ID $smtp_id"
  mp_id=""
  find_smtp_message() {
    local found
    found="$(curl -sf -G "$MAILPIT/api/v1/search" --data-urlencode "query=subject:\"$subject\"")" || return 1
    mp_id="$(jq -r '.messages[0].ID // empty' <<<"$found" 2>/dev/null)" || return 1
    [ -n "$mp_id" ]
  }
  retry 120 "the SMTP message on port $2 in Mailpit" find_smtp_message
  curl -sf "$MAILPIT/api/v1/message/$mp_id/raw" -o "$rawfile" || fail "could not fetch the SMTP message from Mailpit"
  headers="$(tr -d '\r' <"$rawfile" | awk '/^$/{exit} {print}')"
  if grep -qi '^Bcc:' <<<"$headers"; then fail "the Bcc header was not removed (port $2)"; fi
  if grep -q 'marker.example' <<<"$headers"; then fail "the client's Received header was not removed (port $2)"; fi
  grep -q "^Message-ID: <" <<<"$headers" || fail "no Message-ID on the delivered message (port $2)"
  if grep -q "client-$nonce@client.example.net" <<<"$headers"; then fail "the client's Message-ID was kept (port $2)"; fi
  grep -q "^Message-ID: <[^>]*@$DOMAIN>" <<<"$headers" || fail "the Message-ID is not Thinpost's (port $2): $(grep '^Message-ID' <<<"$headers")"
  grep -q "d=$DOMAIN" <<<"$headers" || fail "no DKIM-Signature for $DOMAIN on the SMTP message (port $2)"
  compose run --rm --no-deps -T --entrypoint dkimverify tools-dns <"$rawfile" || fail "DKIM verification of the SMTP message failed (port $2)"
  echo "port $2: Bcc and the client Received header removed, Thinpost Message-ID set, DKIM verified"
done

# A wrong password is refused and never delivered.
if smtp_send "smtp://$SMTP_HOST:587" 587 PLAIN "Quickstart SMTP bad $nonce" "tps_$(printf 'A%.0s' $(seq 1 43))" >/dev/null 2>&1; then fail "a wrong SMTP password was accepted"; fi
echo "a wrong SMTP password is refused"

printf '\nSMOKE PASSED\n'
