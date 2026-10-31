package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGeneratedBodyFlagsInventory(t *testing.T) {
	registry := NewRegistry()
	total, shortcuts := 0, 0
	for _, cmd := range registry.Commands {
		paths := map[string]bool{}
		for _, arg := range cmd.Arguments {
			if arg.BodyPath == "" {
				continue
			}
			if paths[arg.BodyPath] {
				t.Fatalf("duplicate body path %s %s", cmd.Name, arg.BodyPath)
			}
			paths[arg.BodyPath] = true
			if strings.Contains(arg.BodyPath, ".") || cmd.OperationID == "payOrder" && arg.Name == "payment_source_token" {
				shortcuts++
				continue
			}
			total++
			if arg.Flag != "--"+strings.ReplaceAll(arg.BodyPath, "_", "-") {
				t.Fatalf("unexpected collision %s %s", cmd.Name, arg.Flag)
			}
		}
	}
	if total < 900 || shortcuts != 14 {
		t.Fatalf("body flags=%d shortcuts=%d", total, shortcuts)
	}
	for _, name := range []string{"api-keys.create", "api-keys.update", "sandboxes.api-keys.create"} {
		cmd, _ := registry.ByName(name)
		found := false
		for _, arg := range cmd.Arguments {
			if arg.Flag == "--expires-at" {
				found = true
			}
		}
		t.Logf("%s expires-at=%v", name, found)
		if name != "sandboxes.api-keys.create" && !found {
			t.Fatal(name)
		}
	}
}

func TestGeneratedBodyFlagValues(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	app.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		words string
		body  map[string]any
		code  string
	}{
		{`customers create --phone +15555550100 --tax-exempt false`, map[string]any{"phone": "+15555550100", "tax_exempt": false}, ""},
		{`webhook-endpoints update whep_123 --description null`, map[string]any{"description": nil}, ""},
		{`api-keys update key_123 --expires-at +14d`, map[string]any{"expires_at": "2026-11-14T12:00:00Z"}, ""},
		{`api-keys update key_123 --expires-at 14d`, nil, "AMBIGUOUS_RELATIVE_TIME"},
		{`webhook-endpoints update whep_123 --signing-methods jwt,hmac --expected-version 3`, map[string]any{"signing_methods": []any{"jwt", "hmac"}, "expected_version": int64(3)}, ""},
		{`webhook-endpoints update whep_123 --status actve`, nil, "INVALID_FLAG_VALUE"},
		{`api-keys update key_123 --expires-at null`, map[string]any{"expires_at": nil}, ""},
	} {
		t.Run(tc.words, func(t *testing.T) {
			cmd, opts, _, e := parseInvocation(app.Registry, strings.Fields(tc.words))
			var req preparedRequest
			if e == nil {
				opts.DryRun = "client"
				req, e = app.prepareRequest(context.Background(), cmd, opts, ResolvedConfig{}, "", "http://127.0.0.1:9")
			}
			if tc.code != "" {
				if e == nil || e.Code != tc.code {
					t.Fatalf("error=%v want %s", e, tc.code)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(req.BodyValue, tc.body) {
				t.Fatalf("body=%#v want %#v", req.BodyValue, tc.body)
			}
		})
	}
}

func TestGeneratedBodyBooleanScope(t *testing.T) {
	for _, words := range []string{"customers create --tax-exempt", "customers create --tax-exempt=false", "customers create --tax-exempt true", "trigger --list", "auth login --no-open"} {
		if _, _, _, e := parseInvocation(NewRegistry(), strings.Fields(words)); e != nil {
			t.Fatalf("%s: %v", words, e)
		}
	}
	if _, _, _, e := parseInvocation(NewRegistry(), []string{"customers", "create", "--no-open"}); e == nil {
		t.Fatal("accepted unrelated boolean")
	}
}

func TestGeneratedBodySignedTime(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]string{"now": "2026-10-31T12:00:00Z", "+2h": "2026-10-31T14:00:00Z", "-2h": "2026-10-31T10:00:00Z", "+1w": "2026-11-07T12:00:00Z"} {
		got, e := resolveTime(value, now)
		if e != nil || got != want {
			t.Fatal(value, got, e)
		}
	}
}

