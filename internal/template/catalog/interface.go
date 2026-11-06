// Package catalog is the deployable-application template catalog — a
// byte-for-byte structural clone of internal/inference/catalog (CAT-06
// precedent). It loads an operator-curated templates.json (one entry per
// deployable app: ComfyUI, vLLM, …), validates each spec at load, and exposes
// an atomic-reload read surface for the public GET /v1/templates API.
//
// The TemplateCatalog interface is the read-path seam between handlers and the
// catalog. A future DB-backed implementation MUST satisfy this same interface
// so no handler / router package needs to change.
//
// Reload is INTENTIONALLY NOT in this interface — it is an operational concern
// (SIGHUP, future DB-poll). The cmd/gpuctl wire-up holds a typed *Catalog
// reference for the SIGHUP goroutine; the read-path consumers receive the
// interface. This split keeps the contract minimal (2 methods) and avoids
// forcing a future DBCatalog to invent a fake Reload — same rationale as the
// inference catalog's ModelCatalog (CAT-06 forward-compat).
package catalog

// TemplateCatalog is the read-path interface the public templates handler
// consumes. Swapping the concrete *Catalog for a LoadFromDB implementation
// MUST require zero changes in the handler package.
type TemplateCatalog interface {
	// Get resolves an id to a template. Returns (nil, false) for unknown ids
	// (handler emits 404).
	Get(id string) (*TemplateSpec, bool)

	// List returns all loaded templates. Templates have no modality axis, so
	// unlike the inference catalog this takes no filter argument.
	List() []*TemplateSpec
}

// Compile-time guard so the production *Catalog satisfies the interface. If a
// future struct refactor breaks Get or List signatures, this fails at build
// time — earlier than any test.
var _ TemplateCatalog = (*Catalog)(nil)
