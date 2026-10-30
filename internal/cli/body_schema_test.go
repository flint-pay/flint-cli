package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/flint-pay/flint-cli/internal/schemafixture"
)

func minimalRequestBody(t testing.TB, command *Command) map[string]any {
	t.Helper()
	schema, e := requestBodySchema(command)
	if e != nil {
		t.Fatal(e)
	}
	value, err := schemafixture.Minimal(schema)
	if err != nil {
		t.Fatalf("%s: %v", command.CanonicalName, err)
	}
	body, _ := value.(map[string]any)
	if body == nil {
		body = map[string]any{}
	}
	return body
}

// exhaustiveInputBody is the --input body for invocations that pass every
// argument: a minimal valid body plus an undocumented marker field wherever
// the schema admits undeclared fields, so tests can see the input forwarded.
func exhaustiveInputBody(t testing.TB, command *Command) map[string]any {
	t.Helper()
	marker := map[string]any{"input_marker": "present"}
	operation, _ := openAPIOperationByID(command.OperationID)
	if mediaType, _ := requestContentSchema(operation.RequestBody); command.CanonicalName == "api" || mediaType != "application/json" {
		return marker
	}
	if command.CanonicalName == "orders.pay" {
		// --payment-source-token supplies the whole confirm_payment_intents body.
		return map[string]any{}
	}
	// Mirror prepareRequest: body flags overwrite their paths in the input, and
	// repeatable flags append to whatever list the input already holds.
	withoutRepeats := func(value any) map[string]any {
		body, _ := value.(map[string]any)
		body = deepCopyMap(body)
		if command.CanonicalName == "payment-intents.confirm" {
			delete(body, "payment_source_token")
			delete(body, "payment_method_id")
		}
		for _, arg := range command.Arguments {
			if arg.Repeat && arg.BodyPath != "" {
				delete(body, strings.Split(arg.BodyPath, ".")[0])
			}
		}
		return body
	}
	withFlags := func(body map[string]any) map[string]any {
		body = deepCopyMap(body)
		if command.CanonicalName == "payment-intents.confirm" {
			delete(body, "payment_source_token")
			delete(body, "payment_method_id")
		}
		flagged := Options{Input: "-", Raw: map[string][]string{}}
		for _, arg := range command.Arguments {
			if skipExhaustiveConfirmationFlag(command, arg) {
				continue
			}
			if arg.BodyPath == "" || arg.Name == "input" || arg.Positional > 0 || arg.Query != "" || (command.CanonicalName == "payment-intents.create" && arg.Name == "order") {
				continue
			}
			flagged.Raw[strings.TrimPrefix(arg.Flag, "--")] = exhaustiveArgumentValues(arg)
			for _, value := range exhaustiveArgumentValues(arg) {
				converted, err := convertValue(value, arg.Type)
				if err != nil {
					t.Fatal(err)
				}
				if arg.Repeat {
					existing, _ := lookupPath(body, arg.BodyPath)
					list, _ := existing.([]any)
					converted = append(list, converted)
				}
				_ = setBodyPath(body, arg.BodyPath, converted)
			}
		}
		_ = selectRequestUnionTags(command, flagged, body)
		return body
	}
	schema, e := requestBodySchema(command)
	if e != nil {
		t.Fatal(e)
	}
	value, err := schemafixture.MinimalMatching(schema, func(v any) any { return withFlags(withoutRepeats(v)) })
	if err != nil {
		t.Fatalf("%s: no input body is valid with the exhaustive flags: %v", command.CanonicalName, err)
	}
	body := withoutRepeats(value)
	body["input_marker"] = "present"
	if _, e := validateRequestBody(command, command.CanonicalName, withFlags(body)); e != nil {
		delete(body, "input_marker")
	}
	return body
}

