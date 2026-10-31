package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/flint-pay/flint-cli/internal/opindex"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

func TestStartupRegistryParity(t *testing.T) {
	// Rebuild with the full snapshot and the real command/argument generators.
	expected := newRegistryFromSnapshot(true)
	got := indexedRegistry(true)
	left, _ := json.Marshal(expected.Commands)
	right, _ := json.Marshal(got.Commands)
	if !bytes.Equal(left, right) {
		t.Fatal("generated command catalog differs; run go generate ./internal/cli")
	}
	for i, cmd := range expected.Commands {
		indexed := got.Commands[i]
		if cmd.Render != indexed.Render || cmd.MCPHidden != indexed.MCPHidden {
			t.Fatalf("%s lost internal metadata", cmd.Name)
		}
		for n, arg := range cmd.Arguments {
			if arg.bodyGenerated != indexed.Arguments[n].bodyGenerated {
				t.Fatalf("%s %s lost generated marker", cmd.Name, arg.Name)
			}
		}
	}
	file := filepath.Join(t.TempDir(), "startup.json")
	if err := GenerateStartupIndex(file); err != nil {
		t.Fatal(err)
	}
	regenerated, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := os.ReadFile("../spec/startup.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(regenerated, checked) {
		t.Fatal("startup generation is not reproducible; run go generate ./internal/cli")
	}
	// Registries are per App: parser/test changes cannot corrupt later callers.
	got.Commands[0].Description = "changed"
	fresh := indexedRegistry(true)
	if fresh.Commands[0].Description == "changed" {
		t.Fatal("registry instances share mutable commands")
	}
}

