package cli

import (
	"encoding/json"
	"os"
	"sync"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

var mcpWireSchema = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	raw, err := os.ReadFile("testdata/mcp/schema.json")
	if err != nil {
		return nil, err
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	const uri = "https://test.invalid/mcp"
	if err := compiler.AddResource(uri, document); err != nil {
		return nil, err
	}
	schemas := map[string]*jsonschema.Schema{}
	for _, name := range []string{"JSONRPCMessage", "ServerRequest", "ServerNotification", "InitializeResult", "ListToolsResult", "CallToolResult"} {
		schema, err := compiler.Compile(uri + "#/$defs/" + name)
		if err != nil {
			return nil, err
		}
		schemas[name] = schema
	}
	return schemas, nil
})

func assertMCPWireMessage(t *testing.T, message map[string]any) {
	t.Helper()
	schemas, err := mcpWireSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		t.Fatal(err)
	}
	if err := schemas["JSONRPCMessage"].Validate(normalized); err != nil {
		t.Errorf("invalid MCP output %s: %v", raw, err)
	}
	name, value := "", normalized
	if _, ok := message["method"]; ok {
		name = "ServerNotification"
		if _, ok := message["id"]; ok {
			name = "ServerRequest"
		}
	} else if result, ok := message["result"].(map[string]any); ok {
		value = normalized.(map[string]any)["result"]
		for field, definition := range map[string]string{"protocolVersion": "InitializeResult", "tools": "ListToolsResult", "content": "CallToolResult"} {
			if _, ok := result[field]; ok {
				name = definition
			}
		}
	}
	if name != "" {
		if err := schemas[name].Validate(value); err != nil {
			t.Errorf("invalid %s output: %v", name, err)
		}
	}

}

func TestMCPProtocolIDsAndRecovery(t *testing.T) {
	app, _, _ := testApp(t, "")
	conn := startMCPTestConn(t, app, defaultOptions())
	for _, id := range []string{`1.5`, `1e3`, `1.0`, `{}`, `[]`, `true`, `null`, `9007199254740992`, `-9007199254740992`, `99999999999999999999999999`} {
		conn.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
		message := conn.next()
		if _, exists := message["id"]; exists || lookupPathValue(message, "error.code") != float64(-32600) {
			t.Fatalf("id %s: %#v", id, message)
		}
	}
	for _, id := range []string{`"request"`, `0`, `-9007199254740991`, `9007199254740991`} {
		conn.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
		if message := conn.next(); message["error"] != nil {
			t.Fatalf("id %s: %#v", id, message)
		}
	}
	for _, line := range []string{"{", "{\"jsonrpc\":\"2.0\",\"id\":\"\xff\",\"method\":\"ping\"}"} {
		conn.send(line)
		message := conn.next()
		if _, exists := message["id"]; exists || lookupPathValue(message, "error.code") != float64(-32700) {
			t.Fatalf("parse error: %#v", message)
		}
		conn.send(`{"jsonrpc":"2.0","id":"recovery","method":"ping"}`)
		if message := conn.next(); message["id"] != "recovery" || message["error"] != nil {
			t.Fatalf("recovery: %#v", message)
		}
	}
	conn.close()
}

func TestMCPProtocolProgressTokens(t *testing.T) {
	app, _, _ := testApp(t, "")
	conn := startMCPTestConn(t, app, defaultOptions())
	conn.initialize(`{}`)
	for _, token := range []string{`{}`, `[]`, `false`, `null`, `1.5`, `1e3`, `1.0`, `9007199254740992`} {
		conn.send(`{"jsonrpc":"2.0","id":"token","method":"tools/call","params":{"name":"flint_help","_meta":{"progressToken":` + token + `}}}`)
		message := conn.next()
		if message["id"] != "token" || lookupPathValue(message, "error.code") != float64(-32602) {
			t.Fatalf("token %s: %#v", token, message)
		}
	}
	for _, token := range []string{`"progress"`, `0`, `9007199254740991`, `-9007199254740991`} {
		conn.send(`{"jsonrpc":"2.0","id":"token","method":"ping","params":{"_meta":{"progressToken":` + token + `}}}`)
		if message := conn.next(); message["error"] != nil {
			t.Fatalf("token %s: %#v", token, message)
		}
	}
	conn.close()
}

func TestMCPProtocolToolModes(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, flags := range [][]string{nil, {"--tools", "all"}, {"--tools", "customers"}, {"--allow-live"}, {"--allow-live", "--allow-destructive"}} {
		conn := startMCPTestConn(t, app, mcpServeOptions(t, flags...))
		conn.initialize(`{}`)
		conn.send(`{"jsonrpc":"2.0","id":"list","method":"tools/list"}`)
		if message := conn.next(); message["error"] != nil {
			t.Fatal(message)
		}
		conn.close()
	}
}
