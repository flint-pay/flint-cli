package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestStartupSnapshotParity(t *testing.T) {
	index := Startup()
	if index.SHA256 != fmt.Sprintf("%x", sha256.Sum256(OpenAPI)) {
		t.Fatal("startup snapshot hash is stale; run go generate ./internal/cli")
	}
	var doc struct {
		Paths      map[string]map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, methods := range doc.Paths {
		for _, expected := range methods {
			id, _ := expected["operationId"].(string)
			if id == "" {
				continue
			}
			count++
			var got map[string]any
			if !index.Operation(id, &got) || !reflect.DeepEqual(got, expected) {
				t.Fatalf("operation %s range differs from full snapshot", id)
			}
		}
	}
	if count != len(index.Operations) {
		t.Fatal("operation set differs")
	}
	if len(doc.Components.Schemas) != len(index.Schemas) {
		t.Fatal("schema set differs")
	}
	for name, expected := range doc.Components.Schemas {
		var got map[string]any
		if !index.Schema(name, &got) || !reflect.DeepEqual(got, expected) {
			t.Fatalf("schema %s range differs from full snapshot", name)
		}
	}
	regenerated, err := GenerateStartup(index.Commands, index.LocalCommands)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(startupJSON), regenerated) {
		t.Fatal("startup metadata is stale; run go generate ./internal/cli")
	}
}
