package cli

import (
	"bytes"
	"encoding/json"
	testflags "flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

var updateHelpTree = testflags.Bool("update-help-tree", false, "Update command help goldens")

func TestCommandTreeResolution(t *testing.T) {
	registry := NewRegistry()
	tree := registry.commandTree()
	for _, cmd := range tree.Commands {
		for _, words := range [][]string{cmd.Path[1:], {cmd.Name}, append(cmd.Path[1:], "fixture")} {
			resolved := tree.Resolve(words)
			if resolved.Command != cmd {
				t.Fatalf("resolve %v: %+v", words, resolved)
			}
		}
	}
	for _, group := range tree.Groups {
		resolved := tree.Resolve(group.Words)
		// A CLI-owned command such as help can also prefix a command group.
		if resolved.Command == nil && resolved.Group != group {
			t.Fatalf("resolve group %s", group.Name)
		}
	}
	for alias, name := range map[string]string{"login": "auth.login", "logout": "auth.logout", "reauth": "auth.reauth", "whoami": "auth.status"} {
		if resolution := tree.Resolve([]string{alias}); resolution.Command == nil || resolution.Command.Name != name {
			t.Fatal(alias, resolution)
		}
	}
	for _, words := range [][]string{{"checkout"}, {"payment"}, {"custmers"}, {"customers", "lst"}, {"customer"}} {
		input := strings.Join(words, " ")
		for _, suggestion := range tree.Suggestions(words) {
			if suggestion == input {
				t.Fatal(input)
			}
			resolution := tree.Resolve(strings.Fields(suggestion))
			if resolution.Command == nil && resolution.Group == nil {
				t.Fatal(suggestion)
			}
		}
	}
}

func TestHelpMatchesParser(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	tree := app.Registry.commandTree()
	cases := [][]string{{"customers.create"}, {"flint", "customers", "create"}, {"orders", "get", "ord_123"}, {"whoami"}}
	for _, cmd := range tree.Commands {
		cases = append(cases, cmd.Path[1:])
	}
	for _, group := range tree.Groups {
		cases = append(cases, group.Words)
	}
	for _, words := range cases {
		for _, output := range []string{"human", "json"} {
			stdout.Reset()
			stderr.Reset()
			left := append(append([]string{"help"}, words...), "--output", output)
			if code := app.Run(left); code != 0 {
				t.Fatalf("%v: %d %s", left, code, stderr.String())
			}
			expected := stdout.String()
			stdout.Reset()
			stderr.Reset()
			right := append(append([]string{}, words...), "--help", "--output", output)
			if code := app.Run(right); code != 0 {
				t.Fatalf("%v: %d %s", right, code, stderr.String())
			}
			if stdout.String() != expected {
				t.Fatalf("help differs for %v (%s)\nleft: %s\nright: %s", words, output, expected, stdout.String())
			}
		}
	}
}

func TestHelpTreeGoldens(t *testing.T) {
	cases := map[string][]string{"root": {}, "all": {"help", "--all"}, "root-json": {"help", "--output", "json"}, "webhook-deliveries": {"help", "webhook-deliveries"}, "api-keys-create": {"help", "api-keys", "create"}, "me": {"help", "me"}}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
			if code := app.Run(args); code != 0 {
				t.Fatalf("%v: %d %s", args, code, stderr.String())
			}
			path := filepath.Join("testdata", "help-"+name+".golden")
			if *updateHelpTree {
				if err := os.WriteFile(path, stdout.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
			}
			expected, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(expected, stdout.Bytes()) {
				t.Fatalf("%s changed; review and update with -update-help-tree", path)
			}
		})
	}
}

