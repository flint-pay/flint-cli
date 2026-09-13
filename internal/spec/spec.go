package spec

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// OpenAPI is the public API contract snapshot used to generate CLI schemas and coverage checks.
//
//go:embed openapi.json
var OpenAPI []byte

// APIVersion returns the dated API compatibility version embedded in the
// exact public contract snapshot shipped with the binary.
func APIVersion() string {
	return embeddedAPIVersion()
}

var embeddedAPIVersion = sync.OnceValue(func() string {
	var document struct {
		Version string `json:"x-flint-api-version"`
	}
	if err := json.Unmarshal(OpenAPI, &document); err != nil || document.Version == "" {
		panic("embedded Flint OpenAPI snapshot has no x-flint-api-version")
	}
	return document.Version
})
