package public

import (
	"net/http"
	"sort"

	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
)

// handleListEnvironments serves GET /v1/environments (no auth — public catalog),
// the CLI/SDK parity mirror of the dashboard's environments roster. It publishes
// the server-controlled launch environment catalog:
//
//   - frameworks:  ids whose image families are FULLY published (every CUDA
//     variant pinned to a real digest) — the framework picker's options.
//   - versions:    the launch version picker's per-framework selectable versions
//     (the `certified:<fw>@<id>` grammar), each tagged with the CUDA variant it
//     targets so a client can filter to the versions a given GPU can run.
//   - includes:    the "What's included" software version lists per framework +
//     "base", keyed by CUDA variant.
//   - vm_images:   the raw-VM OS catalog labels (the `raw_vm:<os>` grammar),
//     keyed by the same os ids offerings carry in os_options.
//
// Names, labels, versions, and os ids only — image refs/digests and per-provider
// native slug maps NEVER cross this wire (T-63-04 / D6 defense by omission; the
// per-GPU pin details live on the /v1/pricing environment block).
func (deps PublicDeps) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	frameworks := []string{}
	includes := map[string]map[string][]manifest.IncludeItem{}
	versions := map[string][]environmentVersion{}
	vmImages := map[string]environmentVMImage{}

	if m := loadedManifest(); m != nil {
		published := map[string]bool{}
		for fw, family := range m.FrameworkImages {
			if len(family) == 0 {
				continue
			}
			ok := true
			for _, ref := range family {
				if manifest.IsPlaceholderRef(ref) {
					ok = false
					break
				}
			}
			if ok {
				published[fw] = true
				frameworks = append(frameworks, fw)
				if items, has := m.FrameworkIncludes[fw]; has {
					includes[fw] = items
				}
			}
		}
		sort.Strings(frameworks)
		// "base" (the bare certified image) is not a framework family but carries
		// an includes list — the launch Base card renders it.
		if items, ok := m.FrameworkIncludes["base"]; ok {
			includes["base"] = items
		}
		// Versions only for frameworks in the published roster.
		for fw, vs := range m.FrameworkVersions {
			if !published[fw] {
				continue
			}
			out := make([]environmentVersion, 0, len(vs))
			for _, v := range vs {
				out = append(out, environmentVersion{
					ID:      v.ID,
					Label:   v.Label,
					Variant: v.Variant,
					Default: v.Default,
				})
			}
			versions[fw] = out
		}
		for osID, img := range m.VMImages {
			vmImages[osID] = environmentVMImage{
				Label:              img.Label,
				Family:             img.Family,
				Version:            img.Version,
				DriverPreinstalled: img.DriverPreinstalled,
			}
		}
	}

	writeJSON(w, http.StatusOK, environmentsResponse{
		Frameworks: frameworks,
		Versions:   versions,
		Includes:   includes,
		VMImages:   vmImages,
	})
}

// environmentsResponse is the GET /v1/environments payload.
type environmentsResponse struct {
	Frameworks []string                                     `json:"frameworks"`
	Versions   map[string][]environmentVersion              `json:"versions"`
	Includes   map[string]map[string][]manifest.IncludeItem `json:"includes"`
	VMImages   map[string]environmentVMImage                `json:"vm_images"`
}

// environmentVersion is the wire projection of one framework version — a
// deliberate copy (not manifest.FrameworkVersion) so the server-side ImageRef
// pin structurally cannot marshal onto the wire (T-63-04).
type environmentVersion struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Variant string `json:"variant"`
	Default bool   `json:"default,omitempty"`
}

// environmentVMImage is the customer-facing projection of one vm_images entry —
// a deliberate copy so the server-side per-provider slug map cannot marshal onto
// the wire (D6 defense by omission; labels are ours, not providers').
type environmentVMImage struct {
	Label              string `json:"label"`
	Family             string `json:"family"`
	Version            string `json:"version"`
	DriverPreinstalled bool   `json:"driver_preinstalled,omitempty"`
}
