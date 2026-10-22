package spec

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"sync"
)

// OpenAPI is the public API contract snapshot used to generate CLI schemas and coverage checks.
// Source: flint-pay/flint@b998a315ad29c278cce9034545c7eb4b2d07bdbd
// SHA-256: 151b8c3f11a7b80612bd400f37c1155ec483128e7758d9b608e3ec8df8451396
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

// BootstrapScopes reads only the bootstrap scope extension. The full schema
// document is loaded later by commands that need API operations or schemas.
func BootstrapScopes() []string { return append([]string(nil), bootstrapScopes()...) }

var bootstrapScopes = sync.OnceValue(func() []string {
	var scopes []string
	if err := decodeMetadata("x-flint-cli-bootstrap-scopes", &scopes); err != nil || len(scopes) == 0 {
		panic("embedded Flint OpenAPI snapshot has no x-flint-cli-bootstrap-scopes")
	}
	return scopes
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
