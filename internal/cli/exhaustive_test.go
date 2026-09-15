package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// These tests are registry-driven on purpose. Adding a command, argument, or
// parser flag without giving it an executable test case must break the suite.

func TestEveryRegisteredCommandAcceptsEveryDeclaredArgument(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		t.Run(command.CanonicalName, func(t *testing.T) {
			argv := append([]string(nil), command.Path[1:]...)
			wantPositionals := make([]string, 0)
			wantFlags := map[string][]string{}
			for _, argument := range command.Arguments {
				values := exhaustiveArgumentValues(argument)
				if argument.Positional > 0 {
					wantPositionals = append(wantPositionals, values[0])
					argv = append(argv, values[0])
					continue
				}
				flagName := strings.TrimPrefix(argument.Flag, "--")
				wantFlags[flagName] = values
				for _, value := range values {
					if argument.Type == "boolean" {
						argv = append(argv, argument.Flag+"="+value)
					} else {
						argv = append(argv, argument.Flag, value)
					}
				}
			}

			parsed, options, help, cliErr := parseInvocation(NewRegistry(), argv)
			if cliErr != nil {
				t.Fatalf("parseInvocation(%v): %v", argv, cliErr)
			}
			if help {
				t.Fatalf("parseInvocation(%v) unexpectedly requested help", argv)
			}
			if parsed.CanonicalName != command.CanonicalName {
				t.Fatalf("parsed command = %s, want %s", parsed.CanonicalName, command.CanonicalName)
			}
			if !slices.Equal(options.Positionals, wantPositionals) {
				t.Fatalf("positionals = %#v, want %#v", options.Positionals, wantPositionals)
			}
			for name, values := range wantFlags {
				if !slices.Equal(options.Raw[name], values) {
					t.Errorf("--%s values = %#v, want %#v", name, options.Raw[name], values)
				}
			}
		})
	}
}

func TestEveryResourceIDArgumentDeclaresHistoryReference(t *testing.T) {
	resourcePrefixesByName := map[string]string{
		"api_key_id":                         "key_",
		"api_request_log_id":                 "rlog_",
		"checkout_session_id":                "cs_",
		"delivery_location_set_id":           "dls_",
		"delivery_location_set_revision_id":  "dlsr_",
		"delivery_method_id":                 "dmet_",
		"delivery_method_revision_id":        "dmetr_",
		"delivery_profile_id":                "dprof_",
		"delivery_profile_revision_id":       "dprofr_",
		"delivery_quote_id":                  "dqt_",
		"delivery_rate_callback_id":          "dcb_",
		"delivery_rate_callback_revision_id": "dcbr_",
		"delivery_rate_id":                   "drate_",
		"delivery_revocation_id":             "drev_",
		"delivery_selection_id":              "dsel_",
		"delivery_zone_id":                   "dzone_",
		"delivery_zone_revision_id":          "dzoner_",
		"expected_delivery_selection_id":     "dsel_",
		"fulfillment_event_id":               "fev_",
		"fulfillment_id":                     "ful_",
		"fulfillment_notification_id":        "fnt_",
		"cursor":                             "whev_",
		"customer":                           "cus_",
		"customer_id":                        "cus_",
		"invoice_id":                         "inv_",
		"merchant_id":                        "mer_",
		"location_id":                        "loc_",
		"order":                              "ord_",
		"order_id":                           "ord_",
		"organization_id":                    "org_",
		"payment_intent":                     "pi_",
		"payment_intent_id":                  "pi_",
		"payment_link_id":                    "pl_",
		"payment_method":                     "pm_",
		"refund_id":                          "ref_",
		"package_id":                         "pkg_",
		"package_item_id":                    "pki_",
		"return_id":                          "ret_",
		"sandbox_id":                         "test_",
		"webhook_delivery_id":                "wdel_",
		"webhook_endpoint_id":                "whep_",
		"webhook_event_id":                   "whev_",
		"shipment_id":                        "shp_",
	}

	for _, command := range NewRegistry().Commands {
		for _, argument := range command.Arguments {
			wantPrefix, isResourceID := resourcePrefixesByName[argument.Name]
			if argument.Name == "resource_id" {
				isResourceID = true
			}
			resourceShaped := strings.HasSuffix(argument.Name, "_id") ||
				strings.HasSuffix(argument.BodyPath, "_id") ||
				strings.HasSuffix(argument.Query, "_id")
			if resourceShaped && argument.Name != "request_id" && argument.Name != "related_request_id" &&
				argument.Name != "external_event_id" &&
				argument.Name != "external_reference_id" && !isResourceID {
				if isPublicResourceID(argument.Name) && !argument.AcceptsHistoryRef {
					t.Errorf("%s %s does not accept resource history references", command.CanonicalName, argument.Name)
				}
				continue
			}
			if !isResourceID {
				continue
			}
			if !argument.AcceptsHistoryRef {
				t.Errorf("%s %s does not declare accepts_history_ref", command.CanonicalName, argument.Name)
			}
			if argument.IDPrefix != wantPrefix {
				t.Errorf("%s %s id_prefix = %q, want %q", command.CanonicalName, argument.Name, argument.IDPrefix, wantPrefix)
			}
		}
	}
}

func TestEveryResourcePrefixHasAHistoryQualifier(t *testing.T) {
	for prefix := range resourcePrefixes {
		if qualifier := historyQualifierForPrefix(prefix); qualifier == "" {
			t.Errorf("resource prefix %q has no @last qualifier", prefix)
		}
	}
}

func TestEveryHistoryReferenceArgumentResolvesAndMissingFails(t *testing.T) {
	resolved := ResolvedConfig{ProfileName: "default", Environment: "sandbox"}
	for _, command := range NewRegistry().Commands {
		for _, argument := range command.Arguments {
			if !argument.AcceptsHistoryRef {
				continue
			}
			qualifier := historyQualifierForPrefix(argument.IDPrefix)
			if argument.IDPrefix != "" && qualifier == "" {
				t.Errorf("%s %s prefix %q has no history qualifier", command.CanonicalName, argument.Name, argument.IDPrefix)
				continue
			}
			references := []string{"@last"}
			historyID := argument.IDPrefix + "history"
			if argument.IDPrefix == "" {
				qualifier = "pi"
				historyID = "pi_history"
				references = nil
			}
			references = append(references, "@last."+qualifier)

			for _, reference := range references {
				name := command.CanonicalName + "/" + argument.Name + "/" + strings.TrimPrefix(reference, "@last")
				t.Run(name, func(t *testing.T) {
					app, _, _ := testApp(t, "")
					if err := app.saveHistory(History{Entries: []HistoryEntry{{
						ID:           historyID,
						ResourceType: resourceTypeForPrefix(argument.IDPrefix),
						Command:      "test",
						Profile:      resolved.ProfileName,
						Environment:  resolved.Environment,
						CreatedAt:    time.Now(),
					}}}); err != nil {
						t.Fatal(err)
					}

					options := historyReferenceOptions(t, command, argument, reference)
					request, cliErr := app.prepareRequest(context.Background(), command, options, resolved, "flint_test_test", "")
					if cliErr != nil {
						t.Fatalf("prepareRequest(%s): %v", reference, cliErr)
					}
					serialized := request.Path + string(request.Body)
					if !strings.Contains(serialized, historyID) {
						t.Errorf("prepared request does not contain resolved ID %q: %s", historyID, serialized)
					}
					if strings.Contains(serialized, "@last") {
						t.Errorf("prepared request leaked history reference: %s", serialized)
					}

					emptyApp, _, _ := testApp(t, "")
					missingOptions := historyReferenceOptions(t, command, argument, reference)
					_, missingErr := emptyApp.prepareRequest(context.Background(), command, missingOptions, resolved, "flint_test_test", "")
					if missingErr == nil || missingErr.Code != "HISTORY_REFERENCE_NOT_FOUND" || missingErr.ExitCode != ExitUsage {
						t.Fatalf("missing history error = %#v, want HISTORY_REFERENCE_NOT_FOUND exit %d", missingErr, ExitUsage)
					}
				})
			}
		}
	}
}

