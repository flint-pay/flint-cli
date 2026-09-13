package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
	webhooksigning "github.com/flint-pay/flint-cli/internal/webhooksigning"
)

func TestRegistryContract(t *testing.T) {
	registry := NewRegistry()
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if doc.FlintAPIVersion == "" {
		t.Fatal("public OpenAPI snapshot has no x-flint-api-version")
	}
	type operationRoute struct{ Method, Path string }
	operations := map[string]operationRoute{}
	for path, methods := range doc.Paths {
		for method, operation := range methods {
			operations[operation.OperationID] = operationRoute{Method: strings.ToUpper(method), Path: path}
		}
	}
	seen := map[string]bool{}
	for _, cmd := range registry.Commands {
		if seen[cmd.Name] {
			t.Fatalf("duplicate command %s", cmd.Name)
		}
		seen[cmd.Name] = true
		if len(cmd.Examples) == 0 {
			t.Errorf("%s has no help examples", cmd.Name)
		}
		if !cmd.Supports.JSONOutput || !cmd.Supports.NoInput {
			t.Errorf("%s lacks machine-mode support", cmd.Name)
		}
		if cmd.Mutation && (!cmd.Supports.IdempotencyKey || !cmd.Supports.DryRunClient) {
			t.Errorf("%s mutation lacks idempotency or dry-run", cmd.Name)
		}
		if cmd.OperationID != "" {
			route, exists := operations[cmd.OperationID]
			if !exists {
				t.Errorf("%s references missing operation %s", cmd.Name, cmd.OperationID)
			} else if route.Method != cmd.Method || route.Path != cmd.APIPath {
				t.Errorf("%s route %s %s differs from %s at %s %s", cmd.Name, cmd.Method, cmd.APIPath, cmd.OperationID, route.Method, route.Path)
			}
			operation, operationExists := openAPIOperationByID(cmd.OperationID)
			if !operationExists || (operation.FlintRouteClass == "" && cmd.OperationID != "authorizePartnerInstall" && cmd.OperationID != "previewPartnerInstallAuthorization" && cmd.OperationID != "exchangePartnerInstallToken" && cmd.OperationID != "getOpenAPISpec") {
				t.Errorf("%s operation %s has no public route class", cmd.Name, cmd.OperationID)
			}
			wantSensitive := operation.FlintRouteClass == "sensitive_write" || operation.FlintRouteClass == "external_provider_action" || operation.OperationID == "authorizePartnerInstall"
			if cmd.Sensitive != wantSensitive {
				t.Errorf("%s sensitive=%t, want %t from route class %q", cmd.Name, cmd.Sensitive, wantSensitive, operation.FlintRouteClass)
			}
		}
		for _, arg := range cmd.Arguments {
			if arg.IDPrefix != "" && !arg.AcceptsHistoryRef {
				t.Errorf("%s %s does not accept history references", cmd.Name, arg.Name)
			}
		}
		metadata := strings.ToLower(cmd.Name + " " + cmd.Description + " " + strings.Join(cmd.Examples, " "))
		if strings.Contains(metadata, "stripe") {
			t.Errorf("%s leaks provider plumbing", cmd.Name)
		}
		if strings.Contains(metadata, "\u2014") {
			t.Errorf("%s contains an em dash", cmd.Name)
		}
		if _, e := schemaForCommand(cmd, true); e != nil {
			t.Errorf("%s input schema: %v", cmd.Name, e)
		}
		if _, e := schemaForCommand(cmd, false); e != nil {
			t.Errorf("%s output schema: %v", cmd.Name, e)
		}
	}
	for aliasName, canonical := range map[string]string{"payment.create": "payment-intents.create", "checkout.create": "checkout-sessions.create", "checkout.get": "checkout-sessions.get", "checkout.list": "checkout-sessions.list", "whoami": "auth.status", "logout": "auth.logout"} {
		cmd, ok := registry.ByName(aliasName)
		if !ok || cmd.CanonicalName != canonical {
			t.Errorf("alias %s does not normalize to %s", aliasName, canonical)
		}
	}
}

func TestFulfillmentCommandsUseExplicitOpenAPIMetadata(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	for _, name := range []string{
		"delivery-rate-callbacks.check-connection",
		"delivery-rate-callbacks.test-deliveries",
	} {
		command, ok := registry.ByName(name)
		if !ok {
			t.Fatalf("missing command %s", name)
		}
		if command.Destructive {
			t.Fatalf("%s is unexpectedly destructive", name)
		}
	}
	for _, name := range []string{
		"delivery-rate-callbacks.create",
		"delivery-rate-callbacks.update",
	} {
		command, ok := registry.ByName(name)
		if !ok {
			t.Fatalf("missing command %s", name)
		}
		if !command.Sensitive {
			t.Fatalf("%s is not marked sensitive", name)
		}
	}
	rotation, ok := registry.ByName("delivery-rate-callbacks.rotate-secret")
	if !ok {
		t.Fatal("missing delivery-rate-callbacks.rotate-secret")
	}
	if !rotation.Destructive || !rotation.Sensitive {
		t.Fatalf(
			"rotate-secret destructive=%t sensitive=%t",
			rotation.Destructive,
			rotation.Sensitive,
		)
	}
	if len(rotation.Examples) == 0 ||
		!strings.Contains(rotation.Examples[0], "--confirm") {
		t.Fatalf("rotate-secret example = %v", rotation.Examples)
	}
}

