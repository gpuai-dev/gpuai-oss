package vultr

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// gpuNameMap maps Vultr's gpu_type identifier → GPU.ai canonical type. Keys
// are the exact strings the /plans catalog returns (live 2026-08-11:
// NVIDIA_A16, NVIDIA_A40, NVIDIA_L40S; A40 sells only fractional vGPU slices
// today, mapped anyway so a future whole-GPU A40 plan flows without an edit).
var gpuNameMap = map[string]provider.GPUType{
	"NVIDIA_A16":  provider.GPUTypeA16,
	"NVIDIA_A40":  provider.GPUTypeA40,
	"NVIDIA_L40S": provider.GPUTypeL40S,
}

// preferredName pins the vendor id used when provisioning a canonical type
// that several vendor names map to. Empty: every canonical type has exactly
// one Vultr gpu_type today.
var preferredName = map[provider.GPUType]string{}

// reverseGPUNameMap: canonical → vendor id, built deterministically so Go's
// randomized map iteration can't flip a multi-name type between process
// starts.
var reverseGPUNameMap map[provider.GPUType]string

func init() { reverseGPUNameMap = buildReverseGPUNameMap() }

func buildReverseGPUNameMap() map[provider.GPUType]string {
	out := make(map[provider.GPUType]string, len(gpuNameMap))
	names := make([]string, 0, len(gpuNameMap))
	for n := range gpuNameMap {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic default: sorted-first wins
	for _, n := range names {
		gt := gpuNameMap[n]
		if _, exists := out[gt]; !exists {
			out[gt] = n
		}
	}
	for gt, n := range preferredName { // explicit pins override
		out[gt] = n
	}
	return out
}

// NormalizeGPUName: Vultr gpu_type → canonical type.
func NormalizeGPUName(vendorName string) (provider.GPUType, bool) {
	gt, ok := gpuNameMap[vendorName]
	return gt, ok
}

// VultrGPUName: canonical type → Vultr gpu_type (for plan resolution).
func VultrGPUName(gt provider.GPUType) (string, bool) {
	n, ok := reverseGPUNameMap[gt]
	return n, ok
}

// parseGPUCount decodes the mixed-type gpu_count field of a vcg plan: Vultr
// encodes whole-GPU plans as a bare JSON number (2) and fractional vGPU
// slices as a string fraction ("1/8"). Only whole GPUs fit the platform's
// GPUCount model, so a fraction returns (0, false) and the plan is skipped.
func parseGPUCount(raw []byte) (int, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return 0, false
	}
	if strings.Contains(s, "/") {
		return 0, false // fractional vGPU slice — not a whole GPU
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// planIDModel maps the GPU token of a plan id ("vcg-<model>-<n>c-<ram>g-
// <vram>vram") → canonical type, for "vdm" rows that carry no gpu_type. The
// tokens are the ones live on 2026-11-04. Each model is the variant Vultr
// actually sells under it: the A100 plans are 80 GB cards, and the 8×H100
// plan is the HGX (SXM) board its bare-metal twin lists as NVIDIA_H100. AMD
// tokens (mi325x, mi355x) are absent because there is no canonical AMD type;
// those plans are skipped like any unmapped GPU.
var planIDModel = map[string]provider.GPUType{
	"a16":  provider.GPUTypeA16,
	"a40":  provider.GPUTypeA40,
	"a100": provider.GPUTypeA10080GB,
	"h100": provider.GPUTypeH100SXM,
	"b200": provider.GPUTypeB200,
	"l40s": provider.GPUTypeL40S,
}

var planIDPattern = regexp.MustCompile(`^vcg-([a-z0-9]+)-\d+c-\d+gb?-(\d+)vram$`)

// gpuFromPlanID reads (type, count) out of a plan id. The id gives TOTAL
// VRAM, so the count is total / the type's canonical per-GPU VRAM, and only
// an exact multiple is trusted: a remainder means the id names a slice or a
// variant we would mislabel, and the plan is skipped instead.
func gpuFromPlanID(id string) (provider.GPUType, int, bool) {
	m := planIDPattern.FindStringSubmatch(strings.ToLower(id))
	if m == nil {
		return "", 0, false
	}
	gt, ok := planIDModel[m[1]]
	if !ok {
		return "", 0, false
	}
	total, err := strconv.Atoi(m[2])
	per := provider.VRAMByGPUType(gt)
	if err != nil || per <= 0 || total < per || total%per != 0 {
		return "", 0, false
	}
	return gt, total / per, true
}

// planGPU returns a plan's canonical GPU type, whole-GPU count and per-GPU
// VRAM. A row that carries gpu_type ("vcg") is decoded from its fields, and a
// fractional gpu_count there is final (no id fallback: the slice is real). A
// row without it ("vdm") is decoded from its id. ok=false is an expected skip.
func planGPU(p plan) (gt provider.GPUType, count, vramPerGPU int, ok bool) {
	if p.GPUType != "" {
		if count, ok = parseGPUCount(p.GPUCount); !ok {
			return "", 0, 0, false
		}
		if gt, ok = NormalizeGPUName(p.GPUType); !ok {
			return "", 0, 0, false
		}
		vramPerGPU = p.GPUVRAMGB / count // catalog VRAM is the TOTAL across GPUs
	} else if gt, count, ok = gpuFromPlanID(p.ID); !ok {
		return "", 0, 0, false
	}
	if vramPerGPU <= 0 {
		vramPerGPU = provider.VRAMByGPUType(gt)
	}
	return gt, count, vramPerGPU, true
}

// regionCity maps Vultr's region id (a 3-letter airport-style code, the value
// POST /instances takes) → human city label, from the live /v2/regions list
// (2026-08-11). Used for logging/debug only; offerings emit the NATIVE code
// as both Region and DatacenterLocation and
// internal/availability/canonicalRegionMap owns the canonical bucketing.
var regionCity = map[string]string{
	"ams": "Amsterdam", "atl": "Atlanta", "blr": "Bangalore", "bom": "Mumbai",
	"cdg": "Paris", "del": "Delhi NCR", "dfw": "Dallas", "ewr": "New Jersey",
	"fra": "Frankfurt", "hnl": "Honolulu", "icn": "Seoul", "itm": "Osaka",
	"jnb": "Johannesburg", "lax": "Los Angeles", "lhr": "London", "mad": "Madrid",
	"man": "Manchester", "mel": "Melbourne", "mex": "Mexico City", "mia": "Miami",
	"mxp": "Milan", "nrt": "Tokyo", "ord": "Chicago", "sao": "São Paulo",
	"scl": "Santiago", "sea": "Seattle", "sgp": "Singapore", "sjc": "Silicon Valley",
	"sto": "Stockholm", "syd": "Sydney", "tlv": "Tel Aviv", "waw": "Warsaw",
	"yto": "Toronto",
}

// KnownRegion reports whether a region id is in the live-verified Vultr set.
func KnownRegion(id string) bool {
	_, ok := regionCity[id]
	return ok
}
