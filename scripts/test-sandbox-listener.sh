#!/usr/bin/env bash
set -euo pipefail

test "${FLINT_API_KEY#flint_test_}" != "$FLINT_API_KEY" || { echo "A sandbox API key is required." >&2; exit 1; }
: "${FLINT_BIN:?Set FLINT_BIN to the CLI binary}"
work_dir="$(mktemp -d)"
export LISTENER_OUTPUT="$work_dir/listener.ndjson"
export RECEIVER_RESULT="$work_dir/receiver.json"
export RECEIVER_READY="$work_dir/receiver.ready"
customer_output="$work_dir/customer.json"
endpoint_output="$work_dir/endpoint.json"
endpoint_id=""
auth_output="$work_dir/auth.json"
listener_pid=""
receiver_pid=""
cleanup() {
  result=$?
  trap - EXIT
  if test "$result" -ne 0; then
    echo "Sandbox listener check failed (exit $result). CLI error codes:" >&2
    # Never print raw listener records: they can contain event data.
    for output in "$auth_output" "$LISTENER_OUTPUT" "$customer_output" "$endpoint_output"; do
      if test -f "$output"; then
        jq -c 'select((.error | type) == "object") | .error | {type, code, request_id}' "$output" >&2 || true
      fi
    done
    if test -f "$LISTENER_OUTPUT"; then
      echo "Listener checkpoints reached:" >&2
      jq -r '.type | select(. == "listener" or . == "ready" or . == "checkpoint" or . == "forward")' "$LISTENER_OUTPUT" >&2 || true
    fi
  fi
  for child in "$listener_pid" "$receiver_pid"; do
    if test -n "$child"; then
      kill "$child" 2>/dev/null || true
      wait "$child" 2>/dev/null || true
    fi
  done
  if test -n "$endpoint_id"; then
    "$FLINT_BIN" webhook-endpoints delete "$endpoint_id" --confirm --output json >/dev/null 2>&1 || echo "Could not remove disposable sandbox endpoint $endpoint_id." >&2
  fi
  rm -rf "$work_dir"
  exit "$result"
}
trap cleanup EXIT
echo "Checking sandbox authentication..."
"$FLINT_BIN" auth status --timeout 15s --debug --output json >"$auth_output"
jq -e '.data.environment == "sandbox" or .data.environment == "test"' "$auth_output" >/dev/null
echo "Creating a disposable sandbox JWT endpoint..."
"$FLINT_BIN" webhook-endpoints create \
  --url "https://cli-listener.invalid/webhooks" \
  --status active \
  --enabled-events customer.created \
  --signing-methods jwt \
  --output json >"$endpoint_output"
endpoint_id="$(jq -er '.data.webhook_endpoint_id' "$endpoint_output")"
export WEBHOOK_ENDPOINT_ID="$endpoint_id"
export RECEIVER_PORT=18765
export RECEIVER_ONCE=1
# The published SDK checks the JWT with its default verification and trusted
# staging JWKS discovery. Neither request frames nor JWTs are written to disk.
node e2e/receiver/receiver.mjs >/dev/null &
receiver_pid=$!
"$FLINT_BIN" listen \
  --forward-to http://127.0.0.1:18765 \
  --webhook-endpoint-id "$endpoint_id" \
  --event-type customer.created \
  --max-events 1 \
  --timeout 30s \
  --for 60s \
  --debug \
  --output ndjson >"$LISTENER_OUTPUT" &
listener_pid=$!
for _ in $(seq 1 "${FLINT_LISTENER_STARTUP_ATTEMPTS:-650}"); do
  if test -f "$RECEIVER_READY" && grep -q '"type":"ready"' "$LISTENER_OUTPUT" 2>/dev/null; then
    break
  fi
  kill -0 "$listener_pid" 2>/dev/null || break
  sleep 0.1
done
test -f "$RECEIVER_READY"
grep -q '"type":"ready"' "$LISTENER_OUTPUT"
echo "Event stream ready; creating sandbox customer..."
"$FLINT_BIN" customers create \
  --name "CLI release ${GITHUB_REF_NAME:-local}" \
  --email "cli-release-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}@example.com" \
  --idempotency-key "cli-listener-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}" \
  --output json >"$customer_output"
wait "$listener_pid"
wait "$receiver_pid"
jq -e '.valid_signature == true and .event_id != "" and .event_type == "customer.created"' "$RECEIVER_RESULT"
jq -s -e 'any(.[]; .type == "forward" and .status_code == 204) and any(.[]; .type == "checkpoint" and .cursor != "")' "$LISTENER_OUTPUT"