func TestOrderPaymentHistoryMapKeyResolves(t *testing.T) {
	command, ok := NewRegistry().ByName("orders.pay")
	if !ok {
		t.Fatal("orders.pay command is missing")
	}
	resolved := ResolvedConfig{ProfileName: "default", Environment: "sandbox"}
	app, _, _ := testApp(t, "")
	if err := app.saveHistory(History{Entries: []HistoryEntry{{
		ID: "pi_history", ResourceType: "payment_intent", Command: "test",
		Profile: resolved.ProfileName, Environment: resolved.Environment, CreatedAt: time.Now(),
	}}}); err != nil {
		t.Fatal(err)
	}

	_, options, _, cliErr := parseInvocation(NewRegistry(), []string{
		"orders", "pay", "ord_test", "--payment-source-token", "@last.pi=pm_card_visa",
	})
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	request, cliErr := app.prepareRequest(context.Background(), command, options, resolved, "flint_test_test", "")
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	raw := string(request.Body)
	if !strings.Contains(raw, `"payment_intent_id":"pi_history"`) || strings.Contains(raw, "@last") {
		t.Fatalf("order payment body did not resolve history map key: %s", raw)
	}
}

func TestEveryHelpExampleParsesAsItsDocumentedCommand(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		for index, example := range command.Examples {
			example := example
			t.Run(fmt.Sprintf("%s/%d", command.CanonicalName, index), func(t *testing.T) {
				fields := strings.Fields(example)
				if len(fields) == 0 || fields[0] != "flint" {
					t.Fatalf("example is not a flint invocation: %q", example)
				}
				argv := make([]string, 0, len(fields)-1)
				for i := 1; i < len(fields); i++ {
					if fields[i] == "<" {
						break
					}
					argv = append(argv, fields[i])
				}
				parsed, _, _, cliErr := parseInvocation(NewRegistry(), argv)
				if cliErr != nil {
					t.Fatalf("example %q does not parse: %v", example, cliErr)
				}
				if parsed.CanonicalName != command.CanonicalName {
					t.Fatalf("example %q resolves to %s, want %s", example, parsed.CanonicalName, command.CanonicalName)
				}
			})
		}
	}
}

func TestEveryAliasExecutesLikeItsCanonicalCommand(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		for index, aliasPath := range command.AliasPaths {
			if index >= len(command.Aliases) {
				t.Fatalf("%s has an alias path without an alias name", command.CanonicalName)
			}
			aliasName := command.Aliases[index]
			t.Run(aliasName, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if request.URL.Path == "/v1/developer/auth-context" {
						fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
						return
					}
					if command.ResponseMediaType != "" {
						w.Header().Set("Content-Type", command.ResponseMediaType)
						fmt.Fprint(w, "test file content")
						return
					}
					fmt.Fprint(w, `{"data":{"resource_id":"res_test","status":"succeeded","url":"https://example.com/result"},"request_id":"req_test"}`)
				}))
				defer server.Close()

				var canonicalArgs []string
				if command.Local {
					canonicalArgs = append(minimalInvocation(command), "--no-input", "--output", "json")
				} else {
					canonicalArgs = exhaustiveRemoteInvocation(command)
				}
				canonicalPrefixLength := len(command.Path) - 1
				aliasArgs := append([]string(nil), aliasPath[1:]...)
				aliasArgs = append(aliasArgs, canonicalArgs[canonicalPrefixLength:]...)

				run := func(argv []string) (int, string, string) {
					app, stdout, stderr := testApp(t, server.URL)
					app.Stdin = strings.NewReader(`{"input_marker":"present"}`)
					exit := app.Run(argv)
					return exit, stdout.String(), stderr.String()
				}
				canonicalExit, canonicalStdout, canonicalStderr := run(canonicalArgs)
				aliasExit, aliasStdout, aliasStderr := run(aliasArgs)
				if aliasExit != canonicalExit || aliasStdout != canonicalStdout || aliasStderr != canonicalStderr {
					t.Fatalf(
						"alias behavior differs\ncanonical argv=%v exit=%d stdout=%q stderr=%q\nalias argv=%v exit=%d stdout=%q stderr=%q",
						canonicalArgs, canonicalExit, canonicalStdout, canonicalStderr,
						aliasArgs, aliasExit, aliasStdout, aliasStderr,
					)
				}
			})
		}
	}
}

func TestEveryGeneratedCommandSchemaCompiles(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		for _, input := range []bool{true, false} {
			direction := "output"
			if input {
				direction = "input"
			}
			t.Run(command.CanonicalName+"/"+direction, func(t *testing.T) {
				schema, cliErr := schemaForCommand(command, input)
				if cliErr != nil {
					t.Fatal(cliErr)
				}
				raw, err := json.Marshal(schema)
				if err != nil {
					t.Fatal(err)
				}
				var normalized any
				if err := json.Unmarshal(raw, &normalized); err != nil {
					t.Fatal(err)
				}
				resourceURL := "https://cli.withflintpay.com/test/" + command.CanonicalName + "/" + direction
				compiler := jsonschema.NewCompiler()
				if err := compiler.AddResource(resourceURL, normalized); err != nil {
					t.Fatalf("add schema resource: %v", err)
				}
				if _, err := compiler.Compile(resourceURL); err != nil {
					t.Fatalf("compile generated schema: %v", err)
				}
			})
		}
	}
}