func jsonString(t testing.TB, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEveryBodyFlagTargetsADocumentedRequestField(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		if command.OperationID == "" || command.CanonicalName != command.Name {
			continue
		}
		v, e := requestBodyValidator(command)
		if e != nil {
			t.Fatal(e)
		}
		for _, arg := range command.Arguments {
			if arg.BodyPath == "" || arg.Name == "input" || arg.Positional > 0 || arg.Query != "" {
				continue
			}
			if (command.CanonicalName == "payment-intents.create" && arg.Name == "order") || (command.CanonicalName == "orders.pay" && arg.Name == "payment_source_token") {
				continue // Rewritten into a route or payment_intents before sending.
			}
			if field := strings.Split(arg.BodyPath, ".")[0]; !v.open && !containsString(v.declared, field) {
				t.Errorf("%s %s writes undocumented body field %s", command.CanonicalName, arg.Flag, field)
			}
		}
	}
}

func TestEveryRequestBodySchemaHasALocallyValidMinimalBody(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		if command.OperationID == "" || command.CanonicalName != command.Name {
			continue
		}
		operation, _ := openAPIOperationByID(command.OperationID)
		if mediaType, _ := requestContentSchema(operation.RequestBody); mediaType != "application/json" {
			continue
		}
		body := minimalRequestBody(t, command)
		if _, e := validateRequestBody(command, command.CanonicalName, body); e != nil {
			raw, _ := json.Marshal(body)
			t.Errorf("%s minimal body %s: %s", command.CanonicalName, raw, e.Message)
		}
	}
}

func runJSON(t *testing.T, app *App, stdout *bytes.Buffer, argv ...string) (int, map[string]any) {
	t.Helper()
	stdout.Reset()
	exit := app.Run(append(argv, "--output", "json"))
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("argv=%v stdout is not JSON: %v: %s", argv, err, stdout)
	}
	return exit, result
}