func TestFulfillmentLifecycleResourcesHaveTypedCommands(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	for _, name := range []string{
		"fulfillments.transitions.create",
		"shipments.list",
		"shipments.get",
		"shipments.packages.create",
		"shipments.void",
		"packages.list",
		"packages.get",
		"packages.transitions.create",
		"packages.items.list",
		"packages.items.create",
		"packages.void",
	} {
		if _, ok := registry.ByName(name); !ok {
			t.Errorf("missing typed fulfillment command %s", name)
		}
	}
}

func TestInitialCLIScopesCoverBootstrapWorkflow(t *testing.T) {
	granted := map[string]bool{}
	for _, scope := range initialCLIScopes {
		if granted[scope] {
			t.Fatalf("duplicate initial CLI scope %q", scope)
		}
		granted[scope] = true
	}
	for _, cmd := range NewRegistry().Commands {
		// Bootstrap grants deliberately cover the initial payment workflow, not
		// every administrative operation exposed by the public API.
		bootstrap := map[string]bool{"customers.create": true, "customers.get": true, "orders.create": true, "orders.get": true, "orders.pay": true, "payment-intents.create": true, "payment-intents.get": true, "checkout-sessions.create": true, "webhook-endpoints.create": true, "webhook-events.list": true}
		if !bootstrap[cmd.CanonicalName] {
			continue
		}
		// Feedback scopes are intentionally absent from the signup key. Agent
		// submission grants write only after local consent, and read is always a
		// separate explicit key choice.
		if strings.HasPrefix(cmd.CanonicalName, "feedback-reports.") {
			continue
		}
		operation, ok := openAPIOperationByID(cmd.OperationID)
		if !ok || len(operation.FlintRequiredScopes) == 0 {
			continue
		}
		if operation.FlintScopesMode == "any" {
			covered := false
			for _, scope := range operation.FlintRequiredScopes {
				covered = covered || granted[scope]
			}
			if !covered {
				t.Errorf("%s requires one of %v, but the signup key grants none", cmd.CanonicalName, operation.FlintRequiredScopes)
			}
			continue
		}
		for _, scope := range operation.FlintRequiredScopes {
			if !granted[scope] {
				t.Errorf("%s requires %s, which is missing from the signup key", cmd.CanonicalName, scope)
			}
		}
	}
}

func TestCommandEnvironmentAffinityIsExplicitAndValid(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	valid := map[EnvironmentAffinity]bool{
		EnvironmentAffinityMerchant: true, EnvironmentAffinityAccount: true, EnvironmentAffinityNeutral: true,
	}
	for _, command := range registry.Commands {
		if command.Local {
			continue
		}
		if !valid[command.EnvironmentAffinity] {
			t.Errorf("%s has invalid environment affinity %q", command.CanonicalName, command.EnvironmentAffinity)
		}
	}
	for _, name := range []string{"feedback-reports.create", "feedback-reports.get", "feedback-reports.list"} {
		feedback, ok := registry.ByName(name)
		if !ok || feedback.EnvironmentAffinity != EnvironmentAffinityNeutral {
			t.Fatalf("%s affinity=%q", name, feedback.EnvironmentAffinity)
		}
	}
}

func TestJSONMutationRetriesWithOneIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.URL.Path != "/v1/payment-intents" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		mu.Lock()
		attempts++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		attempt := attempts
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"type":"unavailable_error","code":"SERVICE_UNAVAILABLE","message":"retry"}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"data":{"payment_intent":{"payment_intent_id":"pi_123","status":"requires_confirmation"}},"request_id":"req_123"}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"payment-intents", "create", "--amount", "2500", "--currency", "USD", "--payment-option", "card", "--output", "json", "--timeout", "3s"})
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v: %s", err, stdout)
	}
	if strings.TrimSpace(stderr.String()) != "" {
		t.Fatalf("unexpected stderr: %s", stderr)
	}
	if attempts != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("attempts=%d keys=%v", attempts, keys)
	}
}

