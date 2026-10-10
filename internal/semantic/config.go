package semantic

import (
	"bytes"
	"encoding/json"
	"io"
)

// DeploymentConfig is separate from config.SourceConfig (GitHub bindings).
// It contains neither credentials nor per-capability routing.
type DeploymentConfig struct {
	Cheap Selection `json:"cheap"`
}
type Selection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Identifiers are bounded printable ASCII without whitespace. Exact names are
// retained; no normalization or discovery is performed.
func identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func (c DeploymentConfig) Validate() error {
	if !identifier(c.Cheap.Provider) || !identifier(c.Cheap.Model) {
		return ErrConfiguration
	}
	return nil
}
func ParseConfig(r io.Reader) (DeploymentConfig, error) {
	if r == nil {
		return DeploymentConfig{}, ErrConfiguration
	}
	data, err := io.ReadAll(r)
	if err != nil || !json.Valid(data) {
		return DeploymentConfig{}, ErrConfiguration
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if !uniqueKeys(d) {
		return DeploymentConfig{}, ErrConfiguration
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var c DeploymentConfig
	if d.Decode(&c) != nil || c.Validate() != nil {
		return DeploymentConfig{}, ErrConfiguration
	}
	return c, nil
}

// Token traversal rejects duplicates, including escaped spellings of keys.
// Parser diagnostics intentionally never contain user-supplied properties.
func uniqueKeys(d *json.Decoder) bool {
	token, err := d.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return false
			}
			seen[key] = true
			if !uniqueKeys(d) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			if !uniqueKeys(d) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim(']')
	default:
		return true
	}
}