func TestEveryParserFlagHasPositiveCase(t *testing.T) {
	cases := map[string][]string{
		"all":             {"customers", "list", "--all"},
		"clear":           {"history", "--clear"},
		"color":           {"version", "--color", "never"},
		"confirm":         {"history", "--clear", "--confirm"},
		"debug":           {"version", "--debug"},
		"dry-run":         {"customers", "create", "--dry-run", "client"},
		"expand":          {"customers", "get", "cus_test", "--expand", "default_payment_method"},
		"field":           {"version", "--field", "data.cli_version"},
		"fix":             {"doctor", "--fix"},
		"for":             {"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--for", "1s"},
		"help":            {"version", "--help"},
		"idempotency-key": {"customers", "create", "--idempotency-key", "idem_test"},
		"input":           {"customers", "create", "--input", "-"},
		"jq":              {"version", "--jq", ".data"},
		"live":            {"sandboxes", "list", "--live"},
		"max-events":      {"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--max-events", "1"},
		"merchant":        {"version", "--merchant", "mer_test"},
		"no-input":        {"version", "--no-input"},
		"open":            {"checkout-sessions", "create", "--open"},
		"output":          {"version", "--output", "json"},
		"page-size":       {"customers", "list", "--page-size", "1"},
		"page-token":      {"customers", "list", "--page-token", "page_test"},
		"paginate":        {"api", "get", "/v1/customers", "--paginate"},
		"preview":         {"webhook-endpoints", "delete", "whep_test", "--preview"},
		"profile":         {"version", "--profile", "test"},
		"progress":        {"customers", "list", "--all", "--progress", "plain"},
		"quiet":           {"version", "--quiet"},
		"select":          {"version", "--select", "cli_version,api_version"},
		"stdin":           {"auth", "import", "--stdin"},
		"timeout":         {"auth", "status", "--timeout", "1s"},
		"wait-for":        {"customers", "get", "cus_test", "--wait-for", "status=active"},
	}

	for flag := range globalFlags {
		argv, ok := cases[flag]
		if !ok {
			t.Errorf("global parser flag --%s has no positive test case", flag)
			continue
		}
		_, options, help, cliErr := parseInvocation(NewRegistry(), argv)
		if cliErr != nil {
			t.Errorf("--%s positive case %v failed: %v", flag, argv, cliErr)
			continue
		}
		if len(options.Raw[flag]) == 0 {
			t.Errorf("--%s positive case was not recorded in Options.Raw", flag)
		}
		if flag == "help" && !help {
			t.Errorf("--help positive case did not request help")
		}
	}
	for flag := range cases {
		if !globalFlags[flag] {
			t.Errorf("positive test case references unregistered flag --%s", flag)
		}
	}
}

func TestEveryAdvertisedExecutionControlIsAcceptedByItsCommand(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		t.Run(command.CanonicalName, func(t *testing.T) {
			schema, cliErr := schemaForCommand(command, true)
			if cliErr != nil {
				t.Fatal(cliErr)
			}
			properties, _ := schema["properties"].(map[string]any)
			flint, _ := properties["_flint"].(map[string]any)
			controls, _ := flint["properties"].(map[string]any)
			if len(controls) == 0 {
				t.Fatal("input schema has no _flint execution controls")
			}
			for control := range controls {
				control := control
				t.Run(control, func(t *testing.T) {
					argv := minimalInvocation(command)
					if command.CanonicalName == "api" && control == "preview" {
						argv[1] = "delete"
						argv[2] = "/v1/webhook-endpoints/whep_test"
					}
					switch control {
					case "all":
						argv = append(argv, "--all=false")
					case "color":
						argv = append(argv, "--color", "never")
					case "confirm":
						if command.CanonicalName == "history" {
							argv = append(argv, "--clear")
						}
						argv = append(argv, "--confirm=false")
					case "debug":
						argv = append(argv, "--debug=false")
					case "dry_run":
						argv = append(argv, "--dry-run", "client")
					case "expand":
						values, err := expansionValues(command.OperationID)
						if err != nil || len(values) == 0 {
							t.Fatalf("advertised expansion has no value: %v, %#v", err, values)
						}
						argv = append(argv, "--expand", values[0])
					case "field":
						argv = append(argv, "--field", "data")
					case "for":
						if command.Supports.WaitFor {
							argv = append(argv, "--wait-for", "status=succeeded")
						}
						argv = append(argv, "--for", "1s")
					case "idempotency_key":
						argv = append(argv, "--idempotency-key", "idem_test")
					case "jq":
						argv = append(argv, "--jq", ".")
					case "live":
						argv = append(argv, "--live=false")
					case "max_events":
						argv = append(argv, "--max-events", "1")
					case "merchant":
						argv = append(argv, "--merchant", "mer_test")
					case "open":
						argv = append(argv, "--open=false")
					case "page_size":
						argv = append(argv, "--page-size", "1")
					case "page_token":
						argv = append(argv, "--page-token", "page_test")
					case "paginate":
						argv = append(argv, "--paginate=false")
					case "preview":
						argv = append(argv, "--preview=false")
					case "profile":
						argv = append(argv, "--profile", "test")
					case "progress":
						if command.Supports.Pagination {
							argv = append(argv, "--all")
						} else if command.Supports.WaitFor {
							argv = append(argv, "--wait-for", "status=succeeded")
						} else if command.CanonicalName == "api" {
							// Raw pagination is a read-only capability, unlike
							// the default raw mutation fixture.
							argv[1] = "get"
							argv[2] = "/v1/customers"
							argv = append(argv, "--paginate")
						}
						argv = append(argv, "--progress", "quiet")
					case "quiet":
						argv = append(argv, "--quiet=false")
					case "select":
						argv = append(argv, "--select", "status")
					case "timeout":
						argv = append(argv, "--timeout", "1s")
					case "wait_for":
						argv = append(argv, "--wait-for", "status=succeeded")
					default:
						t.Fatalf("advertised execution control %q has no parser fixture", control)
					}
					if _, _, _, parseErr := parseInvocation(NewRegistry(), argv); parseErr != nil {
						t.Fatalf("advertised control %q is rejected for %s: argv=%v error=%v", control, command.CanonicalName, argv, parseErr)
					}
				})
			}
		})
	}
}

func TestEveryBooleanFlagAcceptsExplicitTrueAndFalse(t *testing.T) {
	cases := map[string][]string{
		"all":      {"customers", "list"},
		"clear":    {"history"},
		"confirm":  {"history", "--clear"},
		"debug":    {"version"},
		"fix":      {"doctor"},
		"help":     {"version"},
		"live":     {"sandboxes", "list"},
		"no-input": {"version"},
		"open":     {"checkout-sessions", "create"},
		"paginate": {"api", "get", "/v1/customers"},
		"preview":  {"webhook-endpoints", "delete", "whep_test"},
		"quiet":    {"version"},
		"stdin":    {"auth", "import"},
		"private":  {"support", "open"},
		"ai-agent": {"support", "open"},
		"no-open":  {"support", "open"},
	}
	if len(cases) != len(boolFlags) {
		t.Fatalf("boolean flag matrix has %d entries, parser has %d", len(cases), len(boolFlags))
	}
	for flag := range boolFlags {
		base, ok := cases[flag]
		if !ok {
			t.Errorf("boolean flag --%s has no explicit true/false test", flag)
			continue
		}
		for _, value := range []string{"true", "false"} {
			argv := append(append([]string(nil), base...), "--"+flag+"="+value)
			_, options, _, cliErr := parseInvocation(NewRegistry(), argv)
			if cliErr != nil {
				t.Errorf("%v failed: %v", argv, cliErr)
				continue
			}
			if got := options.Raw[flag]; !reflect.DeepEqual(got, []string{value}) {
				t.Errorf("%v recorded %#v", argv, got)
			}
		}
	}
}

