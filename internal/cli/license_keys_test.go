package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testLicenseValue   = "TEST-FIXTURE-LICENSE"
	testLicenseProduct = "prod_01KEZF7C00BVSHCNWVH528HB66"
)

func TestLicenseRequestsWithAndWithoutCredentials(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprint(authenticated), func(t *testing.T) {
			bearer := ""
			if authenticated {
				bearer = "Bearer flint_test_test"
			}
			paths := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != bearer {
					t.Error("unexpected authorization header")
				}
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				paths <- r.URL.Path
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if !(authenticated && strings.HasSuffix(r.URL.Path, "/release")) {
					if body["key"] != testLicenseValue || fmt.Sprint(body["product_ids"]) != "["+testLicenseProduct+"]" {
						t.Error("expected license key and accepted product ID")
					}
				}
				fmt.Fprint(w, `{"data":{"is_valid":true}}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			if !authenticated {
				t.Setenv("FLINT_API_KEY", "")
			}
			for _, tc := range []struct {
				path string
				args []string
			}{
				{"/v1/license-keys/validate", []string{"license-keys", "validate"}},
				{"/v1/license-key-activations", []string{"license-key-activations", "create", "--name", "test device"}},
				{"/v1/license-key-activations/lka_fixture/release", []string{"license-key-activations", "release", "lka_fixture"}},
				{"/v1/license-keys/validate", []string{"api", "POST", "/v1/license-keys/validate", "--input", `{"key":"` + testLicenseValue + `","product_ids":["` + testLicenseProduct + `"]}`}},
			} {
				if tc.args[0] != "api" && !(authenticated && tc.args[1] == "release") {
					tc.args = append(tc.args, "--key", testLicenseValue, "--product-ids", testLicenseProduct)
				}
				if exit, body := runJSON(t, app, out, tc.args...); exit != ExitOK {
					t.Fatalf("exit=%d result=%v", exit, body)
				}
				if got := <-paths; got != tc.path {
					t.Fatalf("request path %s, want %s", got, tc.path)
				}
			}
		})
	}
}

func TestLicenseKeyPoolFileUploadAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.csv")
	var csv strings.Builder
	csv.WriteString("\ufeffname,license_key\r\n")
	for i := range 1001 {
		fmt.Fprintf(&csv, "\"Fixture, Inc.\",TEST-POOL-%05d\r\n", i)
	}
	csv.WriteString("duplicate,TEST-POOL-00000\r\ninvalid,short\r\n")
	if err := os.WriteFile(path, []byte(csv.String()), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	firstRequests := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "POST" || r.URL.Path != "/v1/license-key-pools/lkp_fixture/keys" {
			t.Errorf("unexpected upload request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Keys      []string `json:"keys"`
			Confirmed bool     `json:"seller_authorization_confirmed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Confirmed || len(body.Keys) < 1 || len(body.Keys) > 1000 {
			t.Errorf("invalid batch size, payload, or missing attestation")
		}
		id := r.Header.Get("Idempotency-Key")
		if id == "" {
			t.Error("missing batch idempotency key")
		}
		order = append(order, id)
		if original, exists := firstRequests[id]; exists {
			if fmt.Sprint(original) != fmt.Sprint(body.Keys) {
				t.Error("replay sent a different batch")
			}
		} else {
			firstRequests[id] = body.Keys
		}
		additions := make([]map[string]any, len(body.Keys))
		for i, key := range body.Keys {
			outcome := "created"
			if key == "TEST-POOL-01000" {
				outcome = "existing"
			}
			additions[i] = map[string]any{"outcome": outcome}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"additions": additions}})
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	args := []string{"license-key-pools", "add-keys", "lkp_fixture", "--file", path, "--idempotency-key", "saved-fixture-upload"}
	if exit, body := runJSON(t, app, out, append(args, "--confirm")...); exit != ExitConfirmation || lookupPathValue(body, "error.code") != "SELLER_AUTHORIZATION_REQUIRED" {
		t.Fatalf("confirmation result: %d %v", exit, body)
	}
	if exit, body := runJSON(t, app, out, append(args, "--dry-run=client")...); exit != ExitOK || fmt.Sprint(lookupPathValue(body, "data.batches")) != "2" {
		t.Fatalf("dry run result: %d %v", exit, body)
	}
	app.IsTTY = func() bool { return true }
	app.Stdin = strings.NewReader("yes\n")
	out.Reset()
	if exit := app.Run(args); exit != ExitOK || !strings.Contains(stderr.String(), "Continue? [y/N]") || !strings.Contains(out.String(), "1000") {
		t.Fatalf("interactive upload result: %d %s %s", exit, out, stderr)
	}
	exit, result := runJSON(t, app, out, append(args, "--seller-authorization-confirmed")...)
	if exit != ExitOK || fmt.Sprint(result["data"]) != "map[added:1000 existing:2 rejected:1]" {
		t.Fatalf("upload result: %d %v %s", exit, result, stderr)
	}
	if strings.Contains(out.String()+stderr.String(), "TEST-POOL-") {
		t.Fatal("upload displayed full keys")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 4 || order[0] == order[1] || order[0] != order[2] || order[1] != order[3] || len(firstRequests[order[0]]) != 1000 || len(firstRequests[order[1]]) != 1 {
		t.Fatalf("unexpected batch sizes or idempotency keys")
	}
}