func TestStartupOperationIndexParity(t *testing.T) {
	registry := NewRegistry()
	var commands []opindex.Command
	for _, cmd := range registry.Commands {
		commands = append(commands, opindex.Command{Name: cmd.Name, CanonicalName: cmd.CanonicalName, Title: cmd.Description, Method: cmd.Method, Path: cmd.APIPath, OperationID: cmd.OperationID, Aliases: cmd.Aliases, Local: cmd.Local, MCPHidden: cmd.MCPHidden && cmd.Name != "trigger", Mutation: cmd.Mutation, Get: cmd.Get, Stream: cmd.Stream, Paginated: cmd.Supports.Pagination})
	}
	full, err := opindex.New(commands, apispec.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := opindex.New(commands, apispec.Startup().Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full, compact) {
		t.Fatal("compact operation index differs from full snapshot")
	}
}

func TestStartupLazyInvocation(t *testing.T) {
	if os.Getenv("FLINT_STARTUP_TEST_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStartupLazyInvocation$", "-test.v")
		cmd.Env = append(os.Environ(), "FLINT_STARTUP_TEST_CHILD=1", "FLINT_BASE_URL=http://127.0.0.1:9")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated lazy invocation: %v\n%s", err, out)
		}
		return
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FLINT_API_KEY", "flint_test_test")
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	t.Setenv("FLINT_PROFILE", "")
	t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
	app := New(BuildInfo{Version: "test"})
	app.Stdout, app.Stderr = io.Discard, io.Discard
	app.IsTTY = func() bool { return false }
	for _, args := range [][]string{{"version"}, {"--help"}, {"customers", "list", "--help"}} {
		if code := app.Run(args); code != 0 {
			t.Fatalf("%v exit %d", args, code)
		}
	}
	if fullSnapshotLoaded.Load() {
		t.Fatal("help/version decoded full snapshot")
	}
	apiCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			io.WriteString(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","mode":"test","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["customers.read"]}}`)
		} else if r.URL.Path == "/v1/customers" {
			apiCalls++
			io.WriteString(w, `{"data":[]}`)
		} else {
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("FLINT_BASE_URL", server.URL)
	if code := app.Run([]string{"customers", "list", "--output", "json"}); code != 0 {
		t.Fatalf("customers list exit %d", code)
	}
	if apiCalls != 1 {
		t.Fatalf("API calls %d", apiCalls)
	}
	if fullSnapshotLoaded.Load() {
		t.Fatal("API invocation decoded full snapshot")
	}
	indexedOperations.Range(func(key, value any) bool {
		if key != "listCustomers" && key != "" {
			t.Errorf("decoded unrelated operation %v", key)
		}
		return true
	})
	schemaComponents.Range(func(key, value any) bool {
		if strings.HasPrefix(key.(string), "Webhook_") {
			t.Errorf("decoded unrelated event schema %v", key)
		}
		return true
	})
	// Raw route matching uses the compact routing index and the matched op only.
	raw, ok := matchPublicOperation("GET", "/v1/customers?email=local%40example.com")
	if !ok || raw.OperationID != "listCustomers" || len(raw.Parameters) == 0 {
		t.Fatal("raw route lost operation schemas")
	}
	if fullSnapshotLoaded.Load() {
		t.Fatal("raw routing decoded full snapshot")
	}
	// Exercise the same lazy resolver with discriminator branches and validation.
	create, _ := app.Registry.ByName("api-keys.create")
	union := commandRequestUnion(create)
	if union.discriminator != "access" || union.view.Variants == nil {
		t.Fatalf("lazy discriminator lost: %#v", union)
	}
	if _, e := requestBodySchema(create); e != nil {
		t.Fatal(e)
	}
	if fullSnapshotLoaded.Load() {
		t.Fatal("variant schema decoded full snapshot")
	}
}

func TestStartupCommandCloneIsolation(t *testing.T) {
	fixture := func() *Command {
		explode := true
		return &Command{
			Name: "fixture", Path: []string{"flint", "fixture"}, CanonicalPath: []string{"flint", "fixture"},
			Aliases: []string{"alias"}, AliasPaths: [][]string{{"flint", "alias"}}, Examples: []string{"flint fixture"},
			Security:  []map[string][]string{{"BearerAuth": {"customers.read"}}, nil},
			Arguments: []Arg{{Name: "choice", Enum: []string{"a"}, Variants: []string{"a"}, RequiredFor: []string{"a"}, Explode: &explode, bodyGenerated: true}},
			Render:    "fixture", MCPHidden: true,
		}
	}
	source := fixture()
	clone := cloneStartupCommand(source)
	if !reflect.DeepEqual(clone, source) {
		t.Fatal("clone lost metadata")
	}
	mutateStartupValue(reflect.ValueOf(clone))
	if !reflect.DeepEqual(source, fixture()) {
		t.Fatal("nested command mutation reached its source")
	}
	for _, empty := range []*Command{{}, {Arguments: []Arg{}, Aliases: []string{}, Security: []map[string][]string{}}} {
		if !reflect.DeepEqual(cloneStartupCommand(empty), empty) {
			t.Fatal("clone changed nil/empty fields")
		}
	}
}

func TestStartupCatalogConcurrentIsolation(t *testing.T) {
	fullBefore, _ := json.Marshal(startupCommands())
	localBefore, _ := json.Marshal(startupLocalCommands())
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			registry := indexedRegistry(worker%2 == 0)
			for _, cmd := range registry.Commands {
				mutateStartupValue(reflect.ValueOf(cmd))
			}
		}()
	}
	workers.Wait()
	fullAfter, _ := json.Marshal(startupCommands())
	localAfter, _ := json.Marshal(startupLocalCommands())
	if !bytes.Equal(fullBefore, fullAfter) || !bytes.Equal(localBefore, localAfter) {
		t.Fatal("registry mutation changed cached catalog")
	}
	fresh, _ := json.Marshal(indexedRegistry(true).Commands)
	if !bytes.Equal(fullBefore, fresh) {
		t.Fatal("registry mutation changed subsequent App")
	}
}

// Mutate every exported nested value, rather than just checking top-level
// pointers. Map insertion also detects shared maps whose values are empty.
func mutateStartupValue(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			mutateStartupValue(value.Elem())
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			mutateStartupValue(value.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			mutateStartupValue(value.Index(i))
		}
	case reflect.Map:
		if value.IsNil() {
			return
		}
		for _, key := range value.MapKeys() {
			item := reflect.New(value.Type().Elem()).Elem()
			item.Set(value.MapIndex(key))
			mutateStartupValue(item)
			value.SetMapIndex(key, item)
		}
		value.SetMapIndex(reflect.ValueOf("mutated").Convert(value.Type().Key()), reflect.Zero(value.Type().Elem()))
	case reflect.String:
		if value.CanSet() {
			value.SetString("mutated")
		}
	case reflect.Bool:
		if value.CanSet() {
			value.SetBool(!value.Bool())
		}
	}
}
