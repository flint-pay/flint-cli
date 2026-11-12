package e2e_test

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuiltCLIListenDefaultJWTReceiver(t *testing.T) {
	const endpointID = "whep_fixture"
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	// An in-memory mock of the server's ES256 signer. The receiver uses the
	// published SDK's default issuer, token, audience, expiry and body checks.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "test-key", "x": enc(key.X.FillBytes(make([]byte, 32))), "y": enc(key.Y.FillBytes(make([]byte, 32)))}}})
	receiver := exec.Command("node", filepath.Join("receiver", "receiver.mjs"))
	receiver.Env = append(os.Environ(), "FLINT_RECEIVER_JWKS="+string(jwks), "WEBHOOK_ENDPOINT_ID="+endpointID, "RECEIVER_ONCE=1")
	pipe, err := receiver.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var receiverErr bytes.Buffer
	receiver.Stderr = &receiverErr
	if err = receiver.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Process.Kill(); _ = receiver.Wait() }()
	lines := bufio.NewScanner(pipe)
	if !lines.Scan() {
		t.Fatalf("receiver did not start: %s", receiverErr.String())
	}
	localURL := lines.Text()
	body := `{"webhook_event_id":"` + eventID + `","event_type":"customer.created","payload_version":2,"api_version":"2026-09-07","mode":"test","data":{"object":{"customer_id":"cus_fixture"}},"created_at":"2026-11-12T00:00:00Z","test":false}`
	hash := sha256.Sum256([]byte(body))
	now := time.Now()
	protected := enc([]byte(`{"alg":"ES256","kid":"test-key","typ":"flint-webhook+jwt"}`))
	claims, _ := json.Marshal(map[string]any{"iss": "https://api.withflintpay.com", "aud": endpointID, "iat": now.Unix(), "exp": now.Add(300 * time.Second).Unix(), "jti": "watt_fixture", "body_sha256": enc(hash[:])})
	signingInput := protected + "." + enc(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	token := "Bearer " + signingInput + "." + enc(signature)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth-context":
			fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["webhooks.read","webhooks.write","customers.read"]},"request_id":"req_test"}`)
		case "/v1/webhook-events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":%q,\"event_type\":\"customer.created\",\"payload\":{}}\n\n", eventID, eventID)
		case "/v1/webhook-endpoints/" + endpointID + "/sign-delivery":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"webhook_endpoint_id": endpointID, "webhook_event_id": eventID, "body": body, "expires_at": now.Add(300 * time.Second), "headers": map[string]string{"Authorization": token, "Content-Type": "application/json", "User-Agent": "Flint-Webhooks/1.0", "Flint-Webhook-Endpoint-Id": endpointID, "webhook-id": eventID, "webhook-timestamp": fmt.Sprint(now.Unix())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	command := exec.Command(buildCLI(t), "listen", "--forward-to", localURL, "--webhook-endpoint-id", endpointID, "--max-events", "1", "--timeout", "2s", "--output", "ndjson")
	configHome := t.TempDir()
	command.Env = append(os.Environ(), "HOME="+configHome, "APPDATA="+configHome, "FLINT_API_KEY=flint_test_test", "FLINT_BASE_URL="+api.URL, "FLINT_NO_INPUT=1", "XDG_CONFIG_HOME="+configHome)
	var out, errOut bytes.Buffer
	command.Stdout = &out
	command.Stderr = &errOut
	if err := command.Run(); err != nil {
		t.Fatalf("CLI did not deliver to default JWT verifier: %v; %s %s", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), `"forwarded":1`) {
		t.Fatal("receiver did not accept event")
	}
	if strings.Contains(out.String()+errOut.String(), token) || strings.Contains(out.String(), body) {
		t.Fatal("signed frame leaked")
	}
	// Configuration and history files must contain no signed frame either.
	_ = filepath.WalkDir(configHome, func(path string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			raw, _ := os.ReadFile(path)
			if bytes.Contains(raw, []byte(token)) {
				t.Error("signed frame persisted")
			}
		}
		return nil
	})
	_, _ = io.Copy(io.Discard, pipe)
}