func TestParserFlagValueDomainsAndBoundaries(t *testing.T) {
	valid := [][]string{
		{"version", "--output", "human"},
		{"version", "--output", "json"},
		{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--output", "ndjson"},
		{"version", "--color", "auto"},
		{"version", "--color", "always"},
		{"version", "--color", "never"},
		{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--progress", "auto"},
		{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--progress", "plain"},
		{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--progress", "json"},
		{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--progress", "quiet"},
		{"customers", "create", "--dry-run", "client"},
		{"customers", "list", "--page-size", "1"},
		{"customers", "list", "--page-size", "100"},
		{"auth", "status", "--timeout", "1ns"},
		{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--max-events", "1"},
		{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--for", "1ns"},
		{"version", "--select", "cli_version", "--select", "api_version"},
		{"customers", "get", "cus_test", "--expand", "default_payment_method"},
	}
	for _, argv := range valid {
		if _, _, _, cliErr := parseInvocation(NewRegistry(), argv); cliErr != nil {
			t.Errorf("valid argv %v failed: %v", argv, cliErr)
		}
	}

	invalid := []struct {
		argv []string
		code string
	}{
		{argv: []string{"version", "--output", "yaml"}, code: "INVALID_OUTPUT"},
		{argv: []string{"version", "--output", "ndjson"}, code: "UNSUPPORTED_FLAG"},
		{argv: []string{"version", "--color", "sometimes"}, code: "INVALID_COLOR"},
		{argv: []string{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--progress", "verbose"}, code: "INVALID_PROGRESS"},
		{argv: []string{"customers", "create", "--dry-run", "server"}, code: "INVALID_DRY_RUN"},
		{argv: []string{"customers", "list", "--page-size", "0"}, code: "INVALID_PAGE_SIZE"},
		{argv: []string{"customers", "list", "--page-size", "101"}, code: "INVALID_PAGE_SIZE"},
		{argv: []string{"customers", "list", "--page-size", "many"}, code: "INVALID_PAGE_SIZE"},
		{argv: []string{"auth", "status", "--timeout", "0s"}, code: "INVALID_DURATION"},
		{argv: []string{"auth", "status", "--timeout", "-1s"}, code: "INVALID_DURATION"},
		{argv: []string{"auth", "status", "--timeout", "later"}, code: "INVALID_DURATION"},
		{argv: []string{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--max-events", "0"}, code: "INVALID_MAX_EVENTS"},
		{argv: []string{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--max-events", "-1"}, code: "INVALID_MAX_EVENTS"},
		{argv: []string{"listen", "--forward-to", "http://127.0.0.1:8080/webhooks", "--for", "0s"}, code: "INVALID_DURATION"},
		{argv: []string{"version", "--quiet=maybe"}, code: "INVALID_FLAG_VALUE"},
		{argv: []string{"version", "--profile="}, code: "INVALID_FLAG_VALUE"},
		{argv: []string{"version", "--select", ","}, code: "INVALID_FLAG_VALUE"},
		{argv: []string{"customers", "get", "cus_test", "--expand", ","}, code: "INVALID_FLAG_VALUE"},
		{argv: []string{"customers", "get", "cus_test", "--expand", "order"}, code: "UNSUPPORTED_EXPANSION"},
		{argv: []string{"customers", "list", "--all", "--page-token", "page_test"}, code: "CONFLICTING_FLAGS"},
		{argv: []string{"version", "--field", "data", "--jq", "."}, code: "CONFLICTING_FLAGS"},
	}
	for _, test := range invalid {
		_, _, _, cliErr := parseInvocation(NewRegistry(), test.argv)
		if cliErr == nil || cliErr.Code != test.code {
			t.Errorf("invalid argv %v error = %#v, want %s", test.argv, cliErr, test.code)
		}
	}
}

func TestOutputTransformFlagsExecuteEndToEnd(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "field",
			argv: []string{"version", "--field", "data.cli_version"},
			want: "test\n",
		},
		{
			name: "select",
			argv: []string{"version", "--select", "cli_version,api_version", "--output", "json"},
			want: `{"data":{"api_version":"2026-02-01","cli_version":"test"}}` + "\n",
		},
		{
			name: "jq",
			argv: []string{"version", "--jq", ".data.cli_version", "--output", "json"},
			want: `"test"` + "\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "")
			if exit := app.Run(test.argv); exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			if stdout.String() != test.want {
				t.Fatalf("stdout=%q, want %q", stdout.String(), test.want)
			}
		})
	}
}

func TestExpandAndOpenFlagsExecuteEndToEnd(t *testing.T) {
	t.Run("expand", func(t *testing.T) {
		expand := ""
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if request.URL.Path == "/v1/developer/auth-context" {
				fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
				return
			}
			expand = request.URL.Query().Get("expand")
			fmt.Fprint(w, `{"data":{"customer_id":"cus_test"}}`)
		}))
		defer server.Close()
		app, stdout, stderr := testApp(t, server.URL)
		exit := app.Run([]string{"customers", "get", "cus_test", "--expand", "default_payment_method", "--output", "json"})
		if exit != ExitOK || stderr.Len() != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if expand != "default_payment_method" {
			t.Fatalf("expand query = %q", expand)
		}
	})

	t.Run("open without TTY", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if request.URL.Path == "/v1/developer/auth-context" {
				fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
				return
			}
			fmt.Fprint(w, `{"data":{"checkout_session":{"checkout_session_id":"cs_test"},"hosted_checkout":{"url":"https://checkout.example.com/cs_test"}}}`)
		}))
		defer server.Close()
		app, stdout, stderr := testApp(t, server.URL)
		app.IsTTY = func() bool { return false }
		exit := app.Run([]string{
			"checkout-sessions", "create",
			"--quick-pay-name", "Test",
			"--amount", "2500",
			"--currency", "USD",
			"--open",
		})
		if exit != ExitOK || stderr.Len() != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if strings.Count(stdout.String(), "https://checkout.example.com/cs_test") != 1 {
			t.Fatalf("checkout URL was not printed exactly once: %s", stdout)
		}
	})
}

func TestProgressAndQuietFlagBehavior(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		want    string
	}{
		{name: "automatic human", options: Options{Progress: "auto", Output: "human"}, want: "Fetched page 1\n"},
		{name: "plain", options: Options{Progress: "plain", Output: "json"}, want: "Fetched page 1\n"},
		{name: "json", options: Options{Progress: "json", Output: "human"}, want: `{"pages":1,"type":"pagination"}` + "\n"},
		{name: "explicit quiet", options: Options{Progress: "quiet", Output: "human"}, want: ""},
		{name: "quiet global", options: Options{Progress: "plain", Output: "human", Quiet: true}, want: ""},
		{name: "automatic machine", options: Options{Progress: "auto", Output: "json"}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, _, stderr := testApp(t, "")
			err := app.writeProgress(test.options, map[string]any{"type": "pagination", "pages": 1}, "Fetched page 1")
			if err != nil {
				t.Fatal(err)
			}
			if stderr.String() != test.want {
				t.Fatalf("stderr=%q, want %q", stderr.String(), test.want)
			}
		})
	}
}

func TestDebugFlagPrintsMetadataWithoutCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":{"customer_id":"cus_test"},"meta":{"api_version":"2026-02-01","trace_id":"trace_test"}}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"customers", "get", "cus_test", "--debug", "--output", "json"})
	if exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	for _, want := range []string{"debug: GET /v1/customers/cus_test", "api_version=2026-02-01", "trace_id=trace_test"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("debug output is missing %q: %s", want, stderr)
		}
	}
	if strings.Contains(stderr.String(), "flint_test_test") {
		t.Errorf("debug output leaked the credential: %s", stderr)
	}
}

