package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestBodyUnionMerge(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		code      string
		want      map[string]any
		overrides []any
	}{
		{args: []string{"customers", "create", "--input", `{"name":"template","phone":"old"}`, "--name", "caller"}, want: map[string]any{"name": "caller", "phone": "old"}, overrides: []any{"name"}},
		{args: []string{"api-keys", "create", "--name", "ci", "--scopes", "customers.read"}, want: map[string]any{"name": "ci", "access": "restricted", "scopes": []any{"customers.read"}}},
		{args: []string{"api-keys", "create", "--input", `{"name":"ci","access":"restricted","scopes":["customers.write"]}`, "--scopes", "customers.read"}, want: map[string]any{"name": "ci", "access": "restricted", "scopes": []any{"customers.read"}}, overrides: []any{"scopes"}},
		{args: []string{"api-keys", "create", "--name", "ci", "--access", "full", "--scopes", "customers.read"}, code: "FLAG_NOT_IN_VARIANT"},
		{args: []string{"api-keys", "create", "--input", `{"name":"ci","access":"full"}`, "--access", "read_only"}, code: "CONFLICTING_ARGUMENTS"},
		{args: []string{"customers", "create", "--name", "one", "--name", "two"}, code: "DUPLICATE_FLAG"},
		{args: []string{"customers", "create", "--name", "same", "--name", "same"}, want: map[string]any{"name": "same"}},
		{args: []string{"payment-intents", "create", "--input", `{"amount_money":"wrong"}`, "--amount", "10"}, code: "CONFLICTING_ARGUMENTS"},
		{args: []string{"payment-intents", "confirm", "pi_123", "--payment-method-id", "pm_123", "--confirmation-token", "ctok_test"}, code: "AMBIGUOUS_VARIANT"},
		{args: []string{"payment-intents", "confirm", "pi_123"}, code: "INVALID_REQUEST_BODY"},
		{args: []string{"payment-intents", "confirm", "pi_123", "--credential-type", "attached"}, want: map[string]any{"credential_type": "attached"}},
		{args: []string{"api-keys", "update", "key_123", "--scopes", "customers.read", "--expected-version", "3"}, code: "MISSING_REQUIRED_ARGUMENT"},
		{args: []string{"api-keys", "update", "key_123", "--access", "restricted", "--scopes", "customers.read", "--expected-version", "3"}, want: map[string]any{"access": "restricted", "scopes": []any{"customers.read"}, "expected_version": float64(3)}},
		{args: []string{"sandboxes", "create", "--name", "qa", "--access", "restricted", "--scopes", "customers.read"}, code: "MISSING_REQUIRED_ARGUMENT"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			app, out, stderr := testApp(t, "http://127.0.0.1:9")
			args := append(append([]string{}, tc.args...), "--dry-run=client", "--idempotency-key", "union-test")
			exit, result := runJSON(t, app, out, args...)
			if tc.code != "" {
				if got, _ := lookupPath(result, "error.code"); exit != ExitUsage || got != tc.code {
					t.Fatalf("exit=%d result=%v stderr=%s", exit, result, stderr)
				}
				return
			}
			body, _ := lookupPath(result, "data.body")
			if exit != 0 || !reflect.DeepEqual(body, tc.want) {
				t.Fatalf("exit=%d body=%#v result=%v", exit, body, result)
			}
			if tc.overrides != nil {
				got, _ := lookupPath(result, "data.overridden_fields")
				if !reflect.DeepEqual(got, tc.overrides) {
					t.Fatal(got, tc.overrides)
				}
			}
		})
	}
}

func TestBodyUnionNestedOverrideAndMissingSources(t *testing.T) {
	app, out, stderr := testApp(t, "http://127.0.0.1:9")
	exit, result := runJSON(t, app, out, "payment-intents", "capture", "pi_123", "--input", `{"amount_money":{"amount":10,"currency":"USD"}}`, "--amount", "20", "--debug", "--dry-run=client", "--idempotency-key", "override")
	got, _ := lookupPath(result, "data.body.amount_money.amount")
	if exit != 0 || got != float64(20) || !strings.Contains(stderr.String(), "--amount overrides amount_money.amount from --input") {
		t.Fatal(exit, result, stderr)
	}
	out.Reset()
	exit, result = runJSON(t, app, out, "api-keys", "create", "--access", "full", "--dry-run=client")
	message, _ := lookupPath(result, "error.message")
	if exit != 2 || !strings.Contains(message.(string), "--name or --input") {
		t.Fatal(exit, result)
	}
}
