// Package manifest holds the pinned-base-environment contract for Phase 63: the
// per-GPU-SKU driver floor, minimum CUDA generation, and DIGEST-PINNED certified
// image reference (D-63-07), plus the toolkit→minimum-driver table. It is the
// single server-controlled source of image references — nothing here is ever
// derived from customer input (V5). Analog: internal/template/catalog.
//
// Wave 0 (Plan 01) is contract-only: LoadEmbedded parses the embedded JSON, and
// the lookup methods return zero-value + false so the RED tests compile and fail
// on assertions rather than panicking. Plan 02 fills the real manifest content
// and the lookup bodies.
package manifest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// GPUEnv is the pinned environment contract for one GPU SKU (D-63-07).
type GPUEnv struct {
	GPUModel    string `json:"gpu_model"`           // catalog GPU model string, e.g. "RTX PRO 6000"
	DriverFloor string `json:"driver_floor"`        // minimum NVIDIA driver, e.g. "570.26"
	CUDAMin     string `json:"cuda_min"`            // minimum CUDA toolkit generation, e.g. "12.8"
	ImageRef    string `json:"image_ref"`           // DIGEST-PINNED certified image: ghcr.io/gpuai-dev/base:cuda128-YYYY-MM@sha256:...
	TorchMin    string `json:"torch_min,omitempty"` // advisory only
}

// IncludeItem is one "What's included" software entry for a framework
// environment (e.g. {"PyTorch", "2.11.0"}). Version may be empty for items
// whose exact build version isn't pinned (e.g. "git").
type IncludeItem struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// VMImage is one raw-VM OS catalog entry (the launch OS picker): the
// customer-facing label plus the per-provider NATIVE image slugs. The wire
// grammar references entries by their map key ("os id", e.g. "ubuntu-24.04" —
// the `raw_vm:<os>` suffix). The Providers map is server-side ONLY (D6 defense
// by omission — the exact capacity_class/deployment_type pattern): offerings
// expose just an os-id list (os_options) computed at cache-write from the
// provider name, and the create path resolves id → native slug engine-side.
type VMImage struct {
	Label   string `json:"label"`   // customer-facing, OURS — never a provider's image name (INFPRIV)
	Family  string `json:"family"`  // distro family, e.g. "ubuntu"
	Version string `json:"version"` // distro version, e.g. "24.04"
	// DriverPreinstalled marks images that ship the NVIDIA driver (+CUDA)
	// baked in, so the bootstrap's driver-floor gate passes without a distro
	// driver install. Display metadata only — the bootstrap probes the real
	// driver either way.
	DriverPreinstalled bool `json:"driver_preinstalled,omitempty"`
	// Providers maps provider registry name → that provider's native image
	// slug/alias/display name (VERIFIED against the provider's live image
	// list before entry — never guessed). A provider absent here simply
	// doesn't offer this OS: the option won't appear in its offerings'
	// os_options and the engine refuses rather than substituting.
	Providers map[string]string `json:"providers"`
}

// FrameworkVersion is one selectable version of a framework environment (the
// launch version picker). A framework's versions are per-CUDA-variant: the GPU
// dictates the variant, so a version is offerable only on machines whose
// cuda_min maps to Variant. Today each (framework, variant) pins exactly one
// version, so a given offering surfaces a single version as info (D5); adding a
// second same-variant image is a pure manifest+image change — a new entry whose
// ImageRef overrides the family default — that needs no engine change.
type FrameworkVersion struct {
	ID      string `json:"id"`                // wire token in `certified:<fw>@<id>` (e.g. "2.13"); [a-z0-9.-]
	Label   string `json:"label"`             // customer-facing, e.g. "PyTorch 2.13"
	Variant string `json:"variant"`           // CUDA variant key this version targets ("cuda128"|"cuda124")
	Default bool   `json:"default,omitempty"` // preselected in the picker (at most one per framework)
	// ImageRef optionally overrides the family default (framework_images[fw][Variant])
	// with a version-specific digest-pinned image — set only when a single variant
	// carries more than one version. Empty => the family ref for Variant is used, so
	// the bare `certified:<fw>` default path and the versioned path stay one image.
	ImageRef string `json:"image_ref,omitempty"`
}

