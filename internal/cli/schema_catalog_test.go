package cli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSchemaErrorsAndHelpKeepSingleDataEnvelope(t *testing.T) {
	for _, argv := range [][]string{
		{"schema", "errors", "--output", "json"},
		{"help", "exit-codes", "--output", "json"},
	} {
		app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
		if exit := app.Run(argv); exit != ExitOK || stderr.Len() != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", argv, exit, stderr)
		}
		var envelope map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		data, ok := envelope["data"].(map[string]any)
		if !ok || len(envelope) != 1 || data["data"] != nil {
			t.Fatalf("%v: expected one data envelope: %s", argv, stdout)
		}
		var got []string
		for _, typ := range data["types"].([]any) {
			got = append(got, typ.(string))
		}
		if !slices.Equal(got, errorCatalogTypes()) || data["exit_codes"] == nil || data["listen_codes"] == nil {
			t.Fatalf("incomplete error catalog: %s", stdout)
		}
	}
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	if exit := app.Run([]string{"help", "errors", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if fields, ok := lookupPath(envelope, "data.fields"); !ok || fields.(map[string]any)["type"] == nil {
		t.Fatalf("help errors lost its field documentation: %s", stdout)
	}
}

func TestSchemaErrorCatalogDoesNotChangeAPIErrorTypes(t *testing.T) {
	for _, typ := range []string{"authentication_error", "validation_error", "future_api_error"} {
		response := &apiResponse{Status: http.StatusBadRequest, Headers: http.Header{}, Value: map[string]any{
			"error": map[string]any{"type": typ, "code": "SERVER_CODE", "message": "Server message"},
		}}
		got := apiErrorFromResponse(response)
		if got.Type != typ || got.Code != "SERVER_CODE" || got.Message != "Server message" {
			t.Fatalf("API error changed: %#v", got)
		}
	}
}

func TestSchemaEventsCompactAndFiltered(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	if exit := app.Run([]string{"schema", "events", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if stdout.Len() > 64<<10 {
		t.Fatalf("compact event catalog is %d bytes", stdout.Len())
	}
	var compact map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &compact); err != nil {
		t.Fatal(err)
	}
	data := compact["data"].(map[string]any)
	if !strings.Contains(data["next"].(string), "flint schema events") || strings.Contains(data["next"].(string), "schema_events") {
		t.Fatalf("terminal guidance = %s", data["next"])
	}
	summaries, err := eventTypeSummaries()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(summaries)
	var want []any
	_ = json.Unmarshal(raw, &want)
	if !reflect.DeepEqual(data["event_types"], want) || data["count"] != float64(len(want)) || compact["webhooks"] != nil {
		t.Fatalf("compact event list does not match snapshot: %s", stdout)
	}
	doc, _ := loadOpenAPI()
	for _, key := range []string{"signing", "retry_policy", "secret_rotation"} {
		if !reflect.DeepEqual(data[key], doc.WebhookMetadata[key]) {
			t.Errorf("compact event list lost %s", key)
		}
	}
	for _, filter := range []string{"payment_intent.succeeded", "payment_intent", "payment_intent."} {
		t.Run(filter, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
			if exit := app.Run([]string{"schema", "events", filter, "--output", "json"}); exit != ExitOK {
				t.Fatalf("exit=%d stderr=%s", exit, stderr)
			}
			var got any
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want, e := mcpEventSchemas(filter)
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(want)
			var decoded any
			_ = json.Unmarshal(raw, &decoded)
			if !reflect.DeepEqual(got, decoded) {
				t.Fatal("terminal schema differs from raw helper schema")
			}
			webhooks := got.(map[string]any)["webhooks"].(map[string]any)
			if len(webhooks) == 0 || (filter == "payment_intent.succeeded" && len(webhooks) != 1) {
				t.Fatalf("unexpected event matches: %v", webhooks)
			}
			for name := range webhooks {
				if !strings.HasPrefix(name, "payment_intent.") {
					t.Fatalf("unrelated event %s", name)
				}
			}
		})
	}
}

func TestSchemaEventsUnknownTypeUsesTerminalGuidance(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	if exit := app.Run([]string{"schema", "events", "payment_intnet", "--output", "json"}); exit != ExitUsage {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	output := stdout.String() + stderr.String()
	for _, want := range []string{"UNKNOWN_EVENT_TYPE", "payment_intent", "Run flint schema events"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in %s", want, output)
		}
	}
	if strings.Contains(output, "schema_events") || strings.Contains(output, "without event_type") {
		t.Fatalf("MCP guidance in terminal error: %s", output)
	}
}