func TestSchemaInputDescribesTheInputBody(t *testing.T) {
	app, stdout, _ := testApp(t, "http://127.0.0.1:1")
	for _, name := range []string{"customers.addresses.create", "payment-intents.create", "orders.pay", "sandboxes.reset"} {
		exit, schema := runJSON(t, app, stdout, "schema", "input", name)
		if exit != ExitOK {
			t.Fatalf("%s exit=%d: %v", name, exit, schema)
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, leaked := range []string{"_flint", "customer_id", "order_id", "sandbox_id", "order", "amount", "currency"} {
			if _, ok := properties[leaked]; ok && (leaked != "customer_id" || name != "payment-intents.create") {
				t.Errorf("%s input schema contains CLI-only %s", name, leaked)
			}
		}
		if raw, _ := json.Marshal(schema); strings.Contains(string(raw), `"_flint"`) {
			t.Errorf("%s input schema mentions _flint: %s", name, raw)
		}
	}
	command, _ := app.Registry.ByName("payment-intents.create")
	body, _ := requestBodySchema(command)
	if body["required"] == nil || body["oneOf"] != nil {
		t.Errorf("payment-intents.create body schema should keep the public request shape: %v", body)
	}
	// The MCP tool schema keeps its CLI arguments and execution controls.
	tool, _ := schemaForMCPCommand(command)
	properties, _ := tool["properties"].(map[string]any)
	if properties["_flint"] == nil || properties["order"] == nil || tool["oneOf"] == nil {
		t.Errorf("MCP tool schema lost CLI arguments: %v", properties)
	}
}

func TestClientDryRunValidatesTheRequestBody(t *testing.T) {
	customerCreate := []string{"customers", "create"}
	cases := []struct {
		name, input, path, contains string
		command                     []string
	}{
		{"wrong type", `{"email":123}`, "$.email", "got number, want string", customerCreate},
		{"misspelled required", `{"emial":"a@example.com"}`, "$.emial", "did you mean email?", customerCreate},
		{"nested", `{"recipient_name":"Ada Lovelace","address":{"city":"X"}}`, "$.address.country", "required field is missing", []string{"customers", "addresses", "create", "cus_123"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, stdout, _ := testApp(t, "http://127.0.0.1:1")
			app.Stdin = strings.NewReader(tc.input)
			exit, result := runJSON(t, app, stdout, append(append([]string{}, tc.command...), "--input", "-", "--dry-run", "client")...)
			errorBody, _ := result["error"].(map[string]any)
			if exit != ExitUsage || errorBody["code"] != "INVALID_REQUEST_BODY" {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			details, _ := lookupPath(errorBody, "details.problems")
			problems, _ := details.([]any)
			found := false
			for _, raw := range problems {
				detail, _ := raw.(map[string]any)
				found = found || (detail["path"] == tc.path && strings.Contains(fmt.Sprint(detail["message"]), tc.contains))
			}
			if !found || !strings.Contains(fmt.Sprint(errorBody["message"]), tc.path) {
				t.Fatalf("details lack %s %q: %v", tc.path, tc.contains, errorBody)
			}
		})
	}
}

func TestInvalidRequestBodyIsNeverSent(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
			return
		}
		calls++
		fmt.Fprint(w, `{"data":{"customer_id":"cus_test"}}`)
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, server.URL)
	app.Stdin = strings.NewReader(`{"email":false}`)
	if exit, _ := runJSON(t, app, stdout, "customers", "create", "--input", "-"); exit != ExitUsage || calls != 0 {
		t.Fatalf("exit=%d calls=%d", exit, calls)
	}
	// Convenience flags merge before validation, so they can complete a body.
	app.Stdin = strings.NewReader(`{"name":"Ada"}`)
	if exit, result := runJSON(t, app, stdout, "customers", "create", "--input", "-", "--email", "ada@example.com"); exit != ExitOK || calls != 1 {
		t.Fatalf("exit=%d calls=%d result=%v", exit, calls, result)
	}
	// The raw api command remains the unvalidated escape hatch.
	app.Stdin = strings.NewReader(`{"emial":1}`)
	if exit, result := runJSON(t, app, stdout, "api", "post", "/v1/customers", "--input", "-", "--dry-run", "client"); exit != ExitOK {
		t.Fatalf("raw api dry-run exit=%d: %v", exit, result)
	}
}

func TestUndocumentedBodyFieldsAreRejected(t *testing.T) {
	app, stdout, _ := testApp(t, "http://127.0.0.1:1")
	for _, extra := range [][]string{nil, {"--quiet"}} {
		app.Stdin = strings.NewReader(`{"email":"a@example.com","nmae":"Ada"}`)
		argv := append([]string{"customers", "create", "--input", "-", "--dry-run", "client"}, extra...)
		exit, result := runJSON(t, app, stdout, argv...)
		if code, _ := lookupPath(result, "error.code"); exit != ExitUsage || code != "INVALID_REQUEST_BODY" || !strings.Contains(fmt.Sprint(result), "did you mean name?") || !strings.Contains(fmt.Sprint(result), "flint api") {
			t.Fatalf("exit=%d result=%v", exit, result)
		}
	}
	app.Stdin = strings.NewReader(`{"tip_money":{"amount":1,"currency":"USD"}}`)
	exit, result := runJSON(t, app, stdout, "payment-intents", "create", "--idempotency-key", "saved-test-action-1", "--order", "ord_test", "--input", "-", "--dry-run", "client")
	if exit != ExitUsage || !strings.Contains(fmt.Sprint(result), "payment-intents.create --order") {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
}

func TestMCPControlsInInputAreRejected(t *testing.T) {
	app, stdout, _ := testApp(t, "http://127.0.0.1:1")
	app.Stdin = strings.NewReader(`{"email":"a@example.com","_flint":{"live":true,"idempotency_key":"idem_1","dry_run":"client"}}`)
	exit, result := runJSON(t, app, stdout, "customers", "create", "--input", "-", "--dry-run", "client")
	message, _ := lookupPath(result, "error.message")
	if exit != ExitUsage || !strings.Contains(fmt.Sprint(message), "retired _flint control object") {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	for _, flag := range []string{"--live", "--idempotency-key", "--dry-run=client"} {
		if !strings.Contains(fmt.Sprint(message), flag) {
			t.Errorf("message does not name %s: %s", flag, message)
		}
	}
}

func TestInvalidInputJSONReportsLineAndColumn(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"{\n  \"email\": \"a@example.com\",\n}", "line 3, column 1"},
		{"{\"email\": tru}", "line 1, column 14"},
		{"{\n  \"name\": \"é\" x}", "line 2, column 15"},
		{"{\"email\":", "line 1, column 10: unexpected end of input"},
		{"\n[1]", "line 2, column 1: input must be a JSON object, not array"},
		{"{} {}", "line 1, column 4: input must contain exactly one JSON object"},
		{"null", "line 1, column 1: input must be a JSON object, not null"},
	}
	for _, tc := range cases {
		app, stdout, _ := testApp(t, "http://127.0.0.1:1")
		app.Stdin = strings.NewReader(tc.input)
		exit, result := runJSON(t, app, stdout, "customers", "create", "--input", "-", "--dry-run", "client")
		message, _ := lookupPath(result, "error.message")
		code, _ := lookupPath(result, "error.code")
		if exit != ExitUsage || code != "INVALID_INPUT_JSON" || !strings.Contains(fmt.Sprint(message), tc.want) {
			t.Errorf("input %q: exit=%d message=%v, want %q", tc.input, exit, message, tc.want)
		}
	}
}