// Manifest holds the parsed manifest.json: the per-GPU-SKU environment contracts
// keyed by GPU model string, and the toolkit-generation→minimum-driver table.
type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	GPUs          map[string]GPUEnv `json:"gpus"`
	// ToolkitDriverFloors maps a CUDA toolkit generation (e.g. "12.8") to the
	// minimum NVIDIA driver that satisfies it (e.g. "570.26").
	ToolkitDriverFloors map[string]string `json:"toolkit_driver_floors"`
	// FrameworkImages holds the framework-environment image families (D-64-01):
	// framework id ("pytorch", "cuda-devel", "jupyter") → CUDA base variant key
	// ("cuda128", "cuda124") → DIGEST-PINNED image ref. pytorch/jupyter are thin
	// layers built FROM the matching base family above; cuda-devel rebuilds the
	// same certified recipe on the upstream -devel base (NVIDIA's apt repo
	// drifts past frozen point-release tags, so devel cannot be an apt layer on
	// the runtime base). All are pinned by the same reviewed flow
	// (framework-images.yml + tools/bump_base_image.py --framework).
	FrameworkImages map[string]map[string]string `json:"framework_images"`
	// FrameworkIncludes publishes the per-framework software version lists the
	// launch UI's "What's included" renders: framework id (framework_images keys
	// plus "base" for the bare certified image) → CUDA variant key ("cuda128",
	// "cuda124") → items. The versions describe the CURRENTLY-PINNED digests —
	// they are captured from the framework-images.yml build (the resolved
	// `pip install` / base-tag versions) and must be refreshed alongside every
	// monthly re-pin. Names + versions only: no image refs or digests ride here
	// (T-63-04 defense by omission — this map is served to the dashboard).
	FrameworkIncludes map[string]map[string][]IncludeItem `json:"framework_includes,omitempty"`
	// VMImages is the raw-VM OS catalog (the launch OS picker), keyed by os id
	// — the token the `raw_vm:<os>` wire grammar carries (e.g. "ubuntu-24.04",
	// so ids allow dots). Server-controlled like everything else here; the
	// per-provider slug maps inside are never serialized to customers.
	VMImages map[string]VMImage `json:"vm_images,omitempty"`
	// FrameworkVersions is the launch version picker's catalog: framework id →
	// ordered selectable versions (the `certified:<fw>@<id>` grammar). Only
	// frameworks with a user-meaningful version choice are listed; a framework
	// absent here has one build per variant and renders single-version info from
	// FrameworkIncludes (D5). Server-controlled; digest pins live in FrameworkImages
	// (or a version's ImageRef override) — never the wire.
	FrameworkVersions map[string][]FrameworkVersion `json:"framework_versions,omitempty"`
}

// LoadEmbedded parses the binary-embedded manifest. Used at boot (fail-fast: a
// malformed embedded manifest.json crashes startup, serve.go analog :202/:236).
func LoadEmbedded() (*Manifest, error) { return Load(EmbeddedManifestJSON) }

