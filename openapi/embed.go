// Package openapi holds the GPU.ai Open Source v1 API spec.
//
// openapi/v1.yaml is the single source of truth. The server embeds it from
// here (see internal/api/public/openapi), and sdk/generate.sh generates the
// Python and TypeScript SDKs from the same file.
package openapi

import _ "embed"

// V1YAML is the raw bytes of openapi/v1.yaml.
//
//go:embed v1.yaml
var V1YAML []byte