func TestMachineModesNeverPromptEvenWithTTY(t *testing.T) {
	tests := []struct {
		name        string
		environment string
		argv        []string
	}{
		{
			name:        "json implies no input",
			environment: "live",
			argv: []string{
				"payment-intents", "create",
				"--amount", "2500",
				"--currency", "USD",
				"--payment-option", "card",
				"--live",
				"--output", "json",
			},
		},
		{
			name:        "explicit no input",
			environment: "sandbox",
			argv:        []string{"webhook-endpoints", "delete", "whep_test", "--no-input"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, exhaustiveAuthContextJSON(test.environment))
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, server.URL)
			reader := &countingReader{}
			app.Stdin = reader
			app.IsTTY = func() bool { return true }
			exit := app.Run(test.argv)
			if exit != ExitConfirmation {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			if reader.Reads != 0 {
				t.Fatalf("machine mode attempted %d reads from stdin", reader.Reads)
			}
		})
	}
}

func TestEnvironmentFlagsAndExplicitFlagsHaveCorrectPrecedence(t *testing.T) {
	t.Setenv("FLINT_OUTPUT", "json")
	t.Setenv("FLINT_COLOR", "always")
	t.Setenv("FLINT_NO_INPUT", "1")

	_, inherited, _, cliErr := parseInvocation(NewRegistry(), []string{"version"})
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	applyEnvironmentOptions(&inherited)
	if inherited.Output != "json" || inherited.Color != "always" || !inherited.NoInput {
		t.Fatalf("inherited options = %#v", inherited)
	}

	_, explicit, _, cliErr := parseInvocation(NewRegistry(), []string{"version", "--output", "human", "--color", "never", "--no-input=false"})
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	applyEnvironmentOptions(&explicit)
	if explicit.Output != "human" || explicit.Color != "never" || !explicit.NoInput {
		t.Fatalf("explicit options = %#v", explicit)
	}
}

func TestInputFlagReadsAFile(t *testing.T) {
	inputPath := t.TempDir() + "/customer.json"
	if err := os.WriteFile(inputPath, []byte(`{"email":"file@example.com","name":"File Input"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		fmt.Fprint(w, `{"data":{"customer_id":"cus_test"}}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"customers", "create", "--input", inputPath, "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if body["email"] != "file@example.com" || body["name"] != "File Input" {
		t.Fatalf("request body = %#v", body)
	}
}

func TestListenExecutesEveryDeclaredArgument(t *testing.T) {
	const cursor = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	var eventType, lastEventID, cursorQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
			return
		}
		eventType = request.URL.Query().Get("event_type")
		cursorQuery = request.URL.Query().Get("after_event_id")
		lastEventID = request.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: ready\ndata: {\"cursor\":%q}\n\n", cursor)
		w.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{
		"listen",
		"--forward-to", "http://127.0.0.1:1/webhooks",
		"--event-type", "payment_intent.succeeded",
		"--cursor", cursor,
		"--for", "25ms",
		"--timeout", "1s",
		"--output", "ndjson",
	})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if eventType != "payment_intent.succeeded" || lastEventID != cursor || cursorQuery != "" {
		t.Fatalf("stream request event_type=%q Last-Event-ID=%q after_event_id=%q", eventType, lastEventID, cursorQuery)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var value any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("invalid listener NDJSON: %v: %s", err, line)
		}
	}
}

type countingReader struct {
	Reads int
}

func (r *countingReader) Read([]byte) (int, error) {
	r.Reads++
	return 0, io.EOF
}

func TestEveryRemoteCommandBuildsAndExecutesARequest(t *testing.T) {
	fixedNow := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	for _, command := range NewRegistry().Commands {
		command := command
		if command.Local || command.CanonicalName == "listen" {
			continue
		}
		t.Run(command.CanonicalName, func(t *testing.T) {
			environment := "sandbox"
			if strings.HasPrefix(command.CanonicalName, "sandboxes.") {
				environment = "live"
			}
			var captured *exhaustiveHTTPRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, exhaustiveAuthContextJSON(environment))
					return
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				captured = &exhaustiveHTTPRequest{
					Method: request.Method,
					URL:    request.URL,
					Header: request.Header.Clone(),
					Body:   body,
				}
				if command.ResponseMediaType != "" {
					w.Header().Set("Content-Type", command.ResponseMediaType)
					fmt.Fprint(w, "test file content")
					return
				}
				fmt.Fprint(w, `{"data":{"resource_id":"res_test","status":"succeeded","url":"https://example.com/result"},"request_id":"req_test"}`)
			}))
			defer server.Close()

			app, stdout, stderr := testApp(t, server.URL)
			app.Now = func() time.Time { return fixedNow }
			app.Stdin = strings.NewReader(`{"input_marker":"present"}`)
			argv := exhaustiveRemoteInvocation(command)
			exit := app.Run(argv)
			if exit != ExitOK {
				t.Fatalf("argv=%v exit=%d stdout=%s stderr=%s", argv, exit, stdout, stderr)
			}
			var output any
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("stdout is not JSON: %v: %s", err, stdout)
			}
			if command.CanonicalName == "auth.status" {
				if captured != nil {
					t.Fatalf("auth.status made an unexpected second request: %#v", captured)
				}
				return
			}
			if captured == nil {
				t.Fatal("command made no API request")
			}
			assertExhaustiveRequest(t, command, captured, fixedNow)
		})
	}
}

func TestEveryMutationExecutesClientDryRunWithoutNetwork(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		if !command.Mutation {
			continue
		}
		t.Run(command.CanonicalName, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
			app.Stdin = strings.NewReader(`{"input_marker":"present"}`)
			argv := minimalInvocation(command)
			argv = append(argv, "--dry-run", "client", "--output", "json")
			if strings.HasPrefix(command.CanonicalName, "sandboxes.") {
				t.Setenv("FLINT_API_KEY", "flint_live_exhaustive")
				argv = append(argv, "--live")
			}
			exit := app.Run(argv)
			if exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("argv=%v exit=%d stdout=%s stderr=%s", argv, exit, stdout, stderr)
			}
			var result map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("invalid dry-run JSON: %v: %s", err, stdout)
			}
			schema, schemaErr := mcpOutputSchema(command)
			if schemaErr != nil {
				t.Fatal(schemaErr)
			}
			assertMCPOutputMatchesSchema(t, schema, result)
			if sideEffects, ok := lookupPath(result, "data.persistent_side_effects"); !ok || sideEffects != false {
				t.Fatalf("dry-run result = %#v", result)
			}
			if key, ok := lookupPath(result, "data.headers.Idempotency-Key"); !ok || strings.TrimSpace(fmt.Sprint(key)) == "" {
				t.Fatalf("dry-run omitted generated idempotency key: %#v", result)
			}
		})
	}

	t.Run("raw API mutation", func(t *testing.T) {
		app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
		app.Stdin = strings.NewReader(`{"name":"Dry Run"}`)
		exit := app.Run([]string{
			"api", "post", "/v1/customers",
			"--input", "-",
			"--dry-run", "client",
			"--idempotency-key", "idem_raw_dry_run",
			"--output", "json",
		})
		if exit != ExitOK || stderr.Len() != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if !strings.Contains(stdout.String(), `"Idempotency-Key":"idem_raw_dry_run"`) {
			t.Fatalf("raw dry-run output = %s", stdout)
		}
	})
}

