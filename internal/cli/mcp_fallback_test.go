package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Parse the shell quoting emitted by quoteRemediationArgument without invoking
// a shell, expanding values or requiring a platform-specific shell installation.
func mcpFallbackWords(text string) []string {
	words := []string{}
	var word strings.Builder
	quote := rune(0)
	escaped, started := false, false
	for _, ch := range text {
		if escaped {
			word.WriteRune(ch)
			escaped = false
			started = true
			continue
		}
		if quote == '\'' {
			if ch == '\'' {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			started = true
			continue
		}
		if quote == '"' {
			if ch == '"' {
				quote = 0
			} else if ch == '\\' {
				escaped = true
			} else {
				word.WriteRune(ch)
			}
			started = true
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			started = true
		case '\\':
			escaped = true
		case ' ', '\t', '\n':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if started {
		words = append(words, word.String())
	}
	return words
}

func TestMCPFallbackRoundTripsPreparedRequest(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"name": "--leading 'quoted' name\nwith $() and `literal`", "metadata": map[string]any{"a.b": "a \"value\""}})
	for _, args := range [][]string{
		{"customers", "create", "--input", string(body), "--idempotency-key", "a key with 'quotes'"},
		{"customers", "update", "cus_roundtrip", "--input", string(body)},
		{"refunds", "create", "--input", `{"payment_intent_id":"pi_roundtrip","refund_type":"amount","amount_money":{"amount":1234,"currency":"KWD"}}`, "--idempotency-key", "refund-key"},
		{"orders", "list", "--status", "open", "--status", "closed", "--customer-id", "cus_roundtrip", "--page-size", "5", "--page-token=--next 'page'\n", "--created-after", "7d"},
		{"customers", "get", "cus_roundtrip", "--expand", "default_payment_method", "--expand", "default_payment_method"},
		{"resource-timelines", "get", "--page-size", "3", "--page-token=--next 'page'\n", "--", "--resource/with 'quotes'\nand spaces"},
	} {
		t.Run(strings.Join(args[:min(2, len(args))], "."), func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			cmd, opts, _, e := parseInvocation(app.Registry, args)
			if e != nil {
				t.Fatal(e)
			}
			resolved := ResolvedConfig{ProfileName: "a profile 'with quotes'", ContextID: "ctx_roundtrip", MerchantGuard: "mer_roundtrip", Sources: map[string]string{"profile": "flag"}}
			before, e := app.prepareRequest(context.Background(), cmd, opts, resolved, "flint_test_synthetic", "http://127.0.0.1:1")
			if e != nil {
				t.Fatal(e)
			}
			command, err := app.cliCommandFor(cmd, before, resolved)
			if err != nil {
				t.Fatal(err)
			}
			words := mcpFallbackWords(command)
			cmd2, opts2, _, e := parseInvocation(app.Registry, words[1:])
			if e != nil {
				t.Fatalf("%s: %v", command, e)
			}
			if opts2.Confirm || opts2.NoInput || opts2.Output != "human" || opts2.Profile != resolved.ProfileName || opts2.ContextID != resolved.ContextID || opts2.Merchant != resolved.MerchantGuard || !opts2.Live {
				t.Fatalf("terminal selection/approval changed: %s %+v", command, opts2)
			}
			if cmd.CanonicalName == "customers.update" && (commandIdempotencyClass(cmd) != "optional" || before.IdempotencyKey == "" || opts2.IdempotencyKey != before.IdempotencyKey) {
				t.Fatalf("generated optional key was lost: %s", command)
			}
			after, e := app.prepareRequest(context.Background(), cmd2, opts2, resolved, "flint_test_synthetic", "http://127.0.0.1:1")
			if e != nil {
				t.Fatalf("%s: %v", command, e)
			}
			if before.Method != after.Method || before.Path != after.Path || string(before.Body) != string(after.Body) || before.IdempotencyKey != after.IdempotencyKey {
				t.Fatalf("request changed: %s\n%+v\n%+v", command, before, after)
			}
		})
	}
}

