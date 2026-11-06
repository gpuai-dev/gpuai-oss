package catalog

// EmptyCatalogJSON is the catalog GPU.ai Open Source ships: no templates.
// Template deploys (one-click application images served behind the FRP
// tunnel's app proxy) are not part of the open-source release, so every
// template_id is rejected as unknown. The loader, validator and deploy path
// stay intact so an operator can supply their own catalog file.
var EmptyCatalogJSON = []byte(`{"version": 1, "templates": []}`)

// LoadEmpty returns the empty catalog.
func LoadEmpty() (*Catalog, error) { return Load(EmptyCatalogJSON) }
