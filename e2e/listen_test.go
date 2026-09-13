package e2e_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	webhooksigning "github.com/flint-pay/flint-cli/internal/webhooksigning"
)

type forwardedRequest struct {
	body    []byte
	headers http.Header
}

func TestBuiltCLIListenForwardsSignedWebhook(t *testing.T) {
	t.Parallel()
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	payload := []byte(`{"payment_intent_id":"pi_test","status":"succeeded"}`)
	forwarded := make(chan forwardedRequest, 1)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read forwarded request: %v", err)
		}
		forwarded <- forwardedRequest{body: body, headers: r.Header.Clone()}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/developer/auth-context":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["webhooks.read","payments.payment_intents.read"]},"request_id":"req_test","meta":{"api_version":"2026-02-01"}}`)
		case "/v1/webhook-events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
			fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":%q,\"event_type\":\"payment_intent.succeeded\",\"payload\":%s}\n\n", eventID, eventID, payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	bin := buildCLI(t)

	command := exec.Command(bin, "listen", "--forward-to", local.URL, "--max-events", "1", "--timeout", "2s", "--output", "ndjson")
	command.Env = append(os.Environ(),
		"FLINT_API_KEY=flint_test_test",
		"FLINT_BASE_URL="+api.URL,
		"FLINT_NO_INPUT=1",
		"XDG_CONFIG_HOME="+t.TempDir(),
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run built CLI: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	secret := listenerSecret(t, stdout.Bytes())
	var request forwardedRequest
	select {
	case request = <-forwarded:
	case <-time.After(time.Second):
		t.Fatal("built CLI did not forward the webhook")
	}
	if !bytes.Equal(request.body, payload) {
		t.Fatalf("forwarded body = %s, want %s", request.body, payload)
	}
	if got := request.headers.Get("webhook-id"); got != eventID {
		t.Fatalf("webhook-id = %q, want %q", got, eventID)
	}
	timestamp, err := strconv.ParseInt(request.headers.Get("webhook-timestamp"), 10, 64)
	if err != nil {
		t.Fatalf("webhook-timestamp: %v", err)
	}
	wantSignature := webhooksigning.StandardWebhookSignatureHeader(eventID, timestamp, payload, secret)
	if got := request.headers.Get("webhook-signature"); got != wantSignature {
		t.Fatalf("webhook-signature = %q, want %q", got, wantSignature)
	}
	if !strings.Contains(stdout.String(), `"type":"checkpoint"`) || !strings.Contains(stdout.String(), `"events":1`) {
		t.Fatalf("listener checkpoint missing from output: %s", stdout.String())
	}
}

func listenerSecret(t *testing.T, output []byte) string {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record["type"] == "listener" {
			secret, _ := record["signing_secret"].(string)
			if secret != "" {
				return secret
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan CLI output: %v", err)
	}
	t.Fatalf("listener signing secret missing from output: %s", output)
	return ""
}
