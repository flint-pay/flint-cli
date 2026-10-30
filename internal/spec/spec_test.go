package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type snapshotProvenance struct {
	SourceRepository   string `json:"source_repository"`
	SourceCommit       string `json:"source_commit"`
	SourcePath         string `json:"source_path"`
	SHA256             string `json:"sha256"`
	OperationSetSHA256 string `json:"operation_set_sha256"`
	OperationCount     int    `json:"operation_count"`
	FreshnessPolicy    string `json:"freshness_policy"`
}

func readProvenance(t *testing.T) snapshotProvenance {
	t.Helper()
	raw, err := os.ReadFile("provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var provenance snapshotProvenance
	if err := json.Unmarshal(raw, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.SourceRepository != "flint-pay/flint" ||
		provenance.SourcePath != "web/apps/developers/src/generated/openapi.json" ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(provenance.SourceCommit) ||
		strings.TrimSpace(provenance.FreshnessPolicy) == "" {
		t.Fatal("provenance must pin a reviewed Flint export and explain intentional lag")
	}
	return provenance
}

func snapshotHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Canonical operations are sorted METHOD + space + path + space + operationId,
// each terminated by a newline. Include every OpenAPI HTTP method so additions
// cannot silently escape the pinned operation-set check.
func snapshotOperations(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var operations []string
	ids := map[string]bool{}
	for path, item := range doc.Paths {
		for method, rawOperation := range item {
			switch method {
			case "get", "put", "post", "delete", "options", "head", "patch", "trace":
			default:
				continue
			}
			var operation struct {
				ID string `json:"operationId"`
			}
			if err := json.Unmarshal(rawOperation, &operation); err != nil {
				t.Fatal(err)
			}
			if operation.ID == "" || ids[operation.ID] {
				t.Fatalf("%s %s has an empty or duplicate operationId %q", method, path, operation.ID)
			}
			ids[operation.ID] = true
			operations = append(operations, strings.ToUpper(method)+" "+path+" "+operation.ID)
		}
	}
	slices.Sort(operations)
	return operations
}

func TestSnapshotProvenance(t *testing.T) {
	provenance := readProvenance(t)
	if got := snapshotHash(OpenAPI); got != provenance.SHA256 {
		t.Fatalf("snapshot SHA-256 = %s, pinned reviewed export = %s; refresh provenance with the export", got, provenance.SHA256)
	}
	operations := snapshotOperations(t, OpenAPI)
	if len(operations) != provenance.OperationCount {
		t.Fatalf("snapshot has %d operations, pinned export has %d", len(operations), provenance.OperationCount)
	}
	if got := snapshotHash([]byte(strings.Join(operations, "\n") + "\n")); got != provenance.OperationSetSHA256 {
		t.Fatalf("operation-set SHA-256 = %s, pinned export = %s", got, provenance.OperationSetSHA256)
	}
}

// FLINT_SPEC_REPO optionally verifies the recorded provenance against a local
// monorepo object database. git show reads the pinned commit, never the working
// tree or a moving branch, and never fetches from a remote or production server.
func TestSnapshotMatchesPinnedExport(t *testing.T) {
	repo := os.Getenv("FLINT_SPEC_REPO")
	if repo == "" {
		t.Skip("set FLINT_SPEC_REPO to compare against a local Flint monorepo")
	}
	provenance := readProvenance(t)
	export, err := exec.Command("git", "-C", repo, "show", provenance.SourceCommit+":"+provenance.SourcePath).Output()
	if err != nil {
		t.Fatal(fmt.Errorf("read pinned reviewed export: %w", err))
	}
	if !bytes.Equal(export, OpenAPI) {
		t.Fatalf("snapshot differs from %s@%s:%s", provenance.SourceRepository, provenance.SourceCommit, provenance.SourcePath)
	}
	if !slices.Equal(snapshotOperations(t, export), snapshotOperations(t, OpenAPI)) {
		t.Fatal("snapshot operation set differs from the pinned reviewed export")
	}
}
