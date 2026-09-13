package contract

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/flint-pay/flint-cli/internal/cli"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

//go:embed coverage_policy.json
var coveragePolicyJSON []byte

type CoverageEntry struct {
	Method         string   `json:"method"`
	Path           string   `json:"path"`
	OperationID    string   `json:"operation_id"`
	Classification string   `json:"classification"`
	Commands       []string `json:"commands,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

func Generate() ([]CoverageEntry, error) {
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(apispec.OpenAPI, &doc); err != nil {
		return nil, err
	}
	var policy []CoverageEntry
	if err := json.Unmarshal(coveragePolicyJSON, &policy); err != nil {
		return nil, fmt.Errorf("decode coverage policy: %w", err)
	}
	policyByOperation := make(map[string]CoverageEntry, len(policy))
	for _, entry := range policy {
		if entry.OperationID == "" {
			return nil, fmt.Errorf("coverage policy entry %s %s has no operation_id", entry.Method, entry.Path)
		}
		if _, exists := policyByOperation[entry.OperationID]; exists {
			return nil, fmt.Errorf("coverage policy contains duplicate operation_id %s", entry.OperationID)
		}
		policyByOperation[entry.OperationID] = entry
	}
	commandsByOperation := map[string][]string{}
	for _, cmd := range cli.NewRegistry().Commands {
		if cmd.OperationID != "" {
			commandsByOperation[cmd.OperationID] = append(commandsByOperation[cmd.OperationID], cmd.CanonicalName)
		}
	}
	var entries []CoverageEntry
	seenPolicy := make(map[string]bool, len(policy))
	for path, methods := range doc.Paths {
		for method, operation := range methods {
			upper := strings.ToUpper(method)
			if upper != "GET" && upper != "POST" && upper != "PUT" && upper != "PATCH" && upper != "DELETE" {
				continue
			}
			if operation.OperationID == "" {
				return nil, fmt.Errorf("%s %s has no operationId", upper, path)
			}
			commands := dedupe(commandsByOperation[operation.OperationID])
			decision, exists := policyByOperation[operation.OperationID]
			if !exists {
				return nil, fmt.Errorf("%s %s (%s) is unclassified in coverage_policy.json", upper, path, operation.OperationID)
			}
			if decision.Method != upper || decision.Path != path {
				return nil, fmt.Errorf("coverage policy for %s describes %s %s, OpenAPI describes %s %s", operation.OperationID, decision.Method, decision.Path, upper, path)
			}
			seenPolicy[operation.OperationID] = true
			entry := CoverageEntry{
				Method:         upper,
				Path:           path,
				OperationID:    operation.OperationID,
				Classification: decision.Classification,
				Reason:         decision.Reason,
			}
			switch entry.Classification {
			case "workflow", "resource":
				if len(commands) == 0 {
					return nil, fmt.Errorf("%s is classified %s but has no registered command", operation.OperationID, entry.Classification)
				}
				entry.Commands = commands
			case "raw-api-only", "deferred":
				if entry.Reason == "" {
					return nil, fmt.Errorf("%s classification %s requires a reason", operation.OperationID, entry.Classification)
				}
				if len(commands) > 0 {
					return nil, fmt.Errorf("%s is classified %s but has registered commands %s", operation.OperationID, entry.Classification, strings.Join(commands, ", "))
				}
			default:
				return nil, fmt.Errorf("%s has invalid coverage classification %q", operation.OperationID, entry.Classification)
			}
			entries = append(entries, entry)
		}
	}
	for _, decision := range policy {
		if !seenPolicy[decision.OperationID] {
			return nil, fmt.Errorf("coverage policy references missing OpenAPI operation %s", decision.OperationID)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path == entries[j].Path {
			return entries[i].Method < entries[j].Method
		}
		return entries[i].Path < entries[j].Path
	})
	return entries, nil
}
func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