func TestGeneratedBodyExamplesAndHistoryPrefix(t *testing.T) {
	registry := NewRegistry()
	for _, cmd := range registry.Commands {
		for _, arg := range cmd.Arguments {
			if arg.BodyPath == "invoice_schedule_entry_id" && arg.IDPrefix != "invse_" {
				t.Fatal(cmd.Name, arg.IDPrefix)
			}
		}
		for _, example := range cmd.Examples {
			if strings.Contains(example, "--expires-at 2h") || strings.Contains(example, "--start-date value") || strings.Contains(example, "--end-date value") {
				t.Fatal(example)
			}
		}
	}
	cmd, _ := registry.ByName("api-keys.create")
	for _, arg := range cmd.Arguments {
		if arg.Name == "access" && !strings.Contains(arg.InferenceHint, "--scopes alone selects restricted") {
			t.Fatal(arg)
		}
		if arg.Name == "scopes" && arg.Discriminator != "--access" {
			t.Fatal(arg)
		}
	}
}

func TestGeneratedBodyFlagRoundTripMinimalRequests(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	checked := 0
	for _, cmd := range app.Registry.Commands {
		op, _ := openAPIOperationByID(cmd.OperationID)
		if media, _ := requestContentSchema(op.RequestBody); cmd.Local || media != "application/json" || cmd.OperationID == "payOrder" {
			continue
		}
		t.Run(cmd.Name, func(t *testing.T) {
			body := minimalRequestBody(t, cmd)
			raw, _ := json.Marshal(body)
			opts := defaultOptions()
			opts.Input = string(raw)
			opts.DryRun = "client"
			opts.IdempotencyKey = "body-roundtrip"
			for _, arg := range cmd.Arguments {
				if !generatedBodyArgument(arg) {
					continue
				}
				value, exists := body[arg.BodyPath]
				if !exists {
					continue
				}
				var values []string
				if value == nil {
					if !arg.Nullable {
						continue
					}
					values = []string{"null"}
				} else if array, ok := value.([]any); ok {
					if len(array) == 0 {
						continue
					}
					for _, item := range array {
						values = append(values, fmt.Sprint(item))
					}
				} else {
					values = []string{fmt.Sprint(value)}
				}
				opts.Raw[strings.TrimPrefix(arg.Flag, "--")] = values
				// A4a still appends repeated flags; exercise replacement in A4b.
				if arg.Repeat {
					delete(body, arg.BodyPath)
				}
				checked++
			}
			input, _ := json.Marshal(body)
			opts.Input = string(input)
			req, e := app.prepareRequest(context.Background(), cmd, opts, ResolvedConfig{}, "", "http://127.0.0.1:9")
			if e != nil {
				t.Fatal(e)
			}
			var normalized any
			_ = json.Unmarshal(req.Body, &normalized)
			expected := map[string]any{}
			_ = json.Unmarshal(raw, &expected)
			// Existing command-owned defaults can fill discriminator tags.
			for k, v := range expected {
				if !reflect.DeepEqual(normalized.(map[string]any)[k], v) {
					t.Fatalf("%s: got %#v want %#v", k, normalized, v)
				}
			}
		})
	}
	if checked < 150 {
		t.Fatal("too few flags exercised", checked)
	}
	t.Logf("round-tripped %d body flags", checked)
}

func TestGeneratedBodyNullableKinds(t *testing.T) {
	for _, tc := range []struct{ command, field, typ string }{{"inventory-levels.update", "safety_stock_quantity", "integer"}, {"customers.update", "tax_exempt", "boolean"}, {"webhook-endpoints.update", "description", "string"}} {
		cmd, _ := NewRegistry().ByName(tc.command)
		found := false
		for _, arg := range cmd.Arguments {
			if arg.BodyPath == tc.field {
				found = true
				if arg.Type != tc.typ {
					t.Fatal(tc, arg)
				}
			}
			if arg.BodyPath == "metadata" {
				t.Fatal("object got a flag", cmd.Name)
			}
		}
		if !found {
			t.Fatal(tc)
		}
	}
}
