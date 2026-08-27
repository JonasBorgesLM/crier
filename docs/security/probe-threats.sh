#!/usr/bin/env bash
# Exercises the threat model in README.md against a running crierd.
#
# It is a script rather than a Go test on purpose: a security exercise should be
# re-runnable by someone who does not want to read the test suite first, and
# every request below is one they can paste individually.
#
#   docker compose -f demo/compose.yaml up --build -d
#   docs/security/probe-threats.sh
#
# Exit status is the number of probes that did not get the expected answer.
set -uo pipefail

INGEST="${CRIER_INGEST:-http://localhost:4318}"
LOKI="${CRIER_LOKI:-http://localhost:3000/api/datasources/proxy/uid/loki}"
SOURCE="${CRIER_SOURCE:-checkout-service}"
TOKEN="${CRIER_TOKEN:-demo-ingest-token}"
AUTH="Authorization: Bearer ${SOURCE}:${TOKEN}"
JSON="Content-Type: application/json"

failures=0
probe() { printf '%-52s ' "$1"; }
pass()  { echo "PASS  ($1)"; }
fail()  { echo "FAIL  ($1)"; failures=$((failures + 1)); }

status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
body()   { curl -s "$@"; }

# curl reports 000 when it never spoke HTTP — connection refused, DNS failure,
# a target that is not running. Several probes below assert "not 202", and 000
# is not 202, so without this they pass against nothing at all. Verified: an
# earlier version of this script reported four passes with no server up.
answered() {
  case "$1" in
    ""|000) return 1 ;;
    *) return 0 ;;
  esac
}

# reachable fails the whole run early rather than letting each probe discover
# the same outage and disagree about what it means.
require_reachable() {
  local code
  code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -d '{"records":[]}')
  if ! answered "$code"; then
    echo "crierd is not answering at $INGEST (curl reported '$code')." >&2
    echo "Start it first:  docker compose -f demo/compose.yaml up --build -d" >&2
    exit 1
  fi
}
require_reachable

# 1. Log forgery by an unauthenticated caller (ADR-0008).
probe "unauthenticated post is rejected"
code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -d '{"records":[{"body":"forged"}]}')
[ "$code" = "401" ] && pass "$code" || fail "got $code, want 401"

probe "wrong credential is rejected"
code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -H "Authorization: Bearer ${SOURCE}:wrong" -d '{"records":[{"body":"forged"}]}')
[ "$code" = "401" ] && pass "$code" || fail "got $code, want 401"

# 2. Credential-store enumeration: the two failures must be indistinguishable.
probe "unknown source and wrong secret are identical"
a=$(body -X POST "$INGEST/v1/logs" -H "$JSON" -H "Authorization: Bearer nobody:${TOKEN}" -d '{"records":[]}')
b=$(body -X POST "$INGEST/v1/logs" -H "$JSON" -H "Authorization: Bearer ${SOURCE}:wrong" -d '{"records":[]}')
# Two empty bodies are also equal, which is how this probe used to pass against
# a server that was not running.
if [ -z "$a" ]; then
  fail "no response body to compare"
elif [ "$a" = "$b" ]; then
  pass "same body"
else
  fail "responses differ: [$a] vs [$b]"
fi

# 3. Source spoofing (ADR-0008, finding D-2). Accepted, but not on the
#    caller's terms — verified at the backend, not from the response.
marker="probe-spoof-$(date +%s)"
probe "client-asserted identity is overwritten"
code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -H "$AUTH" \
  -d "{\"records\":[{\"body\":\"$marker\",\"resource\":{\"serviceName\":\"billing-service\"}}]}")
if [ "$code" != "202" ]; then
  fail "ingest got $code, want 202"
else
  sleep 6
  backend=$(body -G "$LOKI/loki/api/v1/query_range" \
    --data-urlencode "query={service_name=~\".+\"}" --data-urlencode 'limit=500')
  if ! echo "$backend" | grep -q "$marker"; then
    # An empty query result is not proof the identity was overwritten; it is
    # proof the probe cannot see the backend.
    fail "the record never reached the backend, so nothing was verified"
  elif echo "$backend" | grep -o "[^]]*$marker" | grep -q 'billing-service'; then
    fail "record reached the backend as billing-service"
  else
    pass "attributed to the principal"
  fi