// Load parses a manifest payload. Kept separate from LoadEmbedded so tests and a
// future SIGHUP reload path can feed arbitrary bytes (catalog.Load analog).
func Load(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}
	if m.GPUs == nil {
		m.GPUs = map[string]GPUEnv{}
	}
	if m.ToolkitDriverFloors == nil {
		m.ToolkitDriverFloors = map[string]string{}
	}
	if m.FrameworkImages == nil {
		m.FrameworkImages = map[string]map[string]string{}
	}
	if m.FrameworkIncludes == nil {
		m.FrameworkIncludes = map[string]map[string][]IncludeItem{}
	}
	if m.VMImages == nil {
		m.VMImages = map[string]VMImage{}
	}
	if m.FrameworkVersions == nil {
		m.FrameworkVersions = map[string][]FrameworkVersion{}
	}

	// Strict boot-time validation (T-63-02 / T-63-03): a malformed manifest must
	// crash startup rather than serve un-pinned or spoofable image references.
	// The floors table is validated first so per-GPU cuda_min checks below can
	// key off a known-good set of generations.
	for gen, driver := range m.ToolkitDriverFloors {
		if strings.TrimSpace(driver) == "" {
			return nil, fmt.Errorf("manifest: toolkit_driver_floors[%q] has empty driver floor", gen)
		}
	}
	for key, env := range m.GPUs {
		if strings.TrimSpace(env.DriverFloor) == "" {
			return nil, fmt.Errorf("manifest: gpu %q has empty driver_floor", key)
		}
		// cuda_min MUST be a generation present in the floors table — otherwise a
		// pinned SKU could reference a driver floor we cannot resolve (T-63-03).
		floor, ok := m.ToolkitDriverFloors[env.CUDAMin]
		if !ok {
			return nil, fmt.Errorf("manifest: gpu %q cuda_min %q not present in toolkit_driver_floors", key, env.CUDAMin)
		}
		// The per-SKU driver_floor must agree with the toolkit table for its
		// cuda_min — a divergence means one of the two is stale.
		if floor != env.DriverFloor {
			return nil, fmt.Errorf("manifest: gpu %q driver_floor %q disagrees with toolkit_driver_floors[%q]=%q", key, env.DriverFloor, env.CUDAMin, floor)
		}
		// Supply-chain (T-63-02): the certified image MUST be digest-pinned — a
		// mutable tag would let an upstream rebuild swap the image out from under
		// us. Require a "@sha256:" digest in every image_ref.
		if !strings.Contains(env.ImageRef, "@sha256:") {
			return nil, fmt.Errorf("manifest: gpu %q image_ref %q is not digest-pinned (missing @sha256:)", key, env.ImageRef)
		}
	}

	// Framework families (D-64-01) get the same strictness: every ref digest-
	// pinned, and every framework must cover every CUDA variant the gpus table
	// actually uses — a partial family would make certified:<framework> launches
	// fail for a whole slice of the fleet, which should be caught at boot, not
	// at provision time.
	usedVariants := map[string]bool{}
	for _, env := range m.GPUs {
		usedVariants[cudaVariantKey(env.CUDAMin)] = true
	}
	for fw, family := range m.FrameworkImages {
		if strings.TrimSpace(fw) == "" {
			return nil, fmt.Errorf("manifest: framework_images has an empty framework id")
		}
		for variant, ref := range family {
			if strings.TrimSpace(variant) == "" {
				return nil, fmt.Errorf("manifest: framework %q has an empty variant key", fw)
			}
			if !strings.Contains(ref, "@sha256:") {
				return nil, fmt.Errorf("manifest: framework %q variant %q image ref %q is not digest-pinned (missing @sha256:)", fw, variant, ref)
			}
		}
		for variant := range usedVariants {
			if _, ok := family[variant]; !ok {
				return nil, fmt.Errorf("manifest: framework %q is missing variant %q (required by gpus table)", fw, variant)
			}
		}
	}

	// VM OS catalog (the launch OS picker). Every os id must satisfy the wire
	// grammar's charset (the raw_vm:<os> suffix rides untrusted client input,
	// so a manifest id outside it could never be selected — catch the drift at
	// boot), carry a customer-facing label, and map at least one provider to a
	// non-empty native slug (an unmapped entry is dead weight that would still
	// show up in labels).
	for osID, img := range m.VMImages {
		if !ValidVMOSID(osID) {
			return nil, fmt.Errorf("manifest: vm_images id %q is not a valid os id ([a-z0-9.-], 1-32 chars, no leading/trailing '-' or '.')", osID)
		}
		if strings.TrimSpace(img.Label) == "" {
			return nil, fmt.Errorf("manifest: vm_images[%q] has an empty label", osID)
		}
		if strings.TrimSpace(img.Family) == "" || strings.TrimSpace(img.Version) == "" {
			return nil, fmt.Errorf("manifest: vm_images[%q] needs family and version", osID)
		}
		if len(img.Providers) == 0 {
			return nil, fmt.Errorf("manifest: vm_images[%q] maps no providers", osID)
		}
		for prov, slug := range img.Providers {
			if strings.TrimSpace(prov) == "" || strings.TrimSpace(slug) == "" {
				return nil, fmt.Errorf("manifest: vm_images[%q] has an empty provider name or slug", osID)
			}
		}
	}

	// Framework version catalog (the launch version picker). Each version's id
	// must satisfy the same charset the API format gate enforces on the untrusted
	// `certified:<fw>@<id>` suffix (catch drift at boot), be unique within its
	// framework, carry a label + a CUDA variant, and resolve to a PUBLISHED
	// digest-pinned image — either an explicit ImageRef override or the family
	// ref for its variant. At most one version per framework may be the default.
	for fw, versions := range m.FrameworkVersions {
		family, ok := m.FrameworkImages[fw]
		if !ok {
			return nil, fmt.Errorf("manifest: framework_versions[%q] has no matching framework_images family", fw)
		}
		seen := map[string]bool{}
		defaults := 0
		for i, v := range versions {
			if !ValidFrameworkVersionID(v.ID) {
				return nil, fmt.Errorf("manifest: framework_versions[%q][%d] id %q is not a valid version id ([a-z0-9.-], 1-32 chars, no leading/trailing '-' or '.')", fw, i, v.ID)
			}
			if seen[v.ID] {
				return nil, fmt.Errorf("manifest: framework_versions[%q] has duplicate version id %q", fw, v.ID)
			}
			seen[v.ID] = true
			if strings.TrimSpace(v.Label) == "" {
				return nil, fmt.Errorf("manifest: framework_versions[%q][%q] has an empty label", fw, v.ID)
			}
			if strings.TrimSpace(v.Variant) == "" {
				return nil, fmt.Errorf("manifest: framework_versions[%q][%q] has an empty variant", fw, v.ID)
			}
			if v.ImageRef != "" {
				if !strings.Contains(v.ImageRef, "@sha256:") {
					return nil, fmt.Errorf("manifest: framework_versions[%q][%q] image_ref %q is not digest-pinned (missing @sha256:)", fw, v.ID, v.ImageRef)
				}
			} else {
				ref, ok := family[v.Variant]
				if !ok {
					return nil, fmt.Errorf("manifest: framework_versions[%q][%q] variant %q is not covered by framework_images (and no image_ref override)", fw, v.ID, v.Variant)
				}
				if IsPlaceholderRef(ref) {
					return nil, fmt.Errorf("manifest: framework_versions[%q][%q] variant %q is still an unpublished placeholder image", fw, v.ID, v.Variant)
				}
			}
			if v.Default {
				defaults++
			}
		}
		if defaults > 1 {
			return nil, fmt.Errorf("manifest: framework_versions[%q] declares %d default versions (at most 1)", fw, defaults)
		}
	}

	// Includes lists are display metadata, so validation is shape-only: every
	// item needs a name (an empty version is allowed — not every component pins
	// an exact build), and every published framework family should carry a list
	// for each variant it publishes, so the UI never shows a version list for
	// one CUDA generation and silently nothing for the other.
	for fw, variants := range m.FrameworkIncludes {
		if strings.TrimSpace(fw) == "" {
			return nil, fmt.Errorf("manifest: framework_includes has an empty framework id")
		}
		for variant, items := range variants {
			if strings.TrimSpace(variant) == "" {
				return nil, fmt.Errorf("manifest: framework_includes[%q] has an empty variant key", fw)
			}
			for i, item := range items {
				if strings.TrimSpace(item.Name) == "" {
					return nil, fmt.Errorf("manifest: framework_includes[%q][%q] item %d has an empty name", fw, variant, i)
				}
			}
		}
	}
	return &m, nil
}