func TestSchemaCommandsFiltersByPrefix(t *testing.T) {
	app, stdout, _ := testApp(t, "http://127.0.0.1:1")
	names := func(result map[string]any) []string {
		var out []string
		for _, raw := range result["data"].([]any) {
			out = append(out, raw.(map[string]any)["name"].(string))
		}
		return out
	}
	_, all := runJSON(t, app, stdout, "schema", "commands")
	if len(names(all)) != len(commandList(app.Registry)) {
		t.Fatalf("unfiltered catalog changed: %d commands", len(names(all)))
	}
	exit, customers := runJSON(t, app, stdout, "schema", "commands", "customers")
	if exit != ExitOK || len(names(customers)) == 0 {
		t.Fatalf("exit=%d result=%v", exit, customers)
	}
	for _, name := range names(customers) {
		if !strings.HasPrefix(name, "customers.") {
			t.Errorf("customers filter returned %s", name)
		}
	}
	_, dotted := runJSON(t, app, stdout, "schema", "commands", "customers.addresses")
	_, spaced := runJSON(t, app, stdout, "schema", "commands", "customers", "addresses")
	if !slices.Equal(names(dotted), names(spaced)) || !slices.Contains(names(dotted), "customers.addresses.create") || slices.Contains(names(dotted), "customers.create") {
		t.Fatalf("dotted=%v spaced=%v", names(dotted), names(spaced))
	}
	exit, unknown := runJSON(t, app, stdout, "schema", "commands", "custmers")
	suggestions, _ := lookupPath(unknown, "error.details")
	if code, _ := lookupPath(unknown, "error.code"); exit != ExitUsage || code != "UNKNOWN_COMMAND_PREFIX" || !strings.Contains(fmt.Sprint(suggestions), "customers") {
		t.Fatalf("exit=%d result=%v", exit, unknown)
	}
}