func TestEveryDestructiveMutationExecutesPreviewWithoutMutation(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		if !command.Mutation || !command.Destructive {
			continue
		}
		t.Run(command.CanonicalName, func(t *testing.T) {
			endpointCalls := 0
			environment := "sandbox"
			if strings.HasPrefix(command.CanonicalName, "sandboxes.") {
				environment = "live"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, exhaustiveAuthContextJSON(environment))
					return
				}
				endpointCalls++
				fmt.Fprint(w, `{"data":{}}`)
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, server.URL)
			argv := append(minimalInvocation(command), "--preview", "--output", "json")
			if environment == "live" {
				argv = append(argv, "--live")
			}
			exit := app.Run(argv)
			if exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("argv=%v exit=%d stdout=%s stderr=%s", argv, exit, stdout, stderr)
			}
			var result any
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			schema, schemaErr := mcpOutputSchema(command)
			if schemaErr != nil {
				t.Fatal(schemaErr)
			}
			assertMCPOutputMatchesSchema(t, schema, result)
			if endpointCalls != 0 {
				t.Fatalf("preview made %d mutation requests", endpointCalls)
			}
			if !strings.Contains(stdout.String(), `"persistent_side_effects":false`) ||
				!strings.Contains(stdout.String(), `"reversible":false`) {
				t.Fatalf("preview output = %s", stdout)
			}
		})
	}

	t.Run("raw API delete", func(t *testing.T) {
		endpointCalls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if request.URL.Path == "/v1/developer/auth-context" {
				fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
				return
			}
			endpointCalls++
			fmt.Fprint(w, `{"data":{}}`)
		}))
		defer server.Close()
		app, stdout, stderr := testApp(t, server.URL)
		exit := app.Run([]string{"api", "delete", "/v1/webhook-endpoints/whep_test", "--preview", "--output", "json"})
		if exit != ExitOK || stderr.Len() != 0 || endpointCalls != 0 {
			t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, endpointCalls, stdout, stderr)
		}
	})
}

func TestEveryWaitCapableCommandExecutesMatchingWait(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		if !command.Supports.WaitFor {
			continue
		}
		t.Run(command.CanonicalName, func(t *testing.T) {
			resourceCalls := 0
			environment := "sandbox"
			if strings.HasPrefix(command.CanonicalName, "sandboxes.") {
				environment = "live"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, exhaustiveAuthContextJSON(environment))
					return
				}
				resourceCalls++
				fmt.Fprint(w, `{"data":{"resource_id":"res_test","status":"succeeded"}}`)
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, server.URL)
			argv := append(minimalInvocation(command),
				"--wait-for", "status=succeeded",
				"--for", "100ms",
				"--progress", "quiet",
				"--output", "json",
			)
			if environment == "live" {
				argv = append(argv, "--live")
			}
			exit := app.Run(argv)
			if exit != ExitOK || stderr.Len() != 0 || resourceCalls != 1 {
				t.Fatalf("argv=%v exit=%d calls=%d stdout=%s stderr=%s", argv, exit, resourceCalls, stdout, stderr)
			}
		})
	}
}

func TestEveryPaginatedCommandExecutesAllPages(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		if !command.Supports.Pagination || command.Stream {
			continue
		}
		t.Run(command.CanonicalName, func(t *testing.T) {
			resourceCalls := 0
			environment := "sandbox"
			if strings.HasPrefix(command.CanonicalName, "sandboxes.") {
				environment = "live"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, exhaustiveAuthContextJSON(environment))
					return
				}
				resourceCalls++
				if request.URL.Query().Get("page_size") != "1" {
					t.Errorf("page_size = %q", request.URL.Query().Get("page_size"))
				}
				nextToken := ""
				if resourceCalls == 1 {
					if token := request.URL.Query().Get("page_token"); token != "" {
						t.Errorf("first page token = %q", token)
					}
					nextToken = "page_2"
				} else if token := request.URL.Query().Get("page_token"); token != "page_2" {
					t.Errorf("second page token = %q", token)
				}
				var data any = []any{map[string]any{"resource_id": fmt.Sprintf("res_%d", resourceCalls)}}
				if command.CanonicalName == "timeline" {
					// The public timeline contract nests its collection beside
					// resource metadata instead of returning a data array.
					data = map[string]any{
						"resource_id": "pi_test", "resource_type": "payment_intent", "test": true,
						"entries": []any{map[string]any{
							"resource_timeline_entry_id": fmt.Sprintf("entry_%d", resourceCalls),
							"entry_type":                 "event", "occurred_at": "2026-09-13T00:00:00Z", "test": true,
						}},
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "next_page_token": nextToken})
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, server.URL)
			argv := append(minimalInvocation(command),
				"--page-size", "1",
				"--all",
				"--progress", "quiet",
				"--output", "json",
			)
			if environment == "live" {
				argv = append(argv, "--live")
			}
			exit := app.Run(argv)
			if exit != ExitOK || stderr.Len() != 0 || resourceCalls != 2 {
				t.Fatalf("argv=%v exit=%d calls=%d stdout=%s stderr=%s", argv, exit, resourceCalls, stdout, stderr)
			}
			var envelope map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			collection := envelope["data"]
			if command.CanonicalName == "timeline" {
				collection, _ = lookupPath(envelope, "data.entries")
			}
			data, _ := collection.([]any)
			if len(data) != 2 || envelope["next_page_token"] != "" {
				t.Fatalf("combined pagination output = %#v", envelope)
			}
		})
	}
}

