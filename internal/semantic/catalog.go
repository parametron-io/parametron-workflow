package semantic

import (
	"encoding/json"
	"io/fs"
	"strings"
)

// Asset content is immutable text. Identity and version are code-owned.
type Asset struct{ Identity, Version, Content string }
type Assets struct{ Prompt, Schema Asset }
type Catalog struct{ assets map[Capability]Assets }

func capabilities() []Capability { return []Capability{ClassifyIssue, ClassifyPR, EstimateIssue} }
func assetPaths(c Capability) (string, string) {
	return "prompts/cheap/" + string(c) + "-v1.txt", "schemas/model/" + string(c) + "-v1.json"
}

// LoadCatalog reads only fixed canonical paths from an explicitly supplied FS.
// No implicit working directory, environment, embedding or duplicate asset tree
// is used. Production packaging belongs to later runtime composition.
func LoadCatalog(source fs.FS) (Catalog, error) {
	if source == nil {
		return Catalog{}, ErrAssets
	}
	catalog := Catalog{assets: make(map[Capability]Assets)}
	for _, c := range capabilities() {
		promptPath, schemaPath := assetPaths(c)
		prompt, err := fs.ReadFile(source, promptPath)
		if err != nil || strings.TrimSpace(string(prompt)) == "" {
			return Catalog{}, ErrAssets
		}
		schema, err := fs.ReadFile(source, schemaPath)
		if err != nil || !json.Valid(schema) {
			return Catalog{}, ErrAssets
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(schema, &object) != nil || object == nil {
			return Catalog{}, ErrAssets
		}
		catalog.assets[c] = Assets{Asset{promptPath, "v1", string(prompt)}, Asset{schemaPath, "v1", string(schema)}}
	}
	return catalog, nil
}
func (c Catalog) Lookup(capability Capability) (Assets, error) {
	if !supported(capability) {
		return Assets{}, ErrCapability
	}
	a, ok := c.assets[capability]
	if !ok {
		return Assets{}, ErrAssets
	}
	return a, nil
}
