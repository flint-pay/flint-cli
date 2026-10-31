package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestMCPTypedBudgetsAndContract(t *testing.T) {
	app := discoveryFixture(t)
	for _, scope := range []struct {
		name   string
		budget int
	}{{"all", 720000}, {"orders", 50000}} {
		policy := app.mcpPolicy(mcpServeOptions(t, "--tools", scope.name))
		tools, e := app.mcpAdvertisedTools(policy, latestMCPProtocolVersion)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(rpcResult(1, map[string]any{"tools": tools}))
		t.Logf("%s: %d tools, %d bytes", scope.name, len(tools), len(raw))
		if len(raw) > scope.budget {
			t.Errorf("%s tools/list: %d > %d", scope.name, len(raw), scope.budget)
		}
		for _, tool := range tools {
			name := tool["name"].(string)
			if mcpAlwaysEnabled(name) {
				continue
			}
			raw, _ := json.Marshal(tool)
			if scope.name == "all" && len(raw) > 6500 {
				t.Logf("large %s %d", name, len(raw))
			}
			if len(raw) > 10000 {
				t.Errorf("%s: %d > 10000 bytes", name, len(raw))
			}
			if !regexp.MustCompile(`^[a-z0-9_]{1,52}$`).MatchString(name) {
				t.Error(name)
			}
			schema := tool["inputSchema"].(map[string]any)
			if schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Fatal(tool)
			}
			assertTypedSchema(t, name, schema)
			entry, _ := policy.index.Lookup(name)
			cmd, _ := app.mcpTool(name, policy)
			_, original, e := commandDiscoverySchema(cmd, true)
			if e != nil {
				t.Fatal(e)
			}
			reader, _, _ := commandDiscoverySchema(cmd, true)
			resolved, err := reader.Resolve(original, "")
			if err != nil {
				t.Fatal(err)
			}
			props, _ := resolved.Schema["properties"].(map[string]any)
			controls := mcpReadControls
			if entry.Kind == "write" {
				controls = mcpWriteControls
			}
			for _, control := range controls {
				if props[control] != nil {
					t.Errorf("%s argument collides with control %s", name, control)
				}
			}
			hints := tool["annotations"].(map[string]any)
			if hints["readOnlyHint"] != (entry.Kind == "read") || hints["idempotentHint"] != (entry.Kind == "read" || entry.Method == "PUT" || entry.Method == "DELETE" || entry.Idempotency == "required") {
				t.Fatal(tool)
			}
		}
	}
}

func assertTypedSchema(t *testing.T, name string, node map[string]any) {
	t.Helper()
	for key, value := range node {
		switch key {
		case "$ref", "$defs", "oneOf", "anyOf", "allOf", "not", "if", "discriminator", "example", "pattern":
			t.Errorf("%s leaked %s", name, key)
		}
		if strings.HasPrefix(key, "x-") {
			t.Errorf("%s leaked %s", name, key)
		}
		switch key {
		case "properties":
			for field, child := range value.(map[string]any) {
				assertTypedSchema(t, name+"."+field, child.(map[string]any))
			}
		case "items":
			if child, ok := value.(map[string]any); ok {
				assertTypedSchema(t, name+"[]", child)
			}
		}
	}
}

func TestMCPTypedDescriptions(t *testing.T) {
	app := discoveryFixture(t)
	policy := app.mcpPolicy(mcpServeOptions(t, "--tools", "all"))
	tools, e := app.mcpAdvertisedTools(policy, latestMCPProtocolVersion)
	if e != nil {
		t.Fatal(e)
	}
	for _, tool := range tools {
		if mcpAlwaysEnabled(fmt.Sprint(tool["name"])) {
			continue
		}
		description := fmt.Sprint(tool["description"])
		if description == "" {
			entry, _ := policy.index.Lookup(fmt.Sprint(tool["name"]))
			t.Errorf("empty description baseline is zero: %s (%s)", entry.ID, entry.OperationID)
		}
		seen := map[string]bool{}
		title := mcpDescriptionKey(fmt.Sprint(tool["title"]))
		for _, sentence := range strings.Split(description, ". ") {
			key := mcpDescriptionKey(sentence)
			if key != "" && (key == title || seen[key]) {
				t.Errorf("%s repeats title or sentence: %s", tool["name"], description)
			}
			seen[key] = true
		}
		if strings.Contains(description, "--") {
			t.Errorf("%s contains CLI flag text: %s", tool["name"], description)
		}
	}
}

func TestMCPTypedRequiredControls(t *testing.T) {
	app := discoveryFixture(t)
	for _, flags := range [][]string{{"--tools", "all"}, {"--tools", "all", "--allow-live"}, {"--tools", "all", "--read-only"}} {
		policy := app.mcpPolicy(mcpServeOptions(t, flags...))
		tools, e := app.mcpAdvertisedTools(policy, latestMCPProtocolVersion)
		if e != nil {
			t.Fatal(e)
		}
		for _, tool := range tools {
			name := tool["name"].(string)
			if mcpAlwaysEnabled(name) {
				continue
			}
			entry, _ := policy.index.Lookup(name)
			schema := tool["inputSchema"].(map[string]any)
			props := schema["properties"].(map[string]any)
			expected := map[string]bool{"select": true, "live": policy.allowLive, "all": entry.Kind == "read" && entry.Paginated, "wait_for": entry.Kind == "read" && entry.Get, "wait_seconds": entry.Kind == "read" && entry.Get, "dry_run": entry.Kind == "write", "confirm": entry.Kind == "write" && entry.ConfirmRequired()}
			for key, want := range expected {
				if (props[key] != nil) != want {
					t.Errorf("%s %s present=%t want=%t", name, key, props[key] != nil, want)
				}
			}
			if entry.Kind == "write" && props["idempotency_key"] == nil {
				t.Errorf("%s missing write key control", name)
			}
			if (containsString(mcpStrings(schema["required"]), "idempotency_key")) != (entry.Kind == "write" && entry.Idempotency == "required") {
				t.Errorf("%s key requirement mismatch", name)
			}
			if policy.readOnly && entry.Kind == "write" {
				t.Errorf("read-only lists %s", name)
			}
		}
	}
}

func TestMCPTypedArrayDescriptions(t *testing.T) {
	app := discoveryFixture(t)
	policy := app.mcpPolicy(mcpServeOptions(t, "--tools", "all"))
	for _, name := range []string{"api-keys.create", "signup"} {
		cmd, ok := app.Registry.ByName(name)
		if !ok {
			t.Fatal(name)
		}
		schema, e := schemaForMCPTool(cmd, policy)
		if e != nil {
			t.Fatal(e)
		}
		props := schema["properties"].(map[string]any)
		field := "scopes"
		if name == "signup" {
			field = "requested_capability"
		}
		items := props[field].(map[string]any)["items"].(map[string]any)
		text, _ := items["description"].(string)
		if text == "" {
			t.Fatalf("%s.%s lost item help", name, field)
		}
		if name != "signup" && !strings.Contains(text, "allowed values") {
			t.Fatalf("%s.%s lost enum guidance: %s", name, field, text)
		}
	}
}
