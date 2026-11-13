package spec

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"sync"
)

// OpenAPI is the public API contract snapshot used to generate CLI schemas and coverage checks.
// provenance.json pins its reviewed source export; TestSnapshotProvenance validates
// the actual snapshot bytes and exact operation set without network access.
//
//go:embed openapi.json
var OpenAPI []byte

// APIVersion returns the dated API compatibility version embedded in the
// exact public contract snapshot shipped with the binary.
func APIVersion() string {
	return embeddedAPIVersion()
}

var embeddedAPIVersion = sync.OnceValue(func() string {
	var version string
	if err := decodeMetadata("x-flint-api-version", &version); err != nil || version == "" {
		panic("embedded Flint OpenAPI snapshot has no x-flint-api-version")
	}
	return version
})

// These extension names are unique in the embedded, validated snapshot. Decode
// one value rather than traversing its large components and paths a second time.
func decodeMetadata(name string, target any) error {
	key := []byte(`"` + name + `":`)
	index := bytes.LastIndex(OpenAPI, key)
	if index < 0 {
		return &json.SyntaxError{}
	}
	return json.NewDecoder(bytes.NewReader(OpenAPI[index+len(key):])).Decode(target)
}