func TestMCPFallbackPinsProfileSourcesAndProjectGuards(t *testing.T) {
	for _, source := range []string{"flag", "environment", "project", "global"} {
		t.Run(source, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_WORKLOAD_IDENTITY_ID", "FLINT_PROFILE", "FLINT_MERCHANT"} {
				t.Setenv(name, "")
			}
			profile := "--a profile 'with quotes'"
			if err := app.saveConfig(Config{DefaultProfile: profile, Profiles: map[string]Profile{profile: {ContextID: "ctx_roundtrip"}}}); err != nil {
				t.Fatal(err)
			}
			project := ProjectConfig{Merchant: "mer_roundtrip", Sandbox: "test_roundtrip"}
			args := []string{"customers", "get", "cus_roundtrip"}
			switch source {
			case "flag":
				args = append(args, "--profile="+profile)
			case "environment":
				t.Setenv("FLINT_PROFILE", profile)
			case "project":
				project.Profile = profile
			}
			raw, err := json.Marshal(project)
			if err != nil {
				t.Fatal(err)
			}
			projectDir := filepath.Join(app.WorkingDir, ".flint")
			if err := os.MkdirAll(projectDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDir, "config.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd, opts, _, e := parseInvocation(app.Registry, args)
			if e != nil {
				t.Fatal(e)
			}
			resolved, _, err := app.resolveConfig(opts)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Sources["profile"] != source {
				t.Fatalf("profile source = %q", resolved.Sources["profile"])
			}
			req, e := app.prepareRequest(context.Background(), cmd, opts, resolved, "flint_test_synthetic", "http://127.0.0.1:1")
			if e != nil {
				t.Fatal(e)
			}
			command, err := app.cliCommandFor(cmd, req, resolved)
			if err != nil {
				t.Fatal(err)
			}
			_, opts2, _, e := parseInvocation(app.Registry, mcpFallbackWords(command)[1:])
			if e != nil {
				t.Fatalf("%s: %v", command, e)
			}
			if (opts2.Profile != "") != (source != "global") {
				t.Fatalf("profile pin changed: %s", command)
			}
			resolved2, _, err := app.resolveConfig(opts2)
			if err != nil {
				t.Fatal(err)
			}
			if resolved2.ProfileName != resolved.ProfileName || resolved2.ContextID != resolved.ContextID || resolved2.MerchantGuard != resolved.MerchantGuard || resolved2.SandboxGuard != resolved.SandboxGuard {
				t.Fatalf("selection changed: %s\n%+v\n%+v", command, resolved, resolved2)
			}
		})
	}
}

func TestMCPFallbackRejectsUnrepresentableRequest(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	cmd, _ := app.Registry.ByName("customers.get")
	for _, req := range []preparedRequest{
		{Method: "DELETE", Path: "/v1/customers/cus_roundtrip"},
		{Method: "GET", Path: "/v1/orders/cus_roundtrip"},
		{Method: "GET", Path: "/v1/customers/cus_roundtrip?unknown=lost"},
		{Method: "GET", Path: "/v1/customers/cus_roundtrip?expand=%zz"},
		{Method: "GET", Path: "https://example.invalid/v1/customers/cus_roundtrip"},
		{Method: "GET", Path: "/v1/customers/cus_%00"},
	} {
		if command, err := app.cliCommandFor(cmd, req, ResolvedConfig{}); err == nil {
			t.Fatalf("rendered unsupported request %+v as %q", req, command)
		}
	}
}

func TestMCPFallbackLocalHistoryLeavesApprovalToTerminal(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	history, _ := app.Registry.ByName("history")
	_, clear := mcpHistoryTools(history)
	command, err := app.cliCommandFor(clear, preparedRequest{}, ResolvedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, opts, _, e := parseInvocation(app.Registry, mcpFallbackWords(command)[1:])
	if e != nil || !opts.Clear || opts.Confirm || opts.Live || opts.NoInput {
		t.Fatalf("local fallback changed: %s %+v %v", command, opts, e)
	}
}

func TestMCPFallbackShellWords(t *testing.T) {
	if !reflect.DeepEqual(mcpFallbackWords("'' 'one'\"'\"'two' '--flag=--value'"), []string{"", "one'two", "--flag=--value"}) {
		t.Fatal("shell quote parser failed")
	}
}
