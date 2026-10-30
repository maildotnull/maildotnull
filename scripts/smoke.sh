#!/usr/bin/env bash
# Smoke test: spins up a throwaway MailDotNull instance and exercises every
# provider endpoint, all three SMTP listeners, the inbox API, and the
# error-testing faults. Local/dev use only — not part of CI, the build, or
# the container image. Needs bash + curl, nothing else.
#
# Usage: scripts/smoke.sh   (builds dist/maildotnull first if it is missing)
set -u
cd "$(dirname "$0")/.."

HTTP_PORT=8135
SMTP_PORT=12025
STARTTLS_PORT=12587
SMTPS_PORT=12465
BASE="http://localhost:$HTTP_PORT"
FROM="smoke@maildotnull.test"
TO="rcpt@maildotnull.test"

PASS=0
FAIL=0
ok()  { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n       %s\n' "$1" "$2"; }

# assert_contains <name> <needle> <haystack>
assert_contains() {
  if [[ "$3" == *"$2"* ]]; then ok "$1"; else bad "$1" "expected to contain [$2], got: ${3:0:200}"; fi
}

# post_json <path> <body> — echoes "<status>\n<body>"
post_json() {
  curl -s -o /tmp/smoke-body.$$ -w '%{http_code}' -X POST "$BASE$1" \
    -H 'Content-Type: application/json' -d "$2"
  echo
  cat /tmp/smoke-body.$$
}

# send_smtp <url> [extra curl args...] — sends $EML, echoes combined output, returns curl's exit code
send_smtp() {
  local url="$1"; shift
  curl -v -s --url "$url" --mail-from "$FROM" --mail-rcpt "$TO" --upload-file "$EML" "$@" 2>&1
}

msg_count() { curl -s "$BASE/api/messages" | grep -o '"id":"' | wc -l; }
first_id()  { curl -s "$BASE/api/messages" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4; }

# --- start a throwaway instance ----------------------------------------------

if [[ ! -x dist/maildotnull ]]; then
  echo "dist/maildotnull missing — running pnpm build"
  pnpm build || exit 1
fi

TMP=$(mktemp -d)
MAILDOTNULL_HTTP_PORT=$HTTP_PORT MAILDOTNULL_SMTP_PORT=$SMTP_PORT \
MAILDOTNULL_STARTTLS_PORT=$STARTTLS_PORT MAILDOTNULL_SMTPS_PORT=$SMTPS_PORT \
  ./dist/maildotnull >"$TMP/server.log" 2>&1 &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null; rm -rf "$TMP" /tmp/smoke-body.$$' EXIT

for _ in $(seq 1 50); do
  curl -s -o /dev/null "$BASE/api/messages" && break
  sleep 0.1
done
if ! curl -s -o /dev/null "$BASE/api/messages"; then
  echo "server did not come up — log:"; cat "$TMP/server.log"; exit 1
fi

# SSE listener runs in the background for the whole test; checked at the end.
curl -sN --max-time 60 "$BASE/api/events" >"$TMP/sse.log" 2>/dev/null &
SSE_PID=$!

# --- HTTP provider endpoints --------------------------------------------------

echo "providers:"

ATT_TEXT="smoke attachment content"
ATT_B64=$(printf '%s' "$ATT_TEXT" | base64)
HTML='<p>Hello from the <strong>smoke test</strong></p>'
PNG_B64='iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='  # 1x1 PNG

RES=$(post_json /emails "{\"from\":\"$FROM\",\"to\":[\"$TO\"],\"subject\":\"resend\",\"text\":\"hi\",\"html\":\"$HTML\"}")
assert_contains "resend accepts" '"id"' "$RES"

RES=$(curl -s -D "$TMP/sg-headers" -o /dev/null -w '%{http_code}' -X POST "$BASE/v3/mail/send" \
  -H 'Content-Type: application/json' \
  -d "{\"personalizations\":[{\"to\":[{\"email\":\"$TO\"}]}],\"from\":{\"email\":\"$FROM\"},\"subject\":\"sendgrid\",\"content\":[{\"type\":\"text/plain\",\"value\":\"hi\"},{\"type\":\"text/html\",\"value\":\"<img src='cid:smoke-logo'>\"}],\"attachments\":[{\"content\":\"$PNG_B64\",\"filename\":\"logo.png\",\"type\":\"image/png\",\"disposition\":\"inline\",\"content_id\":\"smoke-logo\"}]}")
[[ "$RES" == 202 ]] && grep -qi 'x-message-id' "$TMP/sg-headers" \
  && ok "sendgrid accepts (202 + X-Message-Id)" \
  || bad "sendgrid accepts" "status=$RES headers=$(cat "$TMP/sg-headers")"

RES=$(post_json /v3.1/send "{\"Messages\":[{\"From\":{\"Email\":\"$FROM\"},\"To\":[{\"Email\":\"$TO\"}],\"Subject\":\"mailjet\",\"TextPart\":\"hi\"}]}")
assert_contains "mailjet accepts" '"Status":"success"' "$RES"

printf '%s' "$ATT_TEXT" > "$TMP/file.txt"
RES=$(curl -s -X POST "$BASE/v3/smoke.example/messages" \
  -F "from=$FROM" -F "to=$TO" -F "subject=mailgun" -F "text=hi" \
  -F "attachment=@$TMP/file.txt;type=text/plain")
assert_contains "mailgun accepts (multipart + attachment)" 'Queued. Thank you.' "$RES"

RES=$(post_json /email "{\"From\":\"$FROM\",\"To\":\"$TO\",\"Subject\":\"postmark\",\"TextBody\":\"hi\"}")
assert_contains "postmark accepts" '"ErrorCode":0' "$RES"

RES=$(post_json /email/batch "[{\"From\":\"$FROM\",\"To\":\"$TO\",\"Subject\":\"batch1\",\"TextBody\":\"hi\"},{\"From\":\"$FROM\",\"To\":\"$TO\",\"Subject\":\"batch2\",\"TextBody\":\"hi\"}]")
[[ $(grep -o '"ErrorCode":0' <<<"$RES" | wc -l) == 2 ]] \
  && ok "postmark batch accepts both" || bad "postmark batch" "$RES"

RES=$(post_json /v3/smtp/email "{\"sender\":{\"email\":\"$FROM\"},\"to\":[{\"email\":\"$TO\"}],\"subject\":\"brevo\",\"textContent\":\"hi\"}")
assert_contains "brevo accepts" '"messageId"' "$RES"

RES=$(post_json /v2/email/outbound-emails "{\"FromEmailAddress\":\"$FROM\",\"Destination\":{\"ToAddresses\":[\"$TO\"]},\"Content\":{\"Simple\":{\"Subject\":{\"Data\":\"ses simple\"},\"Body\":{\"Text\":{\"Data\":\"hi\"}}}}}")
assert_contains "ses simple accepts" '"MessageId"' "$RES"

MIME_B64=$(printf 'From: %s\r\nTo: %s\r\nSubject: ses raw\r\n\r\nraw body\r\n' "$FROM" "$TO" | base64 -w0)
RES=$(post_json /v2/email/outbound-emails "{\"Content\":{\"Raw\":{\"Data\":\"$MIME_B64\"}}}")
assert_contains "ses raw accepts" '"MessageId"' "$RES"

# --- SMTP listeners -----------------------------------------------------------

echo "smtp:"

printf 'From: %s\r\nTo: %s\r\nSubject: smtp smoke\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary="XX"\r\n\r\n--XX\r\nContent-Type: text/plain\r\n\r\nhi\r\n--XX\r\nContent-Type: text/plain; name="s.txt"\r\nContent-Disposition: attachment; filename="s.txt"\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n--XX--\r\n' \
  "$FROM" "$TO" "$ATT_B64" > "$TMP/mail.eml"
EML="$TMP/mail.eml"

send_smtp "smtp://localhost:$SMTP_PORT" >/dev/null \
  && ok "plaintext :$SMTP_PORT accepts" || bad "plaintext :$SMTP_PORT" "curl failed"
send_smtp "smtp://localhost:$STARTTLS_PORT" --ssl-reqd -k >/dev/null \
  && ok "starttls :$STARTTLS_PORT accepts" || bad "starttls :$STARTTLS_PORT" "curl failed"
send_smtp "smtps://localhost:$SMTPS_PORT" -k --user "any:thing" >/dev/null \
  && ok "implicit tls :$SMTPS_PORT accepts (with AUTH)" || bad "implicit tls :$SMTPS_PORT" "curl failed"

# --- inbox API ----------------------------------------------------------------

echo "inbox api:"

COUNT=$(msg_count)
[[ "$COUNT" == 13 ]] && ok "all 13 messages stored" || bad "message count" "expected 13, got $COUNT"

LIST=$(curl -s "$BASE/api/messages")
for src in resend sendgrid mailjet mailgun postmark brevo ses smtp; do
  assert_contains "source '$src' present" "\"source\":\"$src\"" "$LIST"
done

# SendGrid inline attachments (content_id) resolve to data: URIs, like SMTP cid: parts.
SG_ID=$(echo "$LIST" | grep -o '{[^{}]*"source":"sendgrid"[^{}]*}' | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
SG=$(curl -s "$BASE/api/messages/$SG_ID")
assert_contains "sendgrid inline cid: image resolves" 'data:image/png;base64,' "$SG"

# Newest message is the SMTP one with an attachment: verify the round trip.
ID=$(first_id)
GOT=$(curl -s "$BASE/api/messages/$ID/attachments/0")
[[ "$GOT" == "$ATT_TEXT" ]] && ok "attachment round-trips byte-identical" \
  || bad "attachment round-trip" "got [$GOT]"

# Fetching the detail marks it read; PATCH flips it back.
curl -s -o /dev/null "$BASE/api/messages/$ID"
ENTRY=$(curl -s "$BASE/api/messages" | grep -o "\"id\":\"$ID\"[^}]*")
assert_contains "detail fetch marks read" '"read":true' "$ENTRY"
curl -s -o /dev/null -X PATCH "$BASE/api/messages/$ID" \
  -H 'Content-Type: application/json' -d '{"read":false}'
ENTRY=$(curl -s "$BASE/api/messages" | grep -o "\"id\":\"$ID\"[^}]*")
assert_contains "PATCH read:false works" '"read":false' "$ENTRY"

curl -s -o /dev/null -X PATCH "$BASE/api/messages"
UNREAD=$(curl -s "$BASE/api/messages" | grep -c '"read":false')
[[ "$UNREAD" == 0 ]] && ok "mark all read" || bad "mark all read" "$UNREAD still unread"

curl -s -o /dev/null -X DELETE "$BASE/api/messages/$ID"
COUNT=$(msg_count)
[[ "$COUNT" == 12 ]] && ok "delete removes one" || bad "delete" "expected 12, got $COUNT"

RES=$(post_json /api/link-check "{\"urls\":[\"$BASE/\"]}")
assert_contains "link check reaches a live URL" '"ok":true' "$RES"

# --- error testing ------------------------------------------------------------

echo "error testing:"

set_faults() { curl -s -X PUT "$BASE/api/error-testing" -H 'Content-Type: application/json' -d "$1"; }

COUNT_BEFORE=$(msg_count)

set_faults '{"http":"rate-limit"}' >/dev/null
RES=$(post_json /emails "{\"from\":\"$FROM\",\"to\":[\"$TO\"],\"subject\":\"x\",\"text\":\"x\"}")
assert_contains "global rate-limit: resend gets 429 shape" 'rate_limit_exceeded' "$RES"
RES=$(post_json /v2/email/outbound-emails "{\"FromEmailAddress\":\"$FROM\",\"Destination\":{\"ToAddresses\":[\"$TO\"]},\"Content\":{\"Simple\":{\"Subject\":{\"Data\":\"x\"},\"Body\":{\"Text\":{\"Data\":\"x\"}}}}}")
assert_contains "global rate-limit: ses gets TooManyRequestsException" 'TooManyRequestsException' "$RES"

set_faults '{"http":"off","providers":{"mailgun":"server-error"}}' >/dev/null
RES=$(curl -s -X POST "$BASE/v3/smoke.example/messages" -F "from=$FROM" -F "to=$TO" -F "subject=x" -F "text=x")
assert_contains "provider override: mailgun 500" 'Internal server error' "$RES"
RES=$(post_json /email "{\"From\":\"$FROM\",\"To\":\"$TO\",\"Subject\":\"y\",\"TextBody\":\"y\"}")
assert_contains "provider override: postmark unaffected" '"ErrorCode":0' "$RES"
curl -s -o /dev/null -X DELETE "$BASE/api/messages/$(first_id)" # remove the extra postmark mail

COUNT_AFTER=$(msg_count)
[[ "$COUNT_AFTER" == "$COUNT_BEFORE" ]] && ok "failed sends are not stored" \
  || bad "failed sends stored?" "count $COUNT_BEFORE -> $COUNT_AFTER"

set_faults '{"http":"delay","delaySeconds":1,"providers":{"mailgun":"inherit"}}' >/dev/null
T0=$SECONDS
RES=$(post_json /emails "{\"from\":\"$FROM\",\"to\":[\"$TO\"],\"subject\":\"delayed\",\"text\":\"x\"}")
ELAPSED=$((SECONDS - T0))
[[ "$RES" == *'"id"'* && "$ELAPSED" -ge 1 ]] && ok "delay fault: accepted after ${ELAPSED}s" \
  || bad "delay fault" "elapsed=${ELAPSED}s body=$RES"
curl -s -o /dev/null -X DELETE "$BASE/api/messages/$(first_id)"

set_faults '{"http":"timeout"}' >/dev/null
curl -s --max-time 3 -o /dev/null -X POST "$BASE/emails" \
  -H 'Content-Type: application/json' -d "{\"from\":\"$FROM\",\"to\":[\"$TO\"],\"subject\":\"x\",\"text\":\"x\"}"
[[ $? == 28 ]] && ok "timeout fault: client times out" || bad "timeout fault" "curl did not time out"
set_faults '{"http":"off"}' >/dev/null

OUT=$(set_faults '{"smtp":"reject-permanent"}'); assert_contains "set smtp reject-permanent" 'reject-permanent' "$OUT"
OUT=$(send_smtp "smtp://localhost:$SMTP_PORT")
assert_contains "smtp permanent reject (550)" '550' "$OUT"

set_faults '{"smtp":"reject-temporary"}' >/dev/null
OUT=$(send_smtp "smtp://localhost:$SMTP_PORT")
assert_contains "smtp temporary reject (451)" '451' "$OUT"

set_faults '{"smtp":"off","smtpAuth":"required"}' >/dev/null
OUT=$(send_smtp "smtp://localhost:$SMTP_PORT")
assert_contains "auth required: unauthenticated rejected (530)" '530' "$OUT"
send_smtp "smtp://localhost:$SMTP_PORT" --user "any:thing" >/dev/null \
  && ok "auth required: authenticated send accepted" \
  || bad "auth required with credentials" "curl failed"
curl -s -o /dev/null -X DELETE "$BASE/api/messages/$(first_id)"

RES=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$BASE/api/error-testing" \
  -H 'Content-Type: application/json' -d '{"http":"nonsense"}')
[[ "$RES" == 400 ]] && ok "invalid fault mode rejected (400)" || bad "invalid fault mode" "status=$RES"

set_faults '{"http":"off","smtp":"off","smtpAuth":"optional"}' >/dev/null
STATE=$(curl -s "$BASE/api/error-testing")
[[ "$STATE" == *'"http":"off"'* && "$STATE" == *'"smtp":"off"'* && "$STATE" == *'"smtpAuth":"optional"'* ]] \
  && ok "faults reset to off" || bad "faults reset" "$STATE"

# --- SSE + clear --------------------------------------------------------------

echo "events + clear:"

kill $SSE_PID 2>/dev/null; wait $SSE_PID 2>/dev/null
grep -q 'event: change' "$TMP/sse.log" && ok "SSE stream emitted change events" \
  || bad "SSE stream" "no change event seen"

curl -s -o /dev/null -X DELETE "$BASE/api/messages"
COUNT=$(msg_count)
[[ "$COUNT" == 0 ]] && ok "clear all empties the inbox" || bad "clear all" "count=$COUNT"

# --- summary ------------------------------------------------------------------

echo
echo "passed: $PASS   failed: $FAIL"
[[ $FAIL == 0 ]]
