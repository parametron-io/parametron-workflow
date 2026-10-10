package semantic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"strings"
)

// Asset identity is its canonical path. Digest identifies the exact loaded bytes.
// Content is immutable text; Git retains historical contracts.
type Asset struct{ Identity, Digest, Content string }
type Assets struct{ Prompt, Schema Asset }
type Catalog struct{ assets map[Capability]Assets }

func capabilities() []Capability { return []Capability{ClassifyIssue, ClassifyPR, EstimateIssue} }
func assetPaths(c Capability) (string, string) {
	return "prompts/cheap/" + string(c) + ".txt", "schemas/model/" + string(c) + ".json"
}

func asset(identity string, content []byte) Asset {
	sum := sha256.Sum256(content)
	return Asset{Identity: identity, Digest: "sha256:" + hex.EncodeToString(sum[:]), Content: string(content)}
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
		catalog.assets[c] = Assets{Prompt: asset(promptPath, prompt), Schema: asset(schemaPath, schema)}
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