// Lookup returns the pinned environment contract for a GPU model. An unknown GPU
// returns the zero GPUEnv and false — NEVER a panic, so callers degrade to the
// legacy (un-pinned) path rather than crash.
//
// Lookup first tries an exact match on the GPU model key, then falls back to a
// normalized (whitespace-trimmed, case-insensitive) comparison so minor catalog
// string drift ("rtx pro 6000 ", "RTX Pro 6000") still
// resolves. An unknown GPU returns the zero GPUEnv and false.
func (m *Manifest) Lookup(gpuModel string) (GPUEnv, bool) {
	if env, ok := m.GPUs[gpuModel]; ok {
		return env, true
	}
	want := normalizeGPUKey(gpuModel)
	if want == "" {
		return GPUEnv{}, false
	}
	for key, env := range m.GPUs {
		if normalizeGPUKey(key) == want {
			return env, true
		}
	}
	return GPUEnv{}, false
}

// normalizeGPUKey lowercases and collapses surrounding whitespace so the Lookup
// fallback tolerates minor catalog string drift without matching unrelated SKUs.
func normalizeGPUKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// DriverFloorForCUDA returns the minimum NVIDIA driver for a CUDA toolkit
// generation (e.g. "12.8" → "570.26"). An unknown generation returns "" + false.
func (m *Manifest) DriverFloorForCUDA(cudaGen string) (string, bool) {
	floor, ok := m.ToolkitDriverFloors[cudaGen]
	return floor, ok
}

