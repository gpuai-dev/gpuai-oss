package manifest

import _ "embed"

// EmbeddedManifestJSON is the binary-embedded pinned-environment manifest: the
// operator-curated, per-GPU-SKU driver/CUDA/certified-image contract that ships
// inside the gpuctl binary (D-63-07). It is the single server-controlled source
// of image references — no customer-supplied image string ever reaches a
// provider adapter (V5). Mirrors internal/template/catalog/embed.go so the two
// embedded-catalog packages stay structurally aligned.
//
// Wave 0 ships an intentionally empty manifest ({} gpus / {} floors); Plan 02
// fills the real per-GPU rows and the toolkit→driver-floor table.
//
//go:embed manifest.json
var EmbeddedManifestJSON []byte
