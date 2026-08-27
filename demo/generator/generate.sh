#!/bin/sh
# The example service. It posts logs to crierd over the v1 wire format, in a
# loop, and one of them carries a secret on purpose.
#
# Deliberately a shell script rather than a Go program: it keeps the demo to
# one build, and what it does should be readable without trusting it.
set -eu

ENDPOINT="${CRIER_ENDPOINT:-http://crierd:4318/v1/logs}"
HEALTH="${CRIER_HEALTH:-http://crierd:9464/healthz}"
SOURCE="${CRIER_SOURCE:-checkout-service}"
TOKEN="${CRIER_TOKEN:-demo-ingest-token}"
INTERVAL="${CRIER_INTERVAL:-3}"

post() {
  # Failures are printed and skipped: a generator that exits on the first
  # refusal turns a transient into a demo that looks broken.
  wget -q -O- \
    --header="Content-Type: application/json" \
    --header="Authorization: Bearer ${SOURCE}:${TOKEN}" \
    --post-data="$1" \
    "$ENDPOINT" || echo "post failed, retrying next tick"
}

echo "waiting for crierd at ${HEALTH}"
until wget -q -O /dev/null "$HEALTH" 2>/dev/null; do
  sleep 1
done
echo "crierd is up; generating logs every ${INTERVAL}s"

n=0
while true; do
  n=$((n + 1))

  post '{"records":[
    {"severityNumber":9,"severityText":"INFO","body":"checkout completed",
     "attributes":{"order.id":"ord-'"$n"'","amount.cents":4990,"payment.method":"card"}},
    {"severityNumber":13,"severityText":"WARN","body":"payment provider slow to respond",
     "attributes":{"provider":"acme-pay","latency.ms":2300}}
  ]}'

  # The record the demo exists for. Both secrets are real shapes: an AWS access
  # key ID interpolated into free text, and a credential under a key that reads
  # as sensitive. Neither should reach the backend intact.
  post '{"records":[
    {"severityNumber":17,"severityText":"ERROR",
     "body":"failed to upload receipt: AWS key AKIAIOSFODNN7EXAMPLE rejected by S3",
     "attributes":{"api_key":"sk-live-51H8xQ2eZvKYlo2C","bucket":"receipts","retry":true}}
  ]}'

  # A record claiming to be someone else. crier overwrites the identity from
  # the authenticated principal and counts the discrepancy (ADR-0008).
  post '{"records":[
    {"severityNumber":9,"severityText":"INFO","body":"this record claims to come from billing-service",
     "resource":{"serviceName":"billing-service"}}
  ]}'

  sleep "$INTERVAL"
done