func TestRequestsIdentifyCLIAndVersion(t *testing.T) {
	var userAgents []string
	var cliVersions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgents = append(userAgents, r.UserAgent())
		cliVersions = append(cliVersions, r.Header.Get("X-Flint-CLI-Version"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	app.Info.Version = "1.2.3"
	exit := app.Run([]string{"customers", "list", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if want := []string{"flintpay-cli/1.2.3", "flintpay-cli/1.2.3"}; !slices.Equal(userAgents, want) {
		t.Fatalf("user agents=%v, want %v", userAgents, want)
	}
	if want := []string{"1.2.3", "1.2.3"}; !slices.Equal(cliVersions, want) {
		t.Fatalf("CLI version headers=%v, want %v", cliVersions, want)
	}
}

func TestPaymentIntentCreateSendsRepeatablePaymentOptions(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.URL.Path != "/v1/payment-intents" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"data":{"payment_intent":{"payment_intent_id":"pi_123","status":"requires_confirmation"}}}`)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{
		"payment-intents", "create", "--amount", "2500", "--currency", "USD",
		"--payment-option", "card", "--payment-option", "ach_debit", "--transaction-purpose", "services",
		"--output", "json",
	})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	options, ok := body["payment_options"].([]any)
	if !ok || !slices.Equal(options, []any{"card", "ach_debit"}) {
		t.Fatalf("payment_options=%#v", body["payment_options"])
	}
	if body["transaction_purpose"] != "services" {
		t.Fatalf("transaction_purpose=%#v", body["transaction_purpose"])
	}
}

func TestPaymentIntentCreateRequiresOptionsOnlyForStandaloneRequests(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	exit := app.Run([]string{"payment-intents", "create", "--amount", "2500", "--currency", "USD", "--dry-run=client", "--output", "json"})
	if exit != ExitUsage || !strings.Contains(stdout.String(), `"param":"payment_option"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}

	stdout.Reset()
	stderr.Reset()
	exit = app.Run([]string{"payment-intents", "create", "--order", "ord_123", "--dry-run=client", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"path":"/v1/orders/ord_123/payment-intents"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestPaymentIntentConfirmSendsConfirmationToken(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.URL.Path != "/v1/payment-intents/pi_123/confirm" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"data":{"payment_intent_id":"pi_123","status":"processing"}}`)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"payment-intents", "confirm", "pi_123", "--confirmation-token", "ctoken_123", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 || body["confirmation_token"] != "ctoken_123" {
		t.Fatalf("exit=%d body=%#v stdout=%s stderr=%s", exit, body, stdout, stderr)
	}
}

func TestPaymentIntentCancelSendsEmptyJSONObject(t *testing.T) {
	var rawBody string
	var contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.URL.Path != "/v1/payment-intents/pi_123/cancel" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		contentType = r.Header.Get("Content-Type")
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		rawBody = string(raw)
		fmt.Fprint(w, `{"data":{"payment_intent_id":"pi_123","status":"canceled"}}`)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"payment-intents", "cancel", "pi_123", "--confirm", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 || rawBody != "{}" || contentType != "application/json" {
		t.Fatalf("exit=%d body=%q content-type=%q stdout=%s stderr=%s", exit, rawBody, contentType, stdout, stderr)
	}
}

func TestLiveJSONMutationFailsClosedWithoutConfirm(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("live"))
			return
		}
		mutations++
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_test")
	exit := app.Run([]string{"payment-intents", "create", "--amount", "2500", "--currency", "USD", "--payment-option", "card", "--live", "--output", "json"})
	if exit != ExitConfirmation {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if mutations != 0 {
		t.Fatalf("mutation was sent")
	}
	var envelope map[string]any
	if json.Unmarshal(stdout.Bytes(), &envelope) != nil {
		t.Fatalf("stdout is not JSON: %s", stdout)
	}
	if !strings.Contains(stdout.String(), "confirmation_required") {
		t.Fatalf("missing structured confirmation error: %s", stdout)
	}
	if !strings.Contains(stdout.String(), `"code":"PRODUCTION_CONFIRMATION_REQUIRED"`) {
		t.Fatalf("missing production-specific confirmation code: %s", stdout)
	}
}

func TestSandboxDestructiveMutationUsesGenericConfirmationCode(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		mutations++
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"webhook-endpoints", "delete", "whep_123", "--output", "json"})
	if exit != ExitConfirmation || mutations != 0 {
		t.Fatalf("exit=%d mutations=%d stdout=%s stderr=%s", exit, mutations, stdout, stderr)
	}
	if !strings.Contains(stdout.String(), `"code":"CONFIRMATION_REQUIRED"`) || strings.Contains(stdout.String(), "PRODUCTION_CONFIRMATION_REQUIRED") {
		t.Fatalf("sandbox confirmation code is misleading: %s", stdout)
	}
}

func TestClientDryRunPerformsNoNetworkRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"payment-intents", "create", "--amount", "2500", "--currency", "USD", "--payment-option", "card", "--dry-run=client", "--output", "json"})
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if requests != 0 {
		t.Fatalf("client dry-run made %d network requests", requests)
	}
	if !strings.Contains(stdout.String(), `"persistent_side_effects":false`) || !strings.Contains(stdout.String(), `"Idempotency-Key":"flint-cli-`) {
		t.Fatalf("unexpected dry-run output: %s", stdout)
	}
}

func TestFeedbackCreateUsesLiveCredentialWithoutLiveAcknowledgementAfterConsent(t *testing.T) {
	resourceCalls := 0
	var submitted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("live"))
			return
		}
		resourceCalls++
		if r.URL.Path != "/v1/feedback-reports" || r.Method != http.MethodPost {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"data":{"feedback_report_id":"fbr_01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"bug","surface":"cli","summary":"Specific feedback","created_at":"2026-08-21T12:00:00Z"}}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_test")
	if err := app.setAgentFeedbackSubmission(defaultOptions(), "enabled"); err != nil {
		t.Fatal(err)
	}
	exit := app.Run([]string{"feedback", "report", "--kind", "bug", "--surface", "cli", "--summary", "Specific feedback", "--output", "json"})
	if exit != ExitOK || resourceCalls != 1 {
		t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, resourceCalls, stdout, stderr)
	}
	if submitted["reporter_kind"] != "ai_agent" {
		t.Fatalf("submitted reporter_kind=%#v", submitted["reporter_kind"])
	}
	client, _ := submitted["reporting_client"].(map[string]any)
	if client["name"] != "flint-cli" || client["platform"] == "" {
		t.Fatalf("reporting_client=%#v", client)
	}
	history, err := app.loadHistory()
	if err != nil || len(history.Entries) == 0 || history.Entries[0].ID != "fbr_01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatalf("history=%#v error=%v", history, err)
	}
}