func TestLicenseKeyMCPGuardAndRedaction(t *testing.T) {
	app, _, _ := testApp(t, "")
	opts := defaultOptions()
	for _, operation := range []string{"license-keys.reveal", "license-keys.rotate-key"} {
		cmd := mustMCPTool(t, app, operation)
		entry, err := app.operationIndex()
		if err != nil {
			t.Fatal(err)
		}
		classification, ok := entry.Lookup(operation)
		if !ok || !classification.ConfirmRequired() || !strings.Contains(mcpBaseDescription(cmd), "private") {
			t.Fatalf("%s lacks secret-handling metadata", operation)
		}
	}
	args := map[string]any{"operation": "license-keys.validate",
		"arguments": map[string]any{"key": testLicenseValue, "product_ids": []any{testLicenseProduct}}, "dry_run": true}
	for _, tool := range []string{"flint_write", "flint_read"} {
		result := mcpCall(t, app, opts, tool, args)
		text := mcpResultText(result)
		if strings.Contains(text, testLicenseValue) || !strings.Contains(text, "[REDACTED]") {
			t.Fatalf("%s exposed the key or omitted the redacted request", tool)
		}
	}
	result := mcpCall(t, app, opts, "flint_write", map[string]any{
		"operation": "license-key-pools.add-keys",
		"arguments": map[string]any{"license_key_pool_id": "lkp_fixture", "file": "keys.csv"},
		"dry_run":   true,
	})
	if result["isError"] != true || !strings.Contains(mcpResultText(result), "file") {
		t.Fatal("MCP treated a local file as a supported input")
	}
}

func TestLicenseKeyFileTextAndMalformedCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(path, []byte("  SAMPLE,KEY  \r\n\nSAMPLE-KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	keys, _, rejected, e := readLicenseKeyFile(t.Context(), path)
	if e != nil || strings.Join(keys, "|") != "SAMPLE,KEY|SAMPLE-KEY" || rejected != 0 {
		t.Fatalf("plain text parsing failed: rejected=%d error=%v", rejected, e)
	}

	path = filepath.Join(t.TempDir(), "keys.csv")
	if err := os.WriteFile(path, []byte("key,name\nSAMPLE-KEY,example\nOTHER-KEY\n"), 0600); err != nil {
		t.Fatal(err)
	}
	keys, _, _, e = readLicenseKeyFile(t.Context(), path)
	if e == nil || e.Code != "INVALID_LICENSE_KEY_CSV" || len(keys) != 0 {
		t.Fatal("malformed CSV was accepted")
	}
	if err := os.WriteFile(path, []byte("Product License Key\n\"SAMPLE-KEY\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	keys, _, _, e = readLicenseKeyFile(t.Context(), path)
	if e == nil || e.Code != "INVALID_LICENSE_KEY_CSV" || len(keys) != 0 {
		t.Fatal("unrecognized single-column CSV header was treated as a key")
	}
}

func TestLicenseKeyMCPSecretResponsesRequireConfirmation(t *testing.T) {
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		requests <- r.URL.Path
		fmt.Fprintf(w, `{"data":{"key":%q}}`, testLicenseValue)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	for _, verb := range []string{"reveal", "rotate-key"} {
		input := map[string]any{
			"operation": "license-keys." + verb,
			"arguments": map[string]any{"license_key_id": "lk_fixture"},
		}
		result := mcpCall(t, app, defaultOptions(), "flint_write", input)
		if result["isError"] != true || !strings.Contains(mcpResultText(result), "CONFIRMATION_REQUIRED") || len(requests) != 0 {
			t.Fatalf("%s ran without confirmation: %v", verb, result)
		}
		input["confirm"] = true
		result = mcpCall(t, app, defaultOptions(), "flint_write", input)
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if result["isError"] == true || lookupPathValue(mcpResultJSON(t, result), "data.key") != mcpRedacted ||
			strings.Contains(string(raw), testLicenseValue) || len(requests) != 1 {
			t.Fatalf("confirmed %s did not return a redacted key", verb)
		}
		<-requests
		matches, err := filepath.Glob(filepath.Join(app.ConfigDir, "mcp-secrets", "result-*", "secrets.json"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("expected one private key file, got %d (%v)", len(matches), err)
		}
		if b, err := os.ReadFile(matches[0]); err != nil || !strings.Contains(string(b), testLicenseValue) {
			t.Fatalf("confirmed %s did not save the key for the user", verb)
		}
		if err := os.Remove(matches[0]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLicenseKeyUploadErrorUsesImportSeed(t *testing.T) {
	app, out, stderr := testApp(t, "")
	err := cliError(ExitAPI, errorTypeAPI, "RATE_LIMITED", "Try again later.")
	err.requestSent = true
	err.Details = map[string]any{"type": err.Type, "code": err.Code, "message": err.Message, "remediation": map[string]any{"retry": "same_request"}}
	const seed, batchKey = "saved-upload", "derived-batch-key"
	e := licenseKeyUploadError(withIdempotencyRetryContext(err, batchKey), seed, batchKey, 2, licenseKeyFileCounts{Added: 1000})
	opts := defaultOptions()
	app.writeError(e, opts)
	opts.Output = "json"
	app.writeError(e, opts)
	for _, text := range []string{out.String(), stderr.String()} {
		if !strings.Contains(text, "Before batch 2, 1000 keys were added") ||
			!strings.Contains(text, "--idempotency-key "+seed) || strings.Contains(text, "--idempotency-key "+batchKey) {
			t.Fatalf("incorrect import recovery advice: %s", text)
		}
	}
}

func TestLicenseKeyMCPFallbackUsesPrivateInputFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth-context" {
			t.Error("MCP sent a write without terminal approval")
		}
		fmt.Fprint(w, authContextJSON("live"))
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_test")
	opts := mcpServeOptions(t, "--allow-live")
	result := mcpCall(t, app, opts, "flint_write", map[string]any{
		"operation": "license-key-pools.remove-keys",
		"arguments": map[string]any{"license_key_pool_id": "lkp_fixture", "keys": []any{testLicenseValue}},
		"live":      true, "confirm": true,
	})
	text := mcpResultText(result)
	if result["isError"] != true || !strings.Contains(text, "CONFIRMATION_NEEDS_USER") || !strings.Contains(text, "--input=license-keys.json") ||
		!strings.Contains(text, "private file") || strings.Contains(text, testLicenseValue) {
		t.Fatalf("unexpected fallback: %v", result)
	}
}