func TestSchemaLookupsAcceptSpacedNamesAndSuggest(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	for _, kind := range []string{"command", "input", "output"} {
		_, dotted := runJSON(t, app, stdout, "schema", kind, "customers.addresses.create")
		exit, spaced := runJSON(t, app, stdout, "schema", kind, "customers", "addresses", "create")
		if exit != ExitOK || !reflect.DeepEqual(dotted, spaced) {
			t.Fatalf("schema %s spaced exit=%d differs: %v", kind, exit, spaced)
		}
	}
	exit, unknown := runJSON(t, app, stdout, "schema", "input", "customer.create")
	if code, _ := lookupPath(unknown, "error.code"); exit != ExitUsage || code != "UNKNOWN_COMMAND_SCHEMA" || !strings.Contains(fmt.Sprint(unknown), "Did you mean customers.create?") {
		t.Fatalf("exit=%d result=%v", exit, unknown)
	}
	exit, group := runJSON(t, app, stdout, "schema", "command", "customers")
	if message, _ := lookupPath(group, "error.message"); exit != ExitUsage || !strings.Contains(fmt.Sprint(message), "flint schema commands customers") {
		t.Fatalf("exit=%d result=%v", exit, group)
	}
	stderr.Reset()
	if exit := app.Run([]string{"schema", "command", "customer", "create"}); exit != ExitUsage || !strings.Contains(stderr.String(), "Did you mean: customers.create") {
		t.Fatalf("human exit=%d stderr=%q", exit, stderr)
	}
}

func TestAPIKeyCreateAccessFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		exit int
		body map[string]any
	}{
		{"read only", []string{"--access", "read_only"}, ExitOK, map[string]any{"name": "ci", "access": "read_only"}},
		{"full", []string{"--access", "full"}, ExitOK, map[string]any{"name": "ci", "access": "full"}},
		{"restricted", []string{"--access", "restricted", "--scope", "customers.read"}, ExitOK, map[string]any{"name": "ci", "access": "restricted", "scopes": []any{"customers.read"}}},
		{"restricted without scope", []string{"--access", "restricted"}, ExitUsage, nil},
		{"full with scope", []string{"--access", "full", "--scope", "customers.read"}, ExitUsage, nil},
		{"scope implies restricted", []string{"--scope", "customers.read"}, ExitOK, map[string]any{"name": "ci", "access": "restricted", "scopes": []any{"customers.read"}}},
		{"no access choice", nil, ExitUsage, nil},
		{"unknown access", []string{"--access", "admin"}, ExitUsage, nil},
		{"input keeps its access", []string{"--input", `{"access":"read_only"}`}, ExitOK, map[string]any{"name": "ci", "access": "read_only"}},
		{"input scopes without access", []string{"--input", `{"scopes":["customers.read"]}`}, ExitUsage, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, stdout, _ := testApp(t, "http://127.0.0.1:1")
			argv := append([]string{"api-keys", "create", "--name", "ci"}, tc.args...)
			exit, result := runJSON(t, app, stdout, append(argv, "--dry-run", "client")...)
			if exit != tc.exit {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			if tc.body != nil && !reflect.DeepEqual(lookupPathValue(result, "data.body"), tc.body) {
				t.Fatalf("body=%v want %v", lookupPathValue(result, "data.body"), tc.body)
			}
		})
	}
}