func TestFeedbackConsentAndSecretChecksFailBeforeTransport(t *testing.T) {
	resourceCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		resourceCalls++
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	args := []string{"feedback", "report", "--kind", "bug", "--surface", "cli", "--summary", "Specific feedback", "--output", "json"}
	if exit := app.Run(args); exit != ExitAuth || !strings.Contains(stdout.String(), "AGENT_FEEDBACK_DISABLED") || resourceCalls != 0 {
		t.Fatalf("disabled exit=%d calls=%d stdout=%s stderr=%s", exit, resourceCalls, stdout, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	app.Stdin = strings.NewReader(`{"kind":"bug","surface":"cli","summary":"Specific feedback"}`)
	rawArgs := []string{"api", "post", "/v1/feedback-reports", "--input", "-", "--no-input", "--output", "json"}
	if exit := app.Run(rawArgs); exit != ExitAuth || !strings.Contains(stdout.String(), "AGENT_FEEDBACK_DISABLED") || resourceCalls != 0 {
		t.Fatalf("raw disabled exit=%d calls=%d stdout=%s stderr=%s", exit, resourceCalls, stdout, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if err := app.setAgentFeedbackSubmission(defaultOptions(), "enabled"); err != nil {
		t.Fatal(err)
	}
	args = append(args[:len(args)-2], "--description", "Authorization: Bearer abcdefghijklmnop", "--output", "json")
	if exit := app.Run(args); exit != ExitUsage || !strings.Contains(stdout.String(), "UNSAFE_FEEDBACK_CONTENT") || resourceCalls != 0 {
		t.Fatalf("secret exit=%d calls=%d stdout=%s stderr=%s", exit, resourceCalls, stdout, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	unsafeKeyArgs := []string{"feedback", "report", "--kind", "bug", "--surface", "cli", "--summary", "Specific feedback", "--idempotency-key", "flint_live_abcdefghijklmnop", "--output", "json"}
	if exit := app.Run(unsafeKeyArgs); exit != ExitUsage || !strings.Contains(stdout.String(), "INVALID_IDEMPOTENCY_KEY") || resourceCalls != 0 {
		t.Fatalf("unsafe idempotency key exit=%d calls=%d stdout=%s stderr=%s", exit, resourceCalls, stdout, stderr)
	}
	stdout.Reset()
	stderr.Reset()
	app.Stdin = strings.NewReader(`{"kind":"bug","surface":"cli","summary":"Specific feedback","description":"Authorization: Bearer abcdefghijklmnop"}`)
	if exit := app.Run(rawArgs); exit != ExitUsage || !strings.Contains(stdout.String(), "UNSAFE_FEEDBACK_CONTENT") || resourceCalls != 0 {
		t.Fatalf("raw secret exit=%d calls=%d stdout=%s stderr=%s", exit, resourceCalls, stdout, stderr)
	}
}

func TestFeedbackInputReporterKindRequiresConsentAndSecretChecks(t *testing.T) {
	resourceCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		resourceCalls++
	}))
	defer server.Close()

	app, _, stderr := testApp(t, server.URL)
	app.IsTTY = func() bool { return true }
	input := `{"kind":"bug","surface":"cli","summary":"Specific feedback","reporter_kind":"ai_agent","description":"Authorization: Bearer abcdefghijklmnop"}`
	app.Stdin = strings.NewReader(input)
	if exit := app.Run([]string{"feedback", "report", "--input", "-"}); exit != ExitAuth || !strings.Contains(stderr.String(), "Agent feedback submission is disabled") || resourceCalls != 0 {
		t.Fatalf("disabled exit=%d calls=%d stderr=%s", exit, resourceCalls, stderr)
	}

	stderr.Reset()
	if err := app.setAgentFeedbackSubmission(defaultOptions(), "enabled"); err != nil {
		t.Fatal(err)
	}
	app.Stdin = strings.NewReader(input)
	if exit := app.Run([]string{"feedback", "report", "--input", "-"}); exit != ExitUsage || !strings.Contains(stderr.String(), "Remove the recognized secret from description") || resourceCalls != 0 {
		t.Fatalf("secret exit=%d calls=%d stderr=%s", exit, resourceCalls, stderr)
	}
}

func TestFeedbackConfigureRejectsNonInteractiveEnable(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	exit := app.Run([]string{"feedback", "configure", "enabled", "--no-input", "--output", "json"})
	if exit != ExitAuth || !strings.Contains(stdout.String(), "INTERACTIVE_CONSENT_REQUIRED") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestHistoryReferenceResolvesBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.URL.Path != "/v1/orders/ord_123" {
			t.Errorf("resolved path=%s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":{"order":{"order_id":"ord_123","status":"open"}}}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	if err := app.saveHistory(History{Entries: []HistoryEntry{{
		ID: "ord_123", ResourceType: "order", Command: "orders.create",
		Profile: "default", Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_123", CreatedAt: time.Now(),
	}}}); err != nil {
		t.Fatal(err)
	}
	exit := app.Run([]string{"orders", "get", "@last", "--output", "json"})
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestWaitBoundPrintsLastResourceAndExitSix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":{"payment_intent":{"payment_intent_id":"pi_123","status":"processing"}}}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"payment-intents", "get", "pi_123", "--wait-for", "status=succeeded", "--for", "250ms", "--output", "json"})
	if exit != ExitWait {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	var result map[string]any
	if json.Unmarshal(stdout.Bytes(), &result) != nil {
		t.Fatalf("last resource is not JSON: %s", stdout)
	}
	if !strings.Contains(stderr.String(), "wait bound") {
		t.Fatalf("missing wait diagnostic: %s", stderr)
	}
}

func TestWaitBoundCapsTheFirstResourceRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	started := time.Now()
	exit := app.Run([]string{"payment-intents", "get", "pi_123", "--wait-for", "status=succeeded", "--for", "25ms", "--timeout", "1s", "--output", "json"})
	if exit != ExitWait {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("first wait request exceeded the 25ms wait bound: %s", elapsed)
	}
}

func TestUnknownCommandReturnsCanonicalSuggestions(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	exit := app.Run([]string{"payments", "list", "--output", "json"})
	if exit != ExitUsage {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout.String(), `"suggestions":["payment-intents","payment"]`) {
		t.Fatalf("suggestions=%s", stdout)
	}
}

func TestMCPToolsMatchCanonicalRegistryAndInputSchemas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["developer.feedback_reports.read"]}}`)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}, defaultOptions())
	result := response["result"].(map[string]any)
	tools := result["tools"].([]map[string]any)
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		byName[tool["name"].(string)] = tool
		inputSchema, ok := tool["inputSchema"].(map[string]any)
		if !ok || inputSchema["type"] != "object" {
			t.Errorf("MCP tool %s must advertise inputSchema.type object, got %#v", tool["name"], inputSchema["type"])
		}
	}
	want := 0
	for _, cmd := range app.Registry.Commands {
		if !mcpCommandExposed(cmd) {
			continue
		}
		want++
		tool, ok := byName[cmd.CanonicalName]
		if !ok {
			t.Errorf("canonical command %s is missing from MCP", cmd.CanonicalName)
			continue
		}
		schema, schemaErr := schemaForMCPCommand(cmd)
		if schemaErr != nil {
			t.Fatal(schemaErr)
		}
		if fmt.Sprint(tool["inputSchema"]) != fmt.Sprint(schema) {
			t.Errorf("MCP schema differs for %s", cmd.CanonicalName)
		}
		outputSchema, outputSchemaErr := mcpOutputSchema(cmd)
		if outputSchemaErr != nil {
			t.Fatal(outputSchemaErr)
		}
		if fmt.Sprint(tool["outputSchema"]) != fmt.Sprint(outputSchema) {
			t.Errorf("MCP output schema differs for %s", cmd.CanonicalName)
		}
		annotations, ok := tool["annotations"].(map[string]any)
		if !ok || fmt.Sprint(annotations) != fmt.Sprint(mcpToolAnnotations(cmd)) {
			t.Errorf("MCP annotations differ for %s: %#v", cmd.CanonicalName, tool["annotations"])
		}
	}
	if len(tools) != want {
		t.Fatalf("MCP tools=%d, want %d", len(tools), want)
	}
	if _, exists := byName["payment.create"]; exists {
		t.Fatal("MCP exposed an alias as a separate tool")
	}
}

func TestMCPCallUsesJSONEnvelope(t *testing.T) {
	app, _, _ := testApp(t, "")
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": "version", "arguments": map[string]any{}}}, defaultOptions())
	result := response["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("MCP call failed: %#v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if _, ok := structured["data"]; !ok {
		t.Fatalf("MCP result is not a Flint JSON envelope: %#v", structured)
	}
}

func TestMCPCallRejectsArgumentsOutsideAdvertisedSchema(t *testing.T) {
	app, _, _ := testApp(t, "")
	secretPath := t.TempDir() + "/secret.json"
	const marker = "mcp-must-not-read-this-file"
	if err := os.WriteFile(secretPath, []byte(`{"secret":"`+marker+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []map[string]any{
		{"_flint": map[string]any{"input": secretPath}},
		{"_flint": map[string]any{"debug": "true"}},
		{"unexpected": "value"},
	}
	for _, arguments := range tests {
		response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
			"name":      "version",
			"arguments": arguments,
		}}, defaultOptions())
		result := response["result"].(map[string]any)
		if result["isError"] != true {
			t.Fatalf("MCP accepted out-of-schema arguments %#v: %#v", arguments, result)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("MCP exposed local file contents for arguments %#v", arguments)
		}
		if !strings.Contains(string(encoded), "advertised input schema") {
			t.Fatalf("MCP returned an unclear schema error for %#v: %s", arguments, encoded)
		}
	}
}