func TestHelpTreeSectionsAndCatalog(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	tree := app.Registry.commandTree()
	actual := map[string][]string{}
	for _, group := range tree.Groups {
		if len(group.Words) == 1 && group.Commands[0].OperationID != "" && !commandOwned(group.Commands[0]) {
			actual[group.Section] = append(actual[group.Section], group.Name)
		}
	}
	for _, section := range actual {
		sort.Strings(section)
	}
	expectedDeveloper := []string{"api-request-logs", "demo-sessions", "openapi", "partner-app-installs", "partner-apps", "resource-timelines", "test-clocks"}
	if !reflect.DeepEqual(actual[sectionDeveloper], expectedDeveloper) {
		t.Fatal(actual[sectionDeveloper])
	}
	if !reflect.DeepEqual(actual[sectionBuyer], []string{"me"}) {
		t.Fatal(actual[sectionBuyer])
	}
	for _, cmd := range tree.Commands {
		if cmd.Section == "" || cmd.Audience == "" || cmd.Kind == "" {
			t.Fatal(cmd.Name)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if exit := app.Run(nil); exit != 0 {
		t.Fatal(stderr.String())
	}
	expected := map[string][]string{}
	for _, section := range rootHelpSections(tree, false) {
		for _, entry := range section["entries"].([]map[string]any) {
			id := section["id"].(string)
			expected[id] = append(expected[id], entry["name"].(string))
		}
	}
	human := map[string][]string{}
	active := ""
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case line == "Developer tools:":
			active = sectionDeveloper
		case line == "Merchant resources:":
			active = sectionMerchant
		case strings.HasPrefix(line, "Buyer session ("):
			active = sectionBuyer
		case line == "Scripts and AI agents:":
			active = ""
		}
		if active == "" || !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "  Shortcuts:") {
			continue
		}
		words := strings.Fields(line)
		if active != sectionMerchant && len(words) > 1 {
			words = words[:1]
		}
		human[active] = append(human[active], words...)
	}
	if !reflect.DeepEqual(human, expected) {
		t.Fatalf("human/index sections differ: %v vs %v", human, expected)
	}

	stdout.Reset()
	if exit := app.Run([]string{"help", "--all", "--output", "json"}); exit != 0 {
		t.Fatal(stderr.String())
	}
	var data struct {
		Data struct {
			Sections []struct {
				Entries []map[string]any `json:"entries"`
			} `json:"sections"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, section := range data.Data.Sections {
		count += len(section.Entries)
	}
	if count != len(tree.Commands) {
		t.Fatal(count, len(tree.Commands))
	}
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range append(append([]string{}, helpStartHere...), "flint orders create --input order.json", "flint checkout-sessions create --quick-pay-name T-shirt --amount 2500 --currency USD --open", "flint payment-intents create --amount 2500 --currency USD --payment-option card") {
		resolution := tree.Resolve(strings.Fields(example))
		if resolution.Command == nil {
			t.Fatal(example)
		}
		entry, _ := index.Lookup(resolution.Command.Name)
		if missing := index.MissingScopes(entry, embeddedInitialCLIScopes(), true); len(missing) > 0 {
			t.Fatalf("%s needs extra scopes %v", example, missing)
		}
	}
}

func TestCapabilityFlagsUnique(t *testing.T) {
	app, stdout, _ := testApp(t, "http://127.0.0.1:1")
	for _, cmd := range app.Registry.commandTree().Commands {
		stdout.Reset()
		app.printCommandHelp(cmd)
		_, capabilities, found := strings.Cut(stdout.String(), "Capability flags:\n")
		if !found {
			continue
		}
		capabilities, _, _ = strings.Cut(capabilities, "\n")
		seen := map[string]bool{}
		for _, word := range strings.Fields(capabilities) {
			if strings.HasPrefix(word, "--") {
				key, _, _ := strings.Cut(word, "=")
				if seen[key] {
					t.Errorf("%s repeats %s: %s", cmd.Name, key, capabilities)
				}
				seen[key] = true
			}
		}
	}
}

func TestCommandTreeConcurrentResolution(t *testing.T) {
	registry := NewRegistry()
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := 0; n < 10; n++ {
				resolution := registry.resolveWords([]string{"customers", "get", "cus_fixture"})
				if resolution.Command == nil || resolution.Command.Audience != "merchant" {
					t.Error("missing command metadata")
				}
			}
		}()
	}
	workers.Wait()
}
