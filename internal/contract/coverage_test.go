package contract

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestCoverageManifestMatchesPublicOpenAPI(t *testing.T) {
	generated, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var checkedIn []CoverageEntry
	if err := json.Unmarshal(raw, &checkedIn); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generated, checkedIn) {
		t.Fatal("coverage.json is stale; run go run ./cmd/flint-coverage > coverage.json")
	}
	for _, entry := range checkedIn {
		switch entry.Classification {
		case "workflow", "resource":
		default:
			t.Errorf("%s %s has invalid classification %q", entry.Method, entry.Path, entry.Classification)
		}
		if (entry.Classification == "raw-api-only" || entry.Classification == "deferred") && entry.Reason == "" {
			t.Errorf("%s %s lacks a classification reason", entry.Method, entry.Path)
		}
	}
}
