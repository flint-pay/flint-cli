package cli

import (
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestCompletionTreeFlagsAndEnums(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	for _, tc := range []struct {
		words []string
		want  []string
		exact bool
	}{
		{[]string{"ord"}, []string{"orders"}, true},
		{[]string{"orders", "dis"}, []string{"discounts"}, false},
		{[]string{"webhook-endpoints", "update", "whep_test", "--add-ena"}, []string{"--add-enabled-events"}, true},
		{[]string{"api-keys", "create", "--access", "r"}, []string{"read_only", "restricted"}, true},
		{[]string{"api-keys", "create", "--access", "=", "r"}, []string{"--access=read_only", "--access=restricted"}, true},
		{[]string{"api-keys", "create", "--access=r"}, []string{"--access=read_only", "--access=restricted"}, true},
		{[]string{"webhook-endpoints", "create", "--signing-methods", "jwt,h"}, []string{"jwt,hmac"}, true},
		{[]string{"--profile", "work", "orders", "l"}, []string{"line-items", "list"}, false},
		{[]string{"completion", "f"}, []string{"fish"}, true},
		{[]string{"orders", "get", "@last"}, nil, true},
		{[]string{"orders", "get", "ord_test", "--", "--"}, nil, true},
		{[]string{"customers", "list", "--output", "j"}, []string{"json"}, true},
	} {
		got := app.completionCandidates(tc.words)
		if tc.exact && !slices.Equal(got, tc.want) {
			t.Fatalf("%v got=%v want=%v", tc.words, got, tc.want)
		}
		for _, want := range tc.want {
			if !slices.Contains(got, want) {
				t.Fatalf("%v missing %s in %v", tc.words, want, got)
			}
		}
		if slices.Contains(got, "") || slices.Contains(got, "__complete") {
			t.Fatal(got)
		}
	}
}

func TestCompletionScriptsAndOfflineEndpoint(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		app, out, stderr := testApp(t, "http://127.0.0.1:9")
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("completion accessed network"); return nil, nil })}
		if exit := app.Run([]string{"completion", shell}); exit != 0 || !strings.Contains(out.String(), "flint __complete") {
			t.Fatal(shell, exit, out, stderr)
		}
		if _, err := exec.LookPath(shell); err == nil {
			cmd := exec.Command(shell, "-n")
			cmd.Stdin = strings.NewReader(out.String())
			if raw, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(shell, err, string(raw))
			}
		}
		out.Reset()
		if exit := app.Run([]string{"__complete", "api-keys", "create", "--access", "r"}); exit != 0 || out.String() != "read_only\nrestricted\n" {
			t.Fatal(exit, out, stderr)
		}
	}
	app, out, _ := testApp(t, "http://127.0.0.1:9")
	exit, result := runJSON(t, app, out, "completion", "bash")
	if value, _ := lookupPath(result, "data.script"); exit != 0 || value != completionScripts["bash"] {
		t.Fatal(exit, result)
	}
	if _, ok := app.Registry.ByName("__complete"); ok {
		t.Fatal("hidden endpoint entered public tree")
	}
}

func TestCompletionBashSplitInlineReplacement(t *testing.T) {
	script := completionScripts["bash"] + `
flint() { printf '%s\n' --access=read_only --access=restricted; }
COMP_WORDS=(flint api-keys create --access = r)
COMP_CWORD=5
_flint_complete
printf '%s\n' "${COMPREPLY[@]}"
`
	cmd := exec.Command("bash", "-c", script)
	got, err := cmd.CombinedOutput()
	if err != nil || string(got) != "read_only\nrestricted\n" {
		t.Fatal(err, string(got))
	}
	if !strings.Contains(completionScripts["fish"], `$words "$current"`) {
		t.Fatal("fish drops empty token")
	}
}
