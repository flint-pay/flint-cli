package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHumanInitChecksAndNextSteps(t *testing.T) {
	value := map[string]any{"data": map[string]any{
		"checks":         []map[string]any{{"name": "config", "status": "pass", "profile": "default"}, {"name": "connectivity", "status": "fail", "fix": "Check the local server."}},
		"project_config": map[string]any{"status": "created", "path": ".flint/config.json"},
		"next_steps":     []map[string]string{{"workflow": "hosted_checkout", "command": "flint checkout-sessions create --help"}},
	}}
	before, _ := json.Marshal(value)
	var out bytes.Buffer
	if !renderInitHuman(&out, value, time.Now()) {
		t.Fatal("init payload was not recognized")
	}
	for _, want := range []string{"✓ Config loaded (profile: default)", "✗ Connectivity", "Next: Check the local server.", ".flint/config.json", "Next steps", "Workflow", "flint checkout-sessions create --help"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	after, _ := json.Marshal(value)
	if !bytes.Equal(before, after) {
		t.Fatal("human rendering changed init data")
	}
	if renderInitHuman(&out, map[string]any{"data": map[string]any{"authenticated": false}}, time.Now()) {
		t.Fatal("unauthenticated guidance should use generic rendering")
	}
}
