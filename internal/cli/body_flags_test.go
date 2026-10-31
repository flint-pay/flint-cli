package cli

import (
	"context"
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