func TestEveryLocalCommandExecutes(t *testing.T) {
	for _, command := range NewRegistry().Commands {
		command := command
		if !command.Local {
			continue
		}
		t.Run(command.CanonicalName, func(t *testing.T) {
			if command.CanonicalName == "signup" {
				testExhaustiveSignup(t)
				return
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/openapi.json" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if request.URL.Path != "/v1/developer/auth-context" {
					t.Errorf("unexpected local-command API request: %s %s", request.Method, request.URL.Path)
				}
				fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, server.URL)
			argv := append([]string(nil), command.Path[1:]...)

			switch command.CanonicalName {
			case "auth.import":
				t.Setenv("FLINT_API_KEY", "")
				app.Stdin = strings.NewReader("flint_test_imported\n")
				stored := ""
				app.StoreCredential = func(_ string, secret string) error {
					stored = secret
					return nil
				}
				argv = append(argv, "--stdin")
				defer func() {
					if stored != "flint_test_imported" {
						t.Errorf("stored credential = %q", stored)
					}
				}()
			case "auth.logout":
				t.Setenv("FLINT_API_KEY", "")
				app.LoadCredential = func(string) (string, error) { return "flint_test_existing", nil }
				deleted := false
				app.DeleteCredential = func(string) error {
					deleted = true
					return nil
				}
				argv = append(argv, "--confirm")
				defer func() {
					if !deleted {
						t.Error("auth.logout did not delete the credential")
					}
				}()
			case "config.set":
				argv = append(argv, "profile", "exhaustive")
			case "doctor":
				argv = append(argv, "--fix")
			case "history":
				argv = append(argv, "--clear", "--confirm")
			case "schema.command", "schema.input", "schema.output":
				argv = append(argv, "customers.list")
			case "help":
				argv = append(argv, "test-cards")
			case "help.search":
				helpStation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, exhaustiveHelpSearchJSON)
				}))
				defer helpStation.Close()
				t.Setenv("FLINT_HELP_URL", helpStation.URL)
				argv = append(argv, "webhook", "signature")
			case "support.open":
				t.Setenv("FLINT_HELP_URL", "https://help.example.com")
				argv = append(argv, "--no-open")
			case "mcp.serve":
				app.Stdin = strings.NewReader(
					"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"exhaustive-test\",\"version\":\"1\"}}}\n",
				)
			case "version", "config.get", "config.validate", "init",
				"schema.commands", "schema.errors", "schema.events":
			default:
				t.Fatalf("local command %s has no execution fixture", command.CanonicalName)
			}
			if command.CanonicalName != "mcp.serve" {
				argv = append(argv, "--output", "json")
			}
			exit := app.Run(argv)
			if exit != ExitOK {
				t.Fatalf("argv=%v exit=%d stdout=%s stderr=%s", argv, exit, stdout, stderr)
			}
			if strings.TrimSpace(stdout.String()) == "" {
				t.Fatalf("argv=%v produced no output", argv)
			}
			if stderr.Len() != 0 {
				t.Fatalf("argv=%v stderr=%s", argv, stderr)
			}
			if command.CanonicalName == "mcp.serve" {
				for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
					var value any
					if err := json.Unmarshal([]byte(line), &value); err != nil {
						t.Fatalf("argv=%v produced invalid JSON-RPC output: %v: %s", argv, err, line)
					}
				}
			} else {
				var value any
				if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
					t.Fatalf("argv=%v produced invalid JSON: %v: %s", argv, err, stdout)
				}
			}
		})
	}
}

func testExhaustiveSignup(t *testing.T) {
	t.Helper()
	var profileBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/onboarding/start":
			fmt.Fprint(w, `{"data":{"verification_token":"devver_test"}}`)
		case "/v1/onboarding/verify-email":
			fmt.Fprint(w, `{"data":{"onboarding_session_token":"devsess_test"}}`)
		case "/v1/onboarding/state":
			fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"next_step":{"code":"complete_profile","machine_completable":true,"submit_method":"POST","submit_endpoint":"/v1/onboarding/profile","required_fields":["country","website_url","support_email","support_phone","support_url"]}}}`)
		case "/v1/onboarding/profile":
			if err := json.NewDecoder(request.Body).Decode(&profileBody); err != nil {
				t.Errorf("decode profile body: %v", err)
			}
			fmt.Fprint(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_test"}}`)
		case "/v1/onboarding/api-key":
			fmt.Fprint(w, `{"data":{"api_key_id":"key_test","merchant_id":"mer_test","sandbox_id":"test_test","secret_key":"flint_test_created"}}`)
		default:
			t.Errorf("unexpected signup request: %s %s", request.Method, request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	stored := ""
	app.StoreCredential = func(_ string, secret string) error {
		stored = secret
		return nil
	}
	app.DeleteCredential = func(string) error { return nil }
	exit := app.Run([]string{
		"signup",
		"--email", "dev@example.com",
		"--first-name", "Ada",
		"--last-name", "Lovelace",
		"--verification-code", "482193",
		"--country", "us",
		"--website-url", "https://example.com",
		"--support-email", "support@example.com",
		"--support-phone", "+12125550123",
		"--support-url", "https://example.com/support",
		"--requested-capability", "payments",
		"--requested-capability", "webhooks",
		"--no-input",
		"--output", "json",
	})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if stored != "flint_test_created" {
		t.Errorf("stored credential = %q", stored)
	}
	if strings.Contains(stdout.String(), "flint_test_created") {
		t.Errorf("signup leaked the credential: %s", stdout)
	}
	wantProfile := map[string]any{
		"country": "US",
		"profile": map[string]any{
			"website_url":   "https://example.com",
			"support_email": "support@example.com",
			"support_phone": "+12125550123",
			"support_url":   "https://example.com/support",
		},
		"requested_capabilities": []any{"payments", "webhooks"},
	}
	if !reflect.DeepEqual(profileBody, wantProfile) {
		t.Errorf("signup profile body = %#v, want %#v", profileBody, wantProfile)
	}
}

type exhaustiveHTTPRequest struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   []byte
}

func minimalInvocation(command *Command) []string {
	argv := append([]string(nil), command.Path[1:]...)
	for _, argument := range command.Arguments {
		if argument.Positional > 0 {
			argv = append(argv, exhaustiveArgumentValues(argument)[0])
			continue
		}
		if !argument.Required {
			continue
		}
		argv = append(argv, argument.Flag, exhaustiveArgumentValues(argument)[0])
	}
	if command.CanonicalName == "payment-intents.create" {
		argv = append(argv, "--amount", "2500", "--currency", "USD", "--payment-option", "card")
	}
	return argv
}

func historyReferenceOptions(t *testing.T, command *Command, argument Arg, reference string) Options {
	t.Helper()
	_, options, _, cliErr := parseInvocation(NewRegistry(), minimalInvocation(command))
	if cliErr != nil {
		t.Fatalf("parse minimal %s invocation: %v", command.CanonicalName, cliErr)
	}
	if argument.Positional > 0 {
		options.Positionals[argument.Positional-1] = reference
	} else {
		options.Raw[strings.TrimPrefix(argument.Flag, "--")] = []string{reference}
	}
	if command.CanonicalName == "payment-intents.create" && argument.Name == "order" {
		delete(options.Raw, "amount")
		delete(options.Raw, "currency")
		delete(options.Raw, "payment-option")
		delete(options.Raw, "transaction-purpose")
	}
	return options
}

func historyQualifierForPrefix(prefix string) string {
	for qualifier, candidate := range historyQualifiers {
		if candidate == prefix {
			return qualifier
		}
	}
	return ""
}

func resourceTypeForPrefix(prefix string) string {
	if prefix == "" {
		return "payment_intent"
	}
	return resourcePrefixes[prefix]
}