// Every API key create and update path sends access: --scope alone selects
// restricted, and sandbox test-key flags imply issue_test_key.
func TestAPIKeyAccessFlagsOnUpdateAndSandboxCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		exit int
		code string
		body map[string]any
	}{
		{"update scope implies restricted", []string{"api-keys", "update", "key_123", "--scope", "customers.read", "--expected-version", "3"}, ExitOK, "", map[string]any{"access": "restricted", "scopes": []any{"customers.read"}, "expected_version": float64(3)}},
		{"update to read only", []string{"api-keys", "update", "key_123", "--access", "read_only", "--expected-version", "3"}, ExitOK, "", map[string]any{"access": "read_only", "expected_version": float64(3)}},
		{"update name only", []string{"api-keys", "update", "key_123", "--name", "ci"}, ExitOK, "", map[string]any{"name": "ci"}},
		{"update full with scope", []string{"api-keys", "update", "key_123", "--access", "full", "--scope", "customers.read", "--expected-version", "3"}, ExitUsage, "INVALID_REQUEST_BODY", nil},
		{"update input scopes without access", []string{"api-keys", "update", "key_123", "--input", `{"scopes":["customers.read"],"expected_version":3}`}, ExitUsage, "INVALID_REQUEST_BODY", nil},
		{"test key scope implies restricted", []string{"sandboxes", "test-key", "test_123", "--name", "qa", "--scope", "customers.read"}, ExitOK, "", map[string]any{"name": "qa", "access": "restricted", "scopes": []any{"customers.read"}}},
		{"test key full", []string{"sandboxes", "api-keys", "create", "test_123", "--name", "qa", "--access", "full"}, ExitOK, "", map[string]any{"name": "qa", "access": "full"}},
		{"test key without access", []string{"sandboxes", "test-key", "test_123", "--name", "qa"}, ExitUsage, "MISSING_REQUIRED_ARGUMENT", nil},
		{"sandbox scope issues a restricted key", []string{"sandboxes", "create", "--name", "qa", "--scope", "customers.read", "--test-key-name", "qa key"}, ExitOK, "", map[string]any{"name": "qa", "issue_test_key": true, "access": "restricted", "scopes": []any{"customers.read"}, "test_key_name": "qa key"}},
		{"sandbox read only key", []string{"sandboxes", "create", "--name", "qa", "--access", "read_only"}, ExitOK, "", map[string]any{"name": "qa", "issue_test_key": true, "access": "read_only"}},
		{"sandbox without key", []string{"sandboxes", "create", "--name", "qa"}, ExitOK, "", map[string]any{"name": "qa"}},
		{"sandbox key name without access", []string{"sandboxes", "create", "--name", "qa", "--test-key-name", "qa key"}, ExitUsage, "MISSING_REQUIRED_ARGUMENT", nil},
		{"sandbox declined key with access", []string{"sandboxes", "create", "--name", "qa", "--access", "full", "--input", `{"issue_test_key":false}`}, ExitUsage, "INVALID_REQUEST_BODY", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, stdout, _ := testApp(t, "http://127.0.0.1:1")
			t.Setenv("FLINT_API_KEY", "flint_live_synthetic")
			exit, result := runJSON(t, app, stdout, append(tc.argv, "--live", "--dry-run", "client")...)
			if exit != tc.exit || (tc.code != "" && lookupPathValue(result, "error.code") != tc.code) {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			if tc.body != nil && !reflect.DeepEqual(lookupPathValue(result, "data.body"), tc.body) {
				t.Fatalf("body=%v want %v", lookupPathValue(result, "data.body"), tc.body)
			}
		})
	}
}

// MCP tools take the request schema directly, so scopes without access are
// refused before any request.
func TestMCPAPIKeyCreateRequiresAccess(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	refused := mcpCall(t, app, mcpAllToolsOptions(t), "api-keys_create", map[string]any{"name": "ci", "scopes": []any{"customers.read"}, "_flint": map[string]any{"dry_run": "client"}})
	if refused["isError"] != true || lookupPathValue(mcpErrorJSON(t, refused), "error.code") != "INVALID_REQUEST" || lookupPathValue(mcpErrorJSON(t, refused), "error.details.0.param") != "access" {
		t.Fatalf("scopes without access = %s", mcpResultText(refused))
	}
	accepted := mcpCall(t, app, mcpAllToolsOptions(t), "api-keys_create", map[string]any{"name": "ci", "access": "restricted", "scopes": []any{"customers.read"}, "_flint": map[string]any{"dry_run": "client"}})
	want := map[string]any{"name": "ci", "access": "restricted", "scopes": []any{"customers.read"}}
	if body := lookupPathValue(mcpResultJSON(t, accepted), "data.body"); accepted["isError"] == true || !reflect.DeepEqual(body, want) {
		t.Fatalf("restricted create = %v (%s)", body, mcpResultText(accepted))
	}
}