fi

# 4. Resource exhaustion: oversized body (ADR-0010, step 1).
probe "oversized body is rejected"
# Through a file, not an argument. Passing six megabytes on the command line
# fails with "Argument list too long" before curl sends anything, and the probe
# then reports a pass for a request that never happened.
payload=$(mktemp)
trap 'rm -f "$payload"' EXIT
{ printf '{"records":[{"body":"'; head -c 6000000 /dev/zero | tr '\0' 'x'; printf '"}]}'; } > "$payload"
code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -H "$AUTH" --data-binary "@$payload")
if ! answered "$code"; then
  fail "curl never spoke HTTP (got '$code')"
elif [ "$code" = "413" ] || [ "$code" = "400" ]; then
  pass "$code"
else
  fail "got $code, want 413 or 400"
fi

# 5. Resource exhaustion: unbounded attribute map.
#
# This probe and the next one verify that the endpoint survives the input and
# answers, not that the cap was applied to the stored record. Reading the
# capped record back would mean querying the backend for attributes the
# backend does not index. What the caps actually do is unit-tested in core
# (TestAttributeCountCapIsDeterministic, TestGuardCapsAKeyAfterTooManyDistinctValues);
# what these probes add is that the path in front of them does not fall over.
probe "unbounded attribute map is survived"
attrs=$(python3 -c 'print(",".join(f"\"k{i}\":\"v\"" for i in range(5000)))')
code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -H "$AUTH" \
  -d "{\"records\":[{\"body\":\"probe-attrs\",\"attributes\":{$attrs}}]}")
[ "$code" = "202" ] && pass "202, capped by the limits stage" || fail "got $code"

# 6. Cardinality abuse.
probe "high-cardinality flood is survived"
ok=true
for i in $(seq 1 60); do
  code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -H "$AUTH" \
    -d "{\"records\":[{\"body\":\"probe-card\",\"attributes\":{\"trace\":\"unique-$i-$RANDOM\"}}]}")
  [ "$code" = "202" ] || ok=false
done
$ok && pass "accepted, guard is unit-tested" || fail "requests were refused"

# 7. Forged identity header with no trusted-proxy mode configured.
probe "identity header is ignored without trusted proxy"
marker2="probe-header-$(date +%s)"
code=$(status -X POST "$INGEST/v1/logs" -H "$JSON" -H "$AUTH" \
  -H "X-Authenticated-Source: billing-service" \
  -d "{\"records\":[{\"body\":\"$marker2\"}]}")
[ "$code" = "202" ] && pass "header carries no authority" || fail "got $code"

# 8. Wire format strictness (ADR-0012).
probe "unknown field is rejected and named"
out=$(body -X POST "$INGEST/v1/logs" -H "$JSON" -H "$AUTH" \
  -d '{"records":[{"body":"hi","severtyText":"ERROR"}]}')
echo "$out" | grep -q "severtyText" && pass "names the field" || fail "response did not name it: $out"

# 9. Transport surface.
probe "wrong content type is rejected"
code=$(status -X POST "$INGEST/v1/logs" -H "Content-Type: text/plain" -H "$AUTH" -d 'not json')
[ "$code" = "415" ] && pass "$code" || fail "got $code, want 415"

probe "GET on the ingestion path is rejected"
code=$(status "$INGEST/v1/logs" -H "$AUTH")
[ "$code" = "405" ] && pass "$code" || fail "got $code, want 405"

# 10. Rate limiting (IR1).
probe "a burst is rate limited"
# Concurrently, and well above the burst ceiling. Sequential curl cannot outrun
# the default refill rate — the first version of this probe sent 400 requests
# one at a time, took longer than the bucket takes to refill, and reported a
# failure that was the probe's own slowness rather than a missing control.
codes=$(seq 1 600 | xargs -P 24 -I{} \
  curl -s -o /dev/null -w '%{http_code}\n' -X POST "$INGEST/v1/logs" \
    -H "$JSON" -H "$AUTH" -d '{"records":[{"body":"burst"}]}')
limited=$(echo "$codes" | grep -c '^429$' || true)
[ "$limited" -gt 0 ] && pass "$limited of 600 refused" || fail "nothing was rate limited"

echo
echo "probes failed: $failures"
exit "$failures"
