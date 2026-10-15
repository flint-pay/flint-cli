package spec

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// OpenAPI is the public API contract snapshot used to generate CLI schemas and coverage checks.
// Source: flint-pay/flint@ef924c4fb76580e3a7bb50dd7bcbe9bad3fa5888
// SHA-256: da782a4138b4497b7a7bdac996c8ecfd07bad1b2dfe584940a3390b64a5700b1
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
