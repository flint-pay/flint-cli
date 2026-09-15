#!/usr/bin/env bash
set -euo pipefail

test "${FLINT_API_KEY#flint_test_}" != "$FLINT_API_KEY" || { echo "A sandbox API key is required." >&2; exit 1; }
: "${FLINT_BIN:?Set FLINT_BIN to the CLI binary}"
work_dir="$(mktemp -d)"
export LISTENER_OUTPUT="$work_dir/listener.ndjson"
export RECEIVER_RESULT="$work_dir/receiver.json"
export RECEIVER_READY="$work_dir/receiver.ready"
customer_output="$work_dir/customer.json"
listener_pid=""
receiver_pid=""
cleanup() {
  result=$?
  trap - EXIT
  if test "$result" -ne 0; then
    echo "Sandbox listener check failed (exit $result). CLI error codes:" >&2
    # Never print raw listener records: they contain the signing secret.
    for output in "$LISTENER_OUTPUT" "$customer_output"; do
      if test -f "$output"; then
        jq -c 'select(.error != null) | .error | {type, code, request_id}' "$output" >&2 || true
      fi
    done
  fi
  for child in "$listener_pid" "$receiver_pid"; do
    if test -n "$child"; then
      kill "$child" 2>/dev/null || true
      wait "$child" 2>/dev/null || true
    fi
  done
  rm -rf "$work_dir"
  exit "$result"
}
trap cleanup EXIT
python3 - <<'PY' &
import base64
import hashlib
import hmac
import json
import os
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        deadline = time.time() + 10
        secret = ""
        while time.time() < deadline and not secret:
            try:
                with open(os.environ["LISTENER_OUTPUT"], encoding="utf-8") as output:
                    for line in output:
                        record = json.loads(line)
                        if record.get("type") == "listener":
                            secret = record.get("signing_secret", "")
                            break
            except (FileNotFoundError, json.JSONDecodeError):
                pass
            if not secret:
                time.sleep(0.05)
        event_id = self.headers.get("webhook-id", "")
        timestamp = self.headers.get("webhook-timestamp", "")
        signatures = self.headers.get("webhook-signature", "").split()
        signed = event_id.encode() + b"." + timestamp.encode() + b"." + body
        expected = base64.b64encode(hmac.new(base64.b64decode(secret.removeprefix("whsec_")), signed, hashlib.sha256).digest()).decode()
        valid = bool(event_id and timestamp and any(value == "v1," + expected for value in signatures))
        with open(os.environ["RECEIVER_RESULT"], "w", encoding="utf-8") as result:
            json.dump({"valid_signature": valid, "event_id": event_id, "body": json.loads(body)}, result)
        self.send_response(204 if valid else 400)
        self.end_headers()

    def log_message(self, *_):
        pass

server = HTTPServer(("127.0.0.1", 18765), Handler)
open(os.environ["RECEIVER_READY"], "w", encoding="utf-8").close()
server.timeout = 75
server.handle_request()
PY
receiver_pid=$!
"$FLINT_BIN" listen \
  --forward-to http://127.0.0.1:18765 \
  --event-type customer.created \
  --max-events 1 \
  --timeout 30s \
  --for 60s \
  --output ndjson >"$LISTENER_OUTPUT" &
listener_pid=$!
for _ in $(seq 1 "${FLINT_LISTENER_STARTUP_ATTEMPTS:-300}"); do
  if test -f "$RECEIVER_READY" && grep -q '"type":"ready"' "$LISTENER_OUTPUT" 2>/dev/null; then
    break
  fi
  kill -0 "$listener_pid" 2>/dev/null || break
  sleep 0.1
done
test -f "$RECEIVER_READY"
grep -q '"type":"ready"' "$LISTENER_OUTPUT"
"$FLINT_BIN" customers create \
  --name "CLI release ${GITHUB_REF_NAME:-local}" \
  --email "cli-release-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}@example.com" \
  --idempotency-key "cli-listener-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}" \
  --output json >"$customer_output"
wait "$listener_pid"
wait "$receiver_pid"
jq -e '.valid_signature == true and .event_id != "" and .body.event_type == "customer.created"' "$RECEIVER_RESULT"
jq -s -e 'any(.[]; .type == "forward" and .status_code == 204) and any(.[]; .type == "checkpoint" and .cursor != "")' "$LISTENER_OUTPUT"
