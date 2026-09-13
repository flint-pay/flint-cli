package e2e_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flint-pay/flint-cli/internal/cli"
)

func TestBuiltCLIExecutesEveryRemoteCommand(t *testing.T) {
	bin := buildCLI(t)
	for _, command := range cli.NewRegistry().Commands {
		command := command
		if command.Local || command.Stream {
			continue
		}
		t.Run(command.CanonicalName, func(t *testing.T) {
			environment := "sandbox"
			credential := "flint_test_exhaustive"
			if strings.HasPrefix(command.CanonicalName, "sandboxes.") {
				environment = "live"
				credential = "flint_live_exhaustive"
			}
			resourceCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprintf(
						w,
						`{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":%q,"merchant_id":"mer_test","sandbox_id":"test_test","scopes":["customers.read","customers.write","payments.payment_intents.read","payments.payment_intents.write","webhooks.read","webhooks.write"]},"request_id":"req_auth","meta":{"api_version":"2026-02-01"}}`,
						environment,
					)
					return
				}
				resourceCalls++
				if command.ResponseMediaType != "" {
					w.Header().Set("Content-Type", command.ResponseMediaType)
					fmt.Fprint(w, "test file content")
					return
				}
				fmt.Fprint(w, `{"data":{"resource_id":"res_test","status":"succeeded","url":"https://example.com/result"},"request_id":"req_test"}`)
			}))
			defer server.Close()

			canonicalArgs := builtRemoteInvocation(command)
			canonicalExit, canonicalStdout, canonicalStderr := runBuiltCLI(
				t, bin, canonicalArgs, credential, server.URL,
			)
			if canonicalExit != cli.ExitOK {
				t.Fatalf(
					"argv=%v exit=%d stdout=%s stderr=%s",
					canonicalArgs, canonicalExit, canonicalStdout, canonicalStderr,
				)
			}
			var output any
			if err := json.Unmarshal([]byte(canonicalStdout), &output); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, canonicalStdout)
			}
			if command.CanonicalName != "auth.status" && resourceCalls == 0 {
				t.Fatal("built command made no API request")
			}

			for index, aliasPath := range command.AliasPaths {
				aliasArgs := append([]string(nil), aliasPath[1:]...)
				aliasArgs = append(aliasArgs, canonicalArgs[len(command.Path)-1:]...)
				aliasExit, aliasStdout, aliasStderr := runBuiltCLI(
					t, bin, aliasArgs, credential, server.URL,
				)
				if aliasExit != canonicalExit || aliasStdout != canonicalStdout || aliasStderr != canonicalStderr {
					t.Fatalf(
						"compiled alias %s differs\ncanonical exit=%d stdout=%q stderr=%q\nalias exit=%d stdout=%q stderr=%q",
						command.Aliases[index],
						canonicalExit, canonicalStdout, canonicalStderr,
						aliasExit, aliasStdout, aliasStderr,
					)
				}
			}
		})
	}
}

func TestBuiltCLIExecutesEveryCommandAndAliasHelp(t *testing.T) {
	bin := buildCLI(t)
	registry := cli.NewRegistry()
	for _, command := range registry.Commands {
		command := command
		t.Run(command.CanonicalName, func(t *testing.T) {
			assertBuiltCLIHelp(t, bin, command.Path[1:], command)
			for index, aliasPath := range command.AliasPaths {
				t.Run("alias-"+command.Aliases[index], func(t *testing.T) {
					assertBuiltCLIHelp(t, bin, aliasPath[1:], command)
				})
			}
		})
	}
}