func exhaustiveRemoteInvocation(command *Command) []string {
	argv := append([]string(nil), command.Path[1:]...)
	for _, argument := range command.Arguments {
		if command.CanonicalName == "payment-intents.create" && argument.Name == "order" {
			continue
		}
		if argument.Name == "save_to" {
			continue
		}
		values := exhaustiveArgumentValues(argument)
		if argument.Positional > 0 {
			argv = append(argv, values[0])
			continue
		}
		for _, value := range values {
			argv = append(argv, argument.Flag, value)
		}
	}
	argv = append(argv, "--output", "json", "--timeout", "2s")
	if command.CanonicalName == "api" {
		argv = append(argv, "--confirm")
	}
	if command.Mutation {
		argv = append(argv, "--idempotency-key", "idem_exhaustive")
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

func assertExhaustiveRequest(t *testing.T, command *Command, request *exhaustiveHTTPRequest, now time.Time) {
	t.Helper()
	wantMethod := command.Method
	wantPath := command.APIPath
	if command.CanonicalName == "api" {
		wantMethod = http.MethodPost
		wantPath = "/v1/payment-intents"
	}
	for _, argument := range command.Arguments {
		if argument.Positional == 0 || command.CanonicalName == "api" {
			continue
		}
		wantPath = strings.Replace(wantPath, "{"+argument.Name+"}", exhaustiveArgumentValues(argument)[0], 1)
	}
	if request.Method != wantMethod || request.URL.Path != wantPath {
		t.Errorf("request = %s %s, want %s %s", request.Method, request.URL.Path, wantMethod, wantPath)
	}
	wantAuth := "Bearer flint_test_test"
	if !command.AuthRequired {
		wantAuth = ""
	}
	if request.Header.Get("Authorization") != wantAuth {
		t.Errorf("Authorization header = %q", request.Header.Get("Authorization"))
	}
	if command.Mutation || command.CanonicalName == "api" {
		if request.Header.Get("Idempotency-Key") != "idem_exhaustive" && command.CanonicalName != "api" {
			t.Errorf("Idempotency-Key = %q, want idem_exhaustive", request.Header.Get("Idempotency-Key"))
		}
		if command.CanonicalName == "api" && request.Header.Get("Idempotency-Key") == "" {
			t.Error("raw mutation omitted Idempotency-Key")
		}
	}

	for _, argument := range command.Arguments {
		if argument.Query == "" {
			continue
		}
		values := exhaustiveArgumentValues(argument)
		for i, value := range values {
			if argument.Type == "time" {
				resolved, err := resolveTime(value, now)
				if err != nil {
					t.Fatal(err)
				}
				value = resolved
			}
			got := request.URL.Query()[argument.Query]
			if i >= len(got) || got[i] != value {
				t.Errorf("query %s = %#v, want value %q at index %d", argument.Query, got, value, i)
			}
		}
	}

	var body map[string]any
	if len(request.Body) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(request.Body)))
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil {
			t.Fatalf("decode request body: %v: %s", err, request.Body)
		}
	}
	hasInput := false
	for _, argument := range command.Arguments {
		if argument.Name == "input" {
			hasInput = true
			continue
		}
		if argument.BodyPath == "" || (command.CanonicalName == "payment-intents.create" && argument.Name == "order") {
			continue
		}
		if command.CanonicalName == "orders.pay" && argument.Name == "payment_source_token" {
			continue
		}
		got, ok := lookupPath(body, argument.BodyPath)
		if !ok {
			t.Errorf("body path %s is missing from %#v", argument.BodyPath, body)
			continue
		}
		values := exhaustiveArgumentValues(argument)
		if argument.Repeat {
			items, ok := got.([]any)
			if !ok || len(items) != len(values) {
				t.Errorf("body path %s = %#v, want %d values", argument.BodyPath, got, len(values))
				continue
			}
			for i := range values {
				if fmt.Sprint(items[i]) != values[i] {
					t.Errorf("body path %s[%d] = %v, want %s", argument.BodyPath, i, items[i], values[i])
				}
			}
		} else if fmt.Sprint(got) != values[0] {
			t.Errorf("body path %s = %v, want %s", argument.BodyPath, got, values[0])
		}
	}
	if hasInput {
		if marker, ok := body["input_marker"]; !ok || marker != "present" {
			t.Errorf("--input body marker missing from %#v", body)
		}
	}
	if command.CanonicalName == "orders.pay" {
		payments, ok := body["payment_intents"].([]any)
		if !ok || len(payments) != 2 {
			t.Errorf("orders.pay body = %#v, want two payment intent selections", body)
		}
	}
}

func exhaustiveArgumentValues(argument Arg) []string {
	if argument.Type == "boolean" {
		return []string{"true"}
	}
	if argument.Type == "time" {
		return []string{"2h"}
	}
	var value string
	switch argument.Name {
	case "method":
		value = "POST"
	case "path":
		value = "/v1/payment-intents"
	case "command":
		value = "customers.list"
	case "key":
		value = "profile"
	case "value":
		value = "test"
	case "topic":
		value = "test-cards"
	case "amount":
		value = "2500"
	case "currency":
		value = "USD"
	case "email", "recipient_email", "support_email":
		value = "dev@example.com"
	case "first_name":
		value = "Ada"
	case "last_name":
		value = "Lovelace"
	case "verification_code":
		value = "482193"
	case "country":
		value = "US"
	case "website_url":
		value = "https://example.com"
	case "support_url":
		value = "https://example.com/support"
	case "support_phone":
		value = "+12125550123"
	case "url":
		value = "https://example.com/webhooks/flint"
	case "forward_to":
		value = "http://127.0.0.1:8080/webhooks"
	case "event_type", "enabled_event":
		value = "payment_intent.succeeded"
	case "delivery_status":
		value = "succeeded"
	case "status":
		value = "succeeded"
	case "status_bucket":
		value = "success"
	case "request_id":
		value = "req_test"
	case "page_size":
		value = "2"
	case "page_token":
		value = "page_test"
	case "created_after":
		value = "2h"
	case "created_before":
		value = "2026-07-23T11:00:00Z"
	case "payment_source_token":
		value = "pi_test=pm_card_visa"
	case "confirmation_token":
		value = "ctoken_test"
	case "payment_method":
		value = "pm_test"
	case "payment_option":
		value = "card"
	case "capture_method":
		value = "automatic"
	case "transaction_purpose":
		value = "services"
	case "cancellation_reason":
		value = "abandoned"
	case "reason":
		value = "requested_by_customer"
	case "scope":
		value = "customers.read"
	case "requested_capability":
		value = "payments"
	case "stdin", "fix", "clear", "private", "no_open":
		value = "true"
	case "area":
		value = "webhooks"
	case "resource":
		value = "whep_test"
	case "query":
		value = "webhooks"
	case "input":
		value = "-"
	default:
		if argument.IDPrefix != "" {
			value = argument.IDPrefix + "test"
		} else {
			value = exhaustiveNamedID(argument.Name)
		}
	}
	if !argument.Repeat {
		return []string{value}
	}
	second := value + "_second"
	switch argument.Name {
	case "payment_source_token":
		second = "pi_second=pm_card_mastercard"
	case "payment_option":
		second = "ach_debit"
	case "enabled_event", "event_type":
		second = "refund.created"
	case "scope":
		second = "customers.write"
	case "requested_capability":
		second = "webhooks"
	}
	return []string{value, second}
}

func exhaustiveNamedID(name string) string {
	switch name {
	case "customer", "customer_id":
		return "cus_test"
	case "order", "order_id":
		return "ord_test"
	case "payment_intent":
		return "pi_test"
	case "webhook_endpoint_id":
		return "whep_test"
	default:
		return "test"
	}
}

func exhaustiveAuthContextJSON(environment string) string {
	return fmt.Sprintf(
		`{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":%q,"merchant_id":"mer_test","sandbox_id":"test_test","scopes":["customers.read","customers.write","payments.payment_intents.read","payments.payment_intents.write","webhooks.read","webhooks.write"]},"request_id":"req_auth","meta":{"api_version":"2026-02-01"}}`,
		environment,
	)
}