func TestMCPOrderLinkedPaymentIntentUsesTypedAlternateInput(t *testing.T) {
	app, _, _ := testApp(t, "")
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
		"name": "payment-intents.create",
		"arguments": map[string]any{
			"order":  "ord_123",
			"_flint": map[string]any{"dry_run": "client"},
		},
	}}, defaultOptions())
	result := response["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("MCP call failed: %#v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	path, ok := lookupPath(structured, "data.path")
	if !ok || path != "/v1/orders/ord_123/payment-intents" {
		t.Fatalf("MCP alternate request path = %#v", structured)
	}
}

func TestOutputProjectionPreservesListEnvelopeAndFieldIsRaw(t *testing.T) {
	value := map[string]any{"data": []any{map[string]any{"payment_intent_id": "pi_123", "status": "succeeded", "amount": 2500.0}}, "next_page_token": "page_2", "request_id": "req_1"}
	projected, e := applyOutputTransforms(value, Options{Select: []string{"payment_intent_id", "status"}})
	if e != nil {
		t.Fatal(e)
	}
	envelope := projected.(map[string]any)
	if envelope["next_page_token"] != "page_2" || envelope["request_id"] != "req_1" {
		t.Fatalf("projection discarded envelope metadata: %#v", envelope)
	}
	app, stdout, _ := testApp(t, "")
	if outputErr := app.writeResult(map[string]any{"data": map[string]any{"url": "https://example.com/checkout"}}, nil, Options{Field: "data.url"}); outputErr != nil {
		t.Fatal(outputErr)
	}
	if stdout.String() != "https://example.com/checkout\n" {
		t.Fatalf("field output=%q", stdout.String())
	}
}