func TestBuiltCLISchemaContainsEveryRegisteredCommand(t *testing.T) {
	bin := buildCLI(t)
	command := exec.Command(bin, "schema", "commands", "--output", "json")
	command.Env = append(os.Environ(), "FLINT_API_KEY=", "FLINT_NO_INPUT=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("schema commands: %v\n%s", err, output)
	}
	var envelope struct {
		Data []struct {
			CanonicalName string `json:"canonical_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		t.Fatalf("schema output is not JSON: %v\n%s", err, output)
	}
	registry := cli.NewRegistry()
	if len(envelope.Data) != len(registry.Commands) {
		t.Fatalf("schema commands=%d, registry commands=%d", len(envelope.Data), len(registry.Commands))
	}
	seen := make(map[string]bool, len(envelope.Data))
	for _, command := range envelope.Data {
		seen[command.CanonicalName] = true
	}
	for _, command := range registry.Commands {
		if !seen[command.CanonicalName] {
			t.Errorf("built CLI schema omitted %s", command.CanonicalName)
		}
	}
}

func runBuiltCLI(t *testing.T, bin string, argv []string, credential, baseURL string) (int, string, string) {
	t.Helper()
	configHome := t.TempDir()
	for _, configDir := range []string{
		filepath.Join(configHome, "flint"),
		filepath.Join(configHome, "Library", "Application Support", "flint"),
	} {
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"default_profile":"default","profiles":{"default":{"agent_feedback_submission":"enabled"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	process := exec.Command(bin, argv...)
	process.Env = append(
		os.Environ(),
		"FLINT_API_KEY="+credential,
		"FLINT_BASE_URL="+baseURL,
		"FLINT_NO_INPUT=1",
		"XDG_CONFIG_HOME="+configHome,
		"HOME="+configHome,
	)
	process.Stdin = strings.NewReader(`{"input_marker":"present"}`)
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	err := process.Run()
	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("start built CLI: %v", err)
		}
		exit = exitErr.ExitCode()
	}
	return exit, stdout.String(), stderr.String()
}

func builtRemoteInvocation(command *cli.Command) []string {
	argv := append([]string(nil), command.Path[1:]...)
	for _, argument := range command.Arguments {
		if command.CanonicalName == "payment-intents.create" && argument.Name == "order" {
			continue
		}
		if argument.Name == "save_to" {
			continue
		}
		values := builtArgumentValues(argument)
		if argument.Positional > 0 {
			argv = append(argv, values[0])
			continue
		}
		for _, value := range values {
			if argument.Type == "boolean" {
				argv = append(argv, argument.Flag+"="+value)
			} else {
				argv = append(argv, argument.Flag, value)
			}
		}
	}
	argv = append(argv, "--output", "json", "--timeout", "2s")
	if command.CanonicalName == "api" {
		argv = append(argv, "--confirm")
	}
	if command.Mutation {
		argv = append(argv, "--idempotency-key", "idem_compiled_exhaustive")
	}
	if command.Destructive {
		argv = append(argv, "--confirm")
	}
	if strings.HasPrefix(command.CanonicalName, "sandboxes.") {
		argv = append(argv, "--live")
		if command.Mutation && !command.Destructive {
			argv = append(argv, "--confirm")
		}
	}
	return argv
}

func builtArgumentValues(argument cli.Arg) []string {
	if argument.Type == "boolean" {
		return []string{"true"}
	}
	if argument.Type == "time" {
		return []string{"2h"}
	}
	value := argument.IDPrefix + "test"
	if argument.IDPrefix == "" {
		value = map[string]string{
			"amount":               "2500",
			"cancellation_reason":  "abandoned",
			"capture_method":       "automatic",
			"command":              "customers.list",
			"confirmation_token":   "ctoken_test",
			"country":              "US",
			"created_after":        "2h",
			"created_before":       "2026-07-23T11:00:00Z",
			"currency":             "USD",
			"delivery_status":      "succeeded",
			"email":                "dev@example.com",
			"enabled_event":        "payment_intent.succeeded",
			"event_type":           "payment_intent.succeeded",
			"input":                "-",
			"item_name":            "Test item",
			"method":               "POST",
			"name":                 "Test",
			"page_size":            "2",
			"page_token":           "page_test",
			"path":                 "/v1/payment-intents",
			"payment_option":       "card",
			"payment_source_token": "pi_test=pm_card_visa",
			"quick_pay_name":       "Test",
			"reason":               "requested_by_customer",
			"recipient_email":      "dev@example.com",
			"request_id":           "req_test",
			"scope":                "customers.read",
			"status":               "succeeded",
			"status_bucket":        "success",
			"transaction_purpose":  "services",
			"url":                  "https://example.com/webhooks/flint",
		}[argument.Name]
	}
	if value == "" {
		value = "test"
	}
	if !argument.Repeat {
		return []string{value}
	}
	second := value + "_second"
	switch argument.Name {
	case "enabled_event", "event_type":
		second = "refund.created"
	case "payment_option":
		second = "ach_debit"
	case "payment_source_token":
		second = "pi_second=pm_card_mastercard"
	case "scope":
		second = "customers.write"
	}
	return []string{value, second}
}

func assertBuiltCLIHelp(t *testing.T, bin string, path []string, command *cli.Command) {
	t.Helper()
	argv := append(append([]string(nil), path...), "--help")
	process := exec.Command(bin, argv...)
	process.Env = append(os.Environ(), "FLINT_API_KEY=", "FLINT_NO_INPUT=1")
	output, err := process.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, output)
	}
	text := string(output)
	if !strings.Contains(text, "Usage:") || !strings.Contains(text, strings.Join(command.Path, " ")) {
		t.Fatalf("%s returned incomplete help:\n%s", strings.Join(argv, " "), text)
	}
	if !strings.Contains(text, "Examples:") {
		t.Fatalf("%s omitted examples:\n%s", strings.Join(argv, " "), text)
	}
}

func buildCLI(t *testing.T) string {
	t.Helper()
	name := "flint"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", bin, "../cmd/flint")
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	return bin
}