// placeholderDigest is the all-zero digest the manifest carries between adding a
// framework family and pinning its first published build (the Phase-63
// PLACEHOLDER scheme). It satisfies the digest-pin shape but must never launch.
const placeholderDigest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// cudaVariantKey maps a GPU's cuda_min generation to its base/framework image
// family key: "12.8" → "cuda128", "12.4" → "cuda124".
func cudaVariantKey(cudaMin string) string {
	return "cuda" + strings.ReplaceAll(strings.TrimSpace(cudaMin), ".", "")
}

// FrameworkImageRef returns the digest-pinned framework-environment image for a
// framework id + a GPU's cuda_min generation (D-64-01). Unknown framework,
// uncovered variant, or a still-PLACEHOLDER (unpublished) ref returns "" + false
// — callers treat that exactly like a missing manifest entry and refuse to
// launch rather than substitute (never a panic, mirrors Lookup).
func (m *Manifest) FrameworkImageRef(framework, cudaMin string) (string, bool) {
	family, ok := m.FrameworkImages[framework]
	if !ok {
		return "", false
	}
	ref, ok := family[cudaVariantKey(cudaMin)]
	if !ok || IsPlaceholderRef(ref) {
		return "", false
	}
	return ref, true
}

// Frameworks returns the framework ids that have at least one PUBLISHED (non-
// placeholder) image, sorted — the server-controlled roster the launch surfaces
// offer. A framework whose family is still all-PLACEHOLDER is invisible, so an
// unpublished image can never be selected.
func (m *Manifest) Frameworks() []string {
	var out []string
	for fw, family := range m.FrameworkImages {
		for _, ref := range family {
			if !IsPlaceholderRef(ref) {
				out = append(out, fw)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// IsPlaceholderRef reports whether an image ref still carries the all-zero
// PLACEHOLDER digest (added-but-not-yet-published).
func IsPlaceholderRef(ref string) bool {
	return strings.HasSuffix(ref, placeholderDigest)
}

// FrameworkVersionsFor returns the selectable versions for a framework (the
// version picker catalog), or nil when the framework has no version choice —
// callers then render its single build as info (D5).
func (m *Manifest) FrameworkVersionsFor(framework string) []FrameworkVersion {
	return m.FrameworkVersions[framework]
}

// FrameworkImageRefForVersion resolves the digest-pinned image for a specific
// framework VERSION on a GPU's cuda_min generation. It refuses (""+false) when
// the framework has no version catalog, no such version id, the version targets
// a different CUDA variant than this GPU (the machine can't run it), or the
// resolved image is still an unpublished placeholder. A false result is the
// engine's and the API gate's cue to reject the launch — never to substitute a
// different image (mirrors FrameworkImageRef).
func (m *Manifest) FrameworkImageRefForVersion(framework, cudaMin, versionID string) (string, bool) {
	versions, ok := m.FrameworkVersions[framework]
	if !ok {
		return "", false
	}
	variant := cudaVariantKey(cudaMin)
	for _, v := range versions {
		if v.ID != versionID {
			continue
		}
		if v.Variant != variant {
			return "", false // this GPU's CUDA variant can't run the requested version
		}
		if v.ImageRef != "" {
			if IsPlaceholderRef(v.ImageRef) {
				return "", false
			}
			return v.ImageRef, true
		}
		return m.FrameworkImageRef(framework, cudaMin)
	}
	return "", false
}

// ValidFrameworkVersionID bounds a framework version id to the picker alphabet:
// [a-z0-9.-], 1-32 chars, no leading/trailing '-' or '.'. The same shape the
// API format gate enforces on the untrusted `certified:<fw>@<id>` suffix —
// checked here at boot so a manifest version the grammar could never select
// fails fast. Identical rules to ValidVMOSID (both allow dotted versions).
func ValidFrameworkVersionID(v string) bool {
	return ValidVMOSID(v)
}

// ValidVMOSID bounds an os id to the vm_images key alphabet: [a-z0-9.-],
// 1-32 chars, no leading/trailing '-' or '.'. The same shape the API format
// gates enforce on the untrusted raw_vm:<os> wire suffix — checked here at
// boot so a manifest entry the grammar could never select fails fast.
func ValidVMOSID(os string) bool {
	if len(os) == 0 || len(os) > 32 {
		return false
	}
	if os[0] == '-' || os[0] == '.' || os[len(os)-1] == '-' || os[len(os)-1] == '.' {
		return false
	}
	for _, c := range os {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// VMOSOptionsFor returns the sorted os ids the named provider has a native
// image slug for — the os_options list stamped on VM offerings at cache-write
// (D6: ids only; the slugs themselves never leave the server). An unknown
// provider (or a container-native one) returns nil, which serializes away
// under omitempty.
func (m *Manifest) VMOSOptionsFor(providerName string) []string {
	var out []string
	for osID, img := range m.VMImages {
		if _, ok := img.Providers[providerName]; ok {
			out = append(out, osID)
		}
	}
	sort.Strings(out)
	return out
}

// VMImageSlug resolves the named provider's NATIVE image slug for an os id.
// Unknown os id or a provider with no mapping returns "" + false — callers
// refuse explicitly (never guess a boot image), mirroring FrameworkImageRef.
func (m *Manifest) VMImageSlug(providerName, osID string) (string, bool) {
	img, ok := m.VMImages[osID]
	if !ok {
		return "", false
	}
	slug, ok := img.Providers[providerKey(providerName)]
	return slug, ok && slug != ""
}

// HasVMImage reports whether an os id exists in the vm_images catalog at all
// (provider-independent — the per-provider narrowing is VMImageSlug).
func (m *Manifest) HasVMImage(osID string) bool {
	_, ok := m.VMImages[osID]
	return ok
}

// providerKey normalizes a provider registry name for the Providers map
// lookup. Registry names are already lower-case; trim defensively so a
// wired-with-whitespace name can't silently miss its mapping.
func providerKey(providerName string) string {
	return strings.ToLower(strings.TrimSpace(providerName))
}