func TestCheckoutHumanOutputPrintsURLOnce(t *testing.T) {
	var output bytes.Buffer
	renderHuman(&output, map[string]any{"data": map[string]any{
		"checkout_session_id": "cs_123",
		"url":                 "https://checkout.withflintpay.com/cs_123",
	}}, &Command{Render: "checkout"}, time.Now())
	if got := strings.Count(output.String(), "https://checkout.withflintpay.com/cs_123"); got != 1 {
		t.Fatalf("checkout URL appeared %d times: %s", got, output.String())
	}
	if !strings.Contains(output.String(), "Checkout session ID") {
		t.Fatalf("checkout metadata was omitted: %s", output.String())
	}
}

func TestPreviewReportsActualEmbeddedPermissionCheck(t *testing.T) {
	cmd := &Command{OperationID: "createCustomer"}
	failed := permissionPreview(cmd, AuthContext{Scopes: []string{"customers.read"}})
	missing, _ := failed["missing_scopes"].([]string)
	if failed["status"] != "fail" || !slices.Equal(missing, []string{"customers.write"}) {
		t.Fatalf("failed permission preview = %#v", failed)
	}
	passed := permissionPreview(cmd, AuthContext{Scopes: []string{"customers.write"}})
	missing, _ = passed["missing_scopes"].([]string)
	if passed["status"] != "pass" || len(missing) != 0 {
		t.Fatalf("passed permission preview = %#v", passed)
	}
	unknown := permissionPreview(&Command{CanonicalName: "api"}, AuthContext{})
	if unknown["status"] != "unknown" {
		t.Fatalf("raw API permission preview = %#v", unknown)
	}
}

func TestListenForwardsExactPayloadWithCanonicalSignatureAndCheckpoint(t *testing.T) {
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const payload = `{ "payment_intent_id" : "pi_123", "status" : "succeeded" }`
	var forwardedBody []byte
	var forwardedHeaders http.Header
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedBody, _ = io.ReadAll(r.Body)
		forwardedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()

	var streamVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.URL.Path != "/v1/webhook-events/stream" {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		streamVersion = r.Header.Get("X-Flint-CLI-Version")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
		fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.succeeded\",\"payload\":%s}\n\n", eventID, eventID, payload)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	instant := time.Unix(1700000000, 0)
	app.Now = func() time.Time { return instant }
	app.Info.Version = "1.2.3"
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "1", "--output", "ndjson"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if string(forwardedBody) != payload {
		t.Fatalf("forwarded body = %q, want exact %q", forwardedBody, payload)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("listener output lines = %d: %q", len(lines), lines)
	}
	var started map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &started); err != nil {
		t.Fatal(err)
	}
	secret, _ := started["signing_secret"].(string)
	if _, err := webhooksigning.StandardWebhookSigningKey(secret); err != nil {
		t.Fatalf("signing secret = %q: %v", secret, err)
	}
	wantSignature := webhooksigning.StandardWebhookSignatureHeader(eventID, instant.Unix(), []byte(payload), secret)
	if got := forwardedHeaders.Get("webhook-signature"); got != wantSignature {
		t.Fatalf("webhook-signature = %q, want %q", got, wantSignature)
	}
	if forwardedHeaders.Get("webhook-id") != eventID || forwardedHeaders.Get("webhook-timestamp") != "1700000000" {
		t.Fatalf("forward headers = %#v", forwardedHeaders)
	}
	if streamVersion != "1.2.3" {
		t.Fatalf("stream CLI version = %q", streamVersion)
	}
	if !strings.Contains(lines[2], `"status_code":204`) || !strings.Contains(lines[3], `"cursor":"`+eventID+`"`) {
		t.Fatalf("unexpected forward/checkpoint records: %q", lines)
	}
}

func TestDurationBoundedEmptyListenExitsWithCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"whev_01ABCDEFGHIJKLMNOPQRSTUVWX\"}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--for", "25ms", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"type":"checkpoint"`) {
		t.Fatalf("unexpected empty stream output: %q", lines)
	}
}

func TestLocalDestructiveCommandsFailClosedWithoutConfirm(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	exit := app.Run([]string{"history", "--clear", "--no-input", "--output", "json"})
	if exit != ExitConfirmation || !strings.Contains(stdout.String(), `"code":"CONFIRMATION_REQUIRED"`) {
		t.Fatalf("exit=%d output=%s", exit, stdout)
	}
}

func TestHistoryHumanOutputRendersEntriesAsFields(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	app.Now = func() time.Time { return time.Date(2026, time.July, 21, 15, 0, 0, 0, time.UTC) }
	if err := app.saveHistory(History{Entries: []HistoryEntry{{
		ID: "pi_123", ResourceType: "payment_intent", Command: "payment-intents.create",
		Profile: "default", Environment: "sandbox", CreatedAt: app.Now().Add(-time.Minute),
	}}}); err != nil {
		t.Fatal(err)
	}

	exit := app.Run([]string{"history", "--output", "human"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	output := stdout.String()
	for _, want := range []string{"ID", "pi_123", "Resource type", "payment_intent", "Command", "payment-intents.create"} {
		if !strings.Contains(output, want) {
			t.Fatalf("history output is missing %q: %s", want, output)
		}
	}
	if strings.Contains(output, "[{pi_123") {
		t.Fatalf("history output contains raw Go structs: %s", output)
	}
}

func TestDoctorComparesObservedAPIVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/openapi.json" {
			fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
			return
		}
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"doctor", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"expected":"2026-02-01"`) || !strings.Contains(stdout.String(), `"observed":"2026-02-01"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestDoctorRejectsCredentialWithNoUsableScopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_123","environment":"sandbox","merchant_id":"mer_123","sandbox_id":"test_123","scopes":[]},"request_id":"req_auth","meta":{"api_version":"2026-02-01"}}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"doctor", "--output", "json"})
	if exit != ExitAuth || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"name":"scopes"`) || !strings.Contains(stdout.String(), `"status":"fail"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestInitPrintsGoldenPathsInCanonicalOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/openapi.json" {
			fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
			return
		}
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"init", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	commerce := strings.Index(stdout.String(), `"workflow":"commerce_order"`)
	hosted := strings.Index(stdout.String(), `"workflow":"hosted_checkout"`)
	standalone := strings.Index(stdout.String(), `"workflow":"standalone_payment"`)
	if commerce < 0 || hosted <= commerce || standalone <= hosted {
		t.Fatalf("golden paths are out of order: %s", stdout)
	}
	if !strings.Contains(stdout.String(), `flint payment create --amount 2500 --currency USD --payment-option card`) {
		t.Fatalf("standalone golden path is not runnable: %s", stdout)
	}
}

func TestMissingCredentialReturnsBothFirstRunPaths(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	t.Setenv("FLINT_API_KEY", "")
	exit := app.Run([]string{"orders", "get", "ord_123", "--output", "json"})
	if exit != ExitAuth || !strings.Contains(stdout.String(), "flint auth import") || !strings.Contains(stdout.String(), "flint signup") || !strings.Contains(stdout.String(), dashboardAPIKeysURL) {
		t.Fatalf("exit=%d output=%s", exit, stdout)
	}
}

func TestSignupIssuesScopedSandboxKeyAndStoresOnlyInCredentialStore(t *testing.T) {
	requests := make([]string, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/onboarding/start":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["email"] != "dev@example.com" || body["first_name"] != "Ada" || body["last_name"] != "Lovelace" {
				t.Errorf("unexpected start body: %#v", body)
			}
			fmt.Fprint(w, `{"data":{"verification_token":"devver_123"}}`)
		case "/v1/onboarding/verify-email":
			fmt.Fprint(w, `{"data":{"onboarding_session_token":"devsess_123"}}`)
		case "/v1/onboarding/state":
			if r.Header.Get("Authorization") != "Bearer devsess_123" {
				t.Errorf("state authorization = %q", r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_123","next_step":{"code":"complete_verification_step"}}}`)
		case "/v1/onboarding/api-key":
			var body struct {
				Name      string   `json:"name"`
				SandboxID string   `json:"sandbox_id"`
				Scopes    []string `json:"scopes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Name != "flint-cli" || body.SandboxID != "test_123" || !slices.Equal(body.Scopes, initialCLIScopes) {
				t.Errorf("unexpected initial key request: %#v", body)
			}
			fmt.Fprint(w, `{"data":{"api_key_id":"key_123","merchant_id":"mer_123","sandbox_id":"test_123","secret_key":"flint_test_secret"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	storedProfile, storedSecret := "", ""
	app.StoreCredential = func(profile, secret string) error {
		storedProfile, storedSecret = profile, secret
		return nil
	}
	app.DeleteCredential = func(string) error { return nil }
	exit := app.Run([]string{
		"signup", "--email", "dev@example.com", "--first-name", "Ada", "--last-name", "Lovelace",
		"--verification-code", "482193", "--no-input", "--output", "json",
	})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	wantRequests := []string{
		"POST /v1/onboarding/start",
		"POST /v1/onboarding/verify-email",
		"GET /v1/onboarding/state",
		"POST /v1/onboarding/api-key",
	}
	if !slices.Equal(requests, wantRequests) {
		t.Fatalf("requests=%v, want %v", requests, wantRequests)
	}
	if storedProfile != "default" || storedSecret != "flint_test_secret" {
		t.Fatalf("stored profile=%q secret=%q", storedProfile, storedSecret)
	}
	if strings.Contains(stdout.String(), "flint_test_secret") || !strings.Contains(stdout.String(), `"credential_saved":true`) {
		t.Fatalf("signup output leaked or omitted credential state: %s", stdout)
	}
	cfg, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.Profiles["default"]
	if profile.Environment != "sandbox" || profile.APIKeyID != "key_123" || profile.MerchantID != "mer_123" || profile.SandboxID != "test_123" {
		t.Fatalf("stored profile metadata = %#v", profile)
	}
}

func testApp(t *testing.T, baseURL string) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("FLINT_API_KEY", "flint_test_test")
	t.Setenv("FLINT_BASE_URL", baseURL)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(BuildInfo{Version: "test", APIVersion: "2026-02-01", SchemaHash: "test"})
	app.Stdout = stdout
	app.Stderr = stderr
	app.Stdin = strings.NewReader("")
	app.IsTTY = func() bool { return false }
	app.ConfigDir = t.TempDir()
	app.WorkingDir = t.TempDir()
	app.LoadCredential = func(string) (string, error) { return "", nil }
	app.StoreCredential = func(string, string) error { return nil }
	app.DeleteCredential = func(string) error { return nil }
	return app, stdout, stderr
}
func authContextJSON(environment string) string {
	return fmt.Sprintf(`{"data":{"auth_type":"api_key","api_key_id":"key_123","environment":%q,"merchant_id":"mer_123","sandbox_id":"test_123","scopes":["payments.payment_intents.read","payments.payment_intents.write"]},"request_id":"req_auth","meta":{"api_version":"2026-02-01"}}`, environment)
}

func TestPayOrderActionInputAndTokenShortcut(t *testing.T) {
	command, _ := NewRegistry().ByName("orders.pay")
	for _, body := range []string{
		`{"action":"pay"}`,
		`{"action":"pay","payment_source":{"confirmation_token":"ctoken_test"}}`,
		`{"action":"confirm_payment_intents","payment_intents":[{"payment_intent_id":"pi_test"}],"completion_behavior":"partial_payment"}`,
		`{"action":"setup","setup_payment_source":{"token":"src_test"}}`,
		`{"action":"resume","payment_attempt_id":"opat_test"}`,
	} {
		t.Run(body, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			app.Stdin = strings.NewReader(body)
			_, opts, _, err := parseInvocation(NewRegistry(), []string{"orders", "pay", "ord_test", "--input", "-", "--dry-run", "client"})
			if err != nil {
				t.Fatal(err)
			}
			req, err := app.prepareRequest(t.Context(), command, opts, ResolvedConfig{}, "", "")
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(body), &want); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(req.BodyValue) != fmt.Sprint(want) {
				t.Fatalf("body = %s, want %s", req.Body, body)
			}
		})
	}
	for _, tc := range []struct {
		body     string
		conflict bool
	}{
		{`{}`, false},
		{`{"action":"confirm_payment_intents"}`, false},
		{`{"action":"pay"}`, true},
		{`{"payment_intents":[]}`, true},
		{`{"setup_payment_source":{"token":"src_test"}}`, true},
		{`{"payment_attempt_id":"opat_test"}`, true},
	} {
		t.Run("shortcut/"+tc.body, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			app.Stdin = strings.NewReader(tc.body)
			_, opts, _, err := parseInvocation(NewRegistry(), []string{"orders", "pay", "ord_test", "--payment-source-token", "pi_test=pm_card_visa", "--input", "-", "--dry-run", "client"})
			if err != nil {
				t.Fatal(err)
			}
			req, err := app.prepareRequest(t.Context(), command, opts, ResolvedConfig{}, "", "")
			if tc.conflict {
				if err == nil || err.Code != "CONFLICTING_ARGUMENTS" {
					t.Fatalf("error = %#v", err)
				}
			} else if err != nil || req.BodyValue["action"] != "confirm_payment_intents" {
				t.Fatalf("body=%#v error=%v", req.BodyValue, err)
			}
		})
	}
	schema, err := schemaForCommand(command, true)
	if err != nil {
		t.Fatal(err)
	}
	branches, _ := schema["oneOf"].([]any)
	if len(branches) != 4 {
		t.Fatalf("expected four action branches: %#v", schema)
	}
}

