package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSpecRequestExamplesValidateEveryBody(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	bodies, examples := 0, 0
	for _, methods := range doc.Paths {
		for _, operation := range methods {
			media, _ := requestContentSchema(operation.RequestBody)
			if media != "application/json" {
				continue
			}
			bodies++
			values := operationRequestExamples(operation)
			if len(values) == 0 {
				t.Errorf("%s has no request example", operation.OperationID)
			}
			cmd := &Command{OperationID: operation.OperationID, CanonicalName: operation.OperationID, InputSchema: requestSchemaName(operation.RequestBody)}
			for _, example := range values {
				examples++
				if _, e := validateRequestBody(cmd, operation.OperationID, example.Body); e != nil {
					t.Errorf("%s/%s: %s %v", operation.OperationID, example.Name, e.Message, e.Details)
				}
			}
		}
	}
	if bodies == 0 || examples < bodies {
		t.Fatalf("missing coverage: %d bodies, %d examples", bodies, examples)
	}
	t.Logf("validated %d examples for %d JSON request bodies", examples, bodies)
}

func TestSpecRequestHelpExamplesPrepareValidBodies(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("example preparation made a network request")
		return nil, nil
	})}
	commands := newRegistryFromSnapshot(true)
	count, flags := 0, 0
	for _, cmd := range commands.Commands {
		if cmd.Local || cmd.OperationID == "" {
			continue
		}
		operation, _ := openAPIOperationByID(cmd.OperationID)
		if media, _ := requestContentSchema(operation.RequestBody); media != "application/json" {
			continue
		}
		count++
		example := specRequestCommandExample(cmd, operation)
		if strings.Contains(example, "request.json") {
			t.Errorf("%s has unexplained file: %s", cmd.Name, example)
		}
		if !strings.Contains(example, "--input") {
			flags++
		}
		words := mcpFallbackWords(example)
		target, opts, _, e := parseInvocation(commands, words[1:])
		if e != nil {
			t.Errorf("%s: %v", example, e)
			continue
		}
		opts.DryRun = "client"
		request, e := app.prepareRequest(context.Background(), target, opts, ResolvedConfig{}, "", "http://127.0.0.1:9")
		if e != nil {
			t.Errorf("%s: %s %v", example, e.Message, e.Details)
			continue
		}
		matches := false
		for _, source := range operationRequestExamples(operation) {
			raw, _ := json.Marshal(source.Body)
			matches = matches || bytes.Equal(raw, request.Body)
		}
		if !matches {
			t.Errorf("%s changed the documented body: %s", example, request.Body)
		}
	}
	if count == 0 || flags == 0 {
		t.Fatalf("missing coverage: %d commands, %d flag examples", count, flags)
	}
	t.Logf("prepared %d command examples, %d use flags", count, flags)
}

func TestSpecRequestExamplesDiscoveryBudgetAndVariant(t *testing.T) {
	registry := NewRegistry()
	for _, cmd := range registry.Commands {
		examples := compactRequestExamples(cmd, "")
		if len(examples) == 0 {
			continue
		}
		raw, err := json.Marshal(examples)
		if err != nil || len(raw) > 600 {
			t.Fatalf("%s example budget: %d, %v", cmd.Name, len(raw), err)
		}
		for _, example := range examples {
			if _, e := validateRequestBody(cmd, cmd.Name, example.Body); e != nil {
				t.Fatal(cmd.Name, e)
			}
		}
	}
	cmd, _ := registry.ByName("refunds.create")
	for _, example := range compactRequestExamples(cmd, "full") {
		if example.Body["refund_type"] != "full" {
			t.Fatal("wrong variant", example)
		}
	}
}
