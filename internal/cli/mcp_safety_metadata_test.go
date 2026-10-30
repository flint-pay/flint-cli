package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestMCPSafetyMetadataDistinguishesHintsAndGates(t *testing.T) {
	app := discoveryFixture(t)
	policy := app.mcpPolicy(defaultOptions())
	policy.toolsFlag = []string{"all"}
	policy.toolPrefixes = []string{"customers.create", "customers.update", "refunds.create", "customers.addresses.delete", "history.clear"}
	tools, e := app.mcpAdvertisedTools(policy, latestMCPProtocolVersion)
	if e != nil {
		t.Fatal(e)
	}
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tool := range tools {
		name := fmt.Sprint(tool["name"])
		if mcpAlwaysEnabled(name) {
			continue
		}
		seen++
		meta, ok := tool["_meta"].(map[string]any)
		if !ok {
			t.Fatalf("%s lacks metadata", name)
		}
		entry, ok := index.Lookup(name)
		if !ok {
			t.Fatalf("%s is not indexed", name)
		}
		if meta["com.withflintpay/operation"] != entry.ID {
			t.Fatal(tool)
		}
		if (meta["com.withflintpay/sensitive"] == true) != entry.Sensitive {
			t.Fatal(tool)
		}
		hints := tool["annotations"].(map[string]any)
		if hints["destructiveHint"] != (entry.Kind == "write" && (entry.Irreversible || entry.Overwrites)) {
			t.Fatal(tool)
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		_, confirm := props["confirm"]
		if confirm != entry.ConfirmRequired() {
			t.Fatalf("%s confirm=%t, gated=%t", entry.ID, confirm, entry.ConfirmRequired())
		}
		if entry.ConfirmRequired() && !strings.Contains(fmt.Sprint(tool["description"]), "Needs confirm: true") {
			t.Fatal(tool)
		}
	}
	if seen != 5 {
		t.Fatalf("saw %d typed tools", seen)
	}
}

func TestMCPSafetyActionsPreserveFallbackAndRecovery(t *testing.T) {
	app := discoveryFixture(t)
	t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
	cmd := mustMCPTool(t, app, "refunds.create")
	args := map[string]any{"order_id": "ord_fixture", "refund_type": "full"}
	e := cliError(ExitConfirmation, errorTypeConfirmationRequired, "CONFIRMATION_NEEDS_USER", "Run this operation in a terminal.")
	e.Details = map[string]any{"command": "flint refunds create --input='{}' --idempotency-key=refund-fixture --live", "idempotency_key": "refund-fixture"}
	policy := app.mcpPolicy(defaultOptions())
	policy.allowLive = true
	actions := app.mcpNextActions(cmd, args, defaultOptions(), policy, e, "live")
	if len(actions) < 2 || actions[0]["actor"] != "user" || actions[0]["command"] != mcpErrorFact(e.Details, "command") {
		t.Fatal(actions)
	}
	text := fmt.Sprint(actions[0]["text"])
	for _, required := range []string{"same credential", "same project directory", "FLINT_API_KEY", "MCP client's env"} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing %s: %s", required, text)
		}
	}
	if actions[1]["actor"] != "agent" || !strings.HasPrefix(fmt.Sprint(actions[1]["text"]), "After the user says") {
		t.Fatal(actions)
	}
	call := actions[1]["call"].(map[string]any)
	if call["tool"] != "flint_read" {
		t.Fatal(call)
	}
}

func TestMCPSafetyRequiredAlwaysReturnsAgentConfirmCall(t *testing.T) {
	app := discoveryFixture(t)
	cmd := mustMCPTool(t, app, "refunds.create")
	for _, environment := range []string{"sandbox", "live", "unverified"} {
		e := cliError(ExitConfirmation, errorTypeConfirmationRequired, "CONFIRMATION_REQUIRED", "Confirmation is required.")
		actions := app.mcpNextActions(cmd, map[string]any{"order_id": "ord_fixture", "refund_type": "full"}, defaultOptions(), app.mcpPolicy(defaultOptions()), e, environment)
		call := actions[0]["call"].(map[string]any)
		if actions[0]["actor"] != "agent" || call["arguments"].(map[string]any)["confirm"] != true {
			t.Fatal(actions)
		}
	}
}