func TestPayOrderMCPActionBranchesAcceptCLIArguments(t *testing.T) {
	command, _ := NewRegistry().ByName("orders.pay")
	for _, body := range []string{
		`{"action":"pay"}`,
		`{"action":"confirm_payment_intents","payment_intents":[{"payment_intent_id":"pi_test"}]}`,
		`{"action":"setup","setup_payment_source":{"token":"src_test"}}`,
		`{"action":"resume","payment_attempt_id":"opat_test"}`,
	} {
		var arguments map[string]any
		if err := json.Unmarshal([]byte(body), &arguments); err != nil {
			t.Fatal(err)
		}
		arguments["order_id"] = "ord_test"
		arguments["_flint"] = map[string]any{"dry_run": "client"}
		if err := validateMCPArguments(command, arguments); err != nil {
			t.Fatalf("valid %s: %v", body, err)
		}
		arguments["unknown"] = true
		if err := validateMCPArguments(command, arguments); err == nil {
			t.Fatalf("accepted unknown field in %s", body)
		}
		delete(arguments, "unknown")
		if arguments["action"] != "confirm_payment_intents" {
			arguments["completion_behavior"] = "partial_payment"
			if err := validateMCPArguments(command, arguments); err == nil {
				t.Fatalf("accepted cross-action field in %s", body)
			}
		}
	}
}

func TestDoctorDiscoversNewerVersionWhileAuthResponseRemainsPinned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Flint-Version") != apispec.APIVersion() {
			t.Errorf("request version=%q", r.Header.Get("Flint-Version"))
		}
		if r.URL.Path == "/v1/openapi.json" {
			if r.Header.Get("Authorization") != "" {
				t.Error("public discovery received credentials")
			}
			fmt.Fprintf(w, `{"x-flint-api-version":%q,"x-flint-api-releases":{"current_version":"2027-02-01"}}`, apispec.APIVersion())
			return
		}
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"doctor", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"observed":"2027-02-01"`) || !strings.Contains(stdout.String(), `"status":"info"`) || !strings.Contains(stdout.String(), "https://developers.withflintpay.com/changelog") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestDoctorRejectsMissingReleaseDiscovery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, server.URL)
	if exit := app.Run([]string{"doctor", "--output", "json"}); exit != ExitAPI || !strings.Contains(stdout.String(), "OpenAPI release catalog") {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
}

func TestCheckoutHumanOutputUsesHostedLaunchURL(t *testing.T) {
	var output bytes.Buffer
	renderHuman(&output, map[string]any{"data": map[string]any{
		"checkout_session": map[string]any{"checkout_session_id": "cs_test", "url": "https://checkout.example.com/session"},
		"hosted_checkout":  map[string]any{"url": "https://checkout.example.com/launch"},
	}}, &Command{Render: "checkout"}, time.Now())
	if !strings.HasPrefix(output.String(), "https://checkout.example.com/launch\n") || strings.Count(output.String(), "https://checkout.example.com/launch") != 1 {
		t.Fatalf("hosted launch URL missing or repeated: %s", output.String())
	}
}
