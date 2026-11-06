// Package openapi embeds the handwritten v1 spec and serves it as JSON.
//
// The source of truth is openapi/v1.yaml at the repo root, embedded by the
// root openapi package. This package only converts and serves it.
package openapi

import (
	"encoding/json"
	"errors"
	"sync"

	"gopkg.in/yaml.v3"

	rootspec "github.com/gpuai-dev/gpuai-oss/openapi"
)

var v1YAML = rootspec.V1YAML

var (
	specOnce sync.Once
	specJSON []byte
	specErr  error
)

// SpecJSON returns the spec as JSON, computed once on first call.
//
// kin-openapi (the validator used in tests) and most documentation tools
// accept JSON natively; we convert at startup so the runtime serving path
// is a simple bytes-write with no per-request work.
func SpecJSON() ([]byte, error) {
	specOnce.Do(func() {
		var m map[string]any
		if uerr := yaml.Unmarshal(v1YAML, &m); uerr != nil {
			specErr = errors.Join(errors.New("openapi: yaml unmarshal failed"), uerr)
			return
		}
		specJSON, specErr = json.Marshal(m)
	})
	return specJSON, specErr
}

// SpecYAML returns the raw embedded YAML bytes (for tests + offline rendering).
func SpecYAML() []byte { return v1YAML }
