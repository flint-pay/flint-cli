package spec

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// StartupIndex is generated from the same snapshot as schema inspection. Ranges
// point into OpenAPI, so execution never uses a second copy of a schema.
type StartupIndex struct {
	SHA256        string            `json:"sha256"`
	Commands      json.RawMessage   `json:"commands"`
	LocalCommands json.RawMessage   `json:"local_commands"`
	Metadata      json.RawMessage   `json:"metadata"`
	Operations    map[string][2]int `json:"operations"`
	Schemas       map[string][2]int `json:"schemas"`
}

//go:embed startup.json
var startupJSON []byte

var startup = sync.OnceValue(func() *StartupIndex {
	var index StartupIndex
	if err := json.Unmarshal(startupJSON, &index); err != nil {
		panic(err)
	}
	return &index
})

func Startup() *StartupIndex { return startup() }

func (index *StartupIndex) Operation(id string, target any) bool {
	return index.decode(index.Operations, id, target)
}
func (index *StartupIndex) Schema(name string, target any) bool {
	return index.decode(index.Schemas, name, target)
}
func (index *StartupIndex) decode(ranges map[string][2]int, name string, target any) bool {
	span, ok := ranges[name]
	if !ok {
		return false
	}
	if span[0] < 0 || span[1] > len(OpenAPI) || span[0] >= span[1] {
		panic("invalid generated snapshot range; run go generate ./internal/cli")
	}
	if err := json.Unmarshal(OpenAPI[span[0]:span[1]], target); err != nil {
		panic(err)
	}
	return true
}

// GenerateStartup derives routing metadata and exact snapshot ranges. Commands
// are supplied by the CLI's normal registry builder, including its overlays.
func GenerateStartup(commands, localCommands json.RawMessage) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(OpenAPI, &doc); err != nil {
		return nil, err
	}
	index := StartupIndex{SHA256: fmt.Sprintf("%x", sha256.Sum256(OpenAPI)), Commands: commands, LocalCommands: localCommands, Operations: map[string][2]int{}, Schemas: map[string][2]int{}}
	rootRanges, err := objectRanges(OpenAPI, 0)
	if err != nil {
		return nil, err
	}
	componentRange := rootRanges["components"]
	componentRanges, err := objectRanges(OpenAPI[componentRange[0]:componentRange[1]], componentRange[0])
	if err != nil {
		return nil, err
	}
	schemaRange := componentRanges["schemas"]
	index.Schemas, err = objectRanges(OpenAPI[schemaRange[0]:schemaRange[1]], schemaRange[0])
	if err != nil {
		return nil, err
	}
	pathRange := rootRanges["paths"]
	pathRanges, err := objectRanges(OpenAPI[pathRange[0]:pathRange[1]], pathRange[0])
	if err != nil {
		return nil, err
	}
	operationRanges := map[string]map[string][2]int{}
	for path, span := range pathRanges {
		methods, err := objectRanges(OpenAPI[span[0]:span[1]], span[0])
		if err != nil {
			return nil, err
		}
		operationRanges[path] = methods
	}

	var paths map[string]map[string]json.RawMessage
	if err := json.Unmarshal(doc["paths"], &paths); err != nil {
		return nil, err
	}
	// Keep only operation metadata consumed by the tree, scope policy and opindex.
	for path, methods := range paths {
		for method, raw := range methods {
			var op map[string]json.RawMessage
			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, err
			}
			var id string
			_ = json.Unmarshal(op["operationId"], &id)
			if id == "" {
				continue
			}
			index.Operations[id] = operationRanges[path][method]
			delete(op, "requestBody")
			delete(op, "responses")
			var params []map[string]json.RawMessage
			_ = json.Unmarshal(op["parameters"], &params)
			for _, param := range params {
				delete(param, "schema")
				delete(param, "description")
				delete(param, "example")
				delete(param, "examples")
			}
			if _, ok := op["parameters"]; ok {
				op["parameters"], _ = json.Marshal(params)
			}
			methods[method], _ = json.Marshal(op)
		}
	}
	doc["paths"], _ = json.Marshal(paths)
	var webhooks map[string]map[string]json.RawMessage
	if err := json.Unmarshal(doc["webhooks"], &webhooks); err != nil {
		return nil, err
	}
	for _, methods := range webhooks {
		for method, raw := range methods {
			var op map[string]json.RawMessage
			_ = json.Unmarshal(raw, &op)
			for key := range op {
				if key != "summary" && key != "description" {
					delete(op, key)
				}
			}
			methods[method], _ = json.Marshal(op)
		}
	}
	doc["webhooks"], _ = json.Marshal(webhooks)
	delete(doc, "components")
	delete(doc, "x-flint-webhooks")
	delete(doc, "x-flint-event-schemas")
	index.Metadata, _ = json.Marshal(doc)
	return json.Marshal(index)
}

// Decoder offsets give exact ranges in linear time. Searching each raw value
// through the whole snapshot makes race-instrumented parity checks quadratic.
func objectRanges(source []byte, base int) (map[string][2]int, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("expected snapshot object at %d", base)
	}
	result := map[string][2]int{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected snapshot key at %d", base+int(decoder.InputOffset()))
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		end := base + int(decoder.InputOffset())
		result[key] = [2]int{end - len(value), end}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return result, nil
}
