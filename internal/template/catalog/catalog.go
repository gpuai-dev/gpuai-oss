package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// MinDiskGB is the floor on any instance disk size, in GB — both a template's
// declared container_disk_gb (validated at catalog load, below) and a
// customer's disk_gb launch request (validated in the API handlers). Anything
// smaller ENOSPCs on boot: a bare CUDA runtime image alone is several GB.
// Exported so the two validators cannot drift apart.
const MinDiskGB = 10

// imageDigestRe requires every active template's image to carry an immutable
// @sha256: digest alongside its tag. See the call site in Load for why.
var imageDigestRe = regexp.MustCompile(`^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)

// minPlausibleCUDA / maxPlausibleCUDA bound a template's min_cuda so a
// decimal-point typo cannot silently strand the template with no eligible
// capacity. Widen when CUDA does.
const (
	minPlausibleCUDA = 10.0
	maxPlausibleCUDA = 20.0
)

// Port is one published port of a deployable template. Exactly one port per
// template carries Primary=true — that is the port the deploy entrypoint
// surfaces to the customer as the app URL (downstream plans).
type Port struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // "http" | "tcp"
	Primary  bool   `json:"primary"`
}

// EnvSpec is one environment variable a template declares. UserOverridable
// controls whether the deploy request may set it; Secret marks values that
// must never be echoed back (downstream credential-surfacing plans).
type EnvSpec struct {
	Key             string `json:"key"`
	Default         string `json:"default"`
	UserOverridable bool   `json:"user_overridable"`
	Secret          bool   `json:"secret"`
}

// TemplateSpec is one operator-curated deployable application. Image and
// StartCommand are INTERNAL orchestration details (TMPL-13 defense-by-omission)
// — they are present in the embedded catalog but the public wire view
// (internal/api/public/handlers_templates.go) structurally omits them.
type TemplateSpec struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	Category        string `json:"category"`
	Kind            string `json:"kind"`             // v1: "service" only
	Status          string `json:"status,omitempty"` // "" or "coming_soon"
	Image           string `json:"image"`            // INTERNAL — omit from wire (TMPL-13)
	ContainerDiskGB int    `json:"container_disk_gb"`
	MinVRAMGB       int    `json:"min_vram_gb"`
	// MinCUDA is the CUDA version the image's own NVIDIA_REQUIRE_CUDA demands
	// (GPU-112). Placement must not put the template on a host whose driver
	// cannot run it: the provider-wide floor is a baseline for the pool, not a
	// promise about any one image, and a host that clears the baseline while
	// falling short of this one boots into "the NVIDIA driver on your system
	// is too old". Zero means "no template-specific floor" — the provider
	// baseline alone applies, which is the pre-GPU-112 behaviour.
	//
	// Enforced only by adapters with per-host placement on marketplace
	// capacity (where host drivers vary); other adapters ignore it. Absent for images whose base declares no
	// requirement we can read — that is "unknown", not "none".
	MinCUDA            float64   `json:"min_cuda,omitempty"`
	MinGPUCount        int       `json:"min_gpu_count"`
	ReadyTimeoutSecond int       `json:"ready_timeout_seconds"` // A5
	Ports              []Port    `json:"ports"`
	Env                []EnvSpec `json:"env"`
	StartCommand       string    `json:"start_command"` // INTERNAL — omit from wire (TMPL-13)
	DocsURL            string    `json:"docs_url"`
	// Description + Includes are customer-facing catalog metadata (D-64-03):
	// a one-line blurb and a short "what's inside" list, surfaced on the deploy
	// hub cards, the public catalog API, and the CLI. Optional — legacy entries
	// without them simply omit the fields on the wire.
	Description string   `json:"description,omitempty"`
	Includes    []string `json:"includes,omitempty"`
}

type catalogFile struct {
	Version   int             `json:"version"`
	Templates []*TemplateSpec `json:"templates"`
}

// Catalog is an immutable-once-loaded in-memory map of templates. Reload swaps
// the contents atomically — in-flight requests holding the old *TemplateSpec
// pointer continue safely because pointers are stable after Load.
type Catalog struct {
	mu    sync.RWMutex
	byID  map[string]*TemplateSpec
	specs []*TemplateSpec
}

// Load parses the catalog JSON and returns an immutable Catalog. It mirrors the
// inference catalog's Load control flow: version==1 gate, dup-id reject,
// per-entry validation. A spec with status=="coming_soon" may omit its image
// and skips the remaining eligibility/port checks — the escape hatch lets the
// operator advertise an upcoming template without a built image (mirrors
// internal/inference/catalog/catalog.go:348-350).
func Load(raw []byte) (*Catalog, error) {
	var f catalogFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("catalog: parse JSON: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("catalog: unsupported version %d (want 1)", f.Version)
	}
	byID := make(map[string]*TemplateSpec, len(f.Templates))
	for _, s := range f.Templates {
		if s.ID == "" {
			return nil, fmt.Errorf("catalog: template with empty id")
		}
		if _, dup := byID[s.ID]; dup {
			return nil, fmt.Errorf("catalog: duplicate template id %q", s.ID)
		}
		// v1 DEPLOYS only long-lived "service" templates (a listening port the
		// deploy entrypoint can health-check). A coming_soon placeholder MAY
		// advertise a future, not-yet-deployable kind (e.g. "finetuning", a
		// run-to-completion job with no app_url) — it renders as a non-interactive
		// Hub card and never reaches the deploy path, so it skips the service-only
		// checks below. A non-service kind that is NOT coming_soon is still
		// rejected: there is no lifecycle to run it.
		if s.Kind != "service" && s.Status != "coming_soon" {
			return nil, fmt.Errorf("catalog: template %q has unsupported kind %q (want service)", s.ID, s.Kind)
		}
		// coming_soon escape hatch: a placeholder template advertises an
		// upcoming app in /v1/templates without an active image behind it.
		// Such entries skip the image-required + eligibility/port checks.
		if s.Image == "" && s.Status != "coming_soon" {
			return nil, fmt.Errorf("catalog: template %q has no image (need image unless status=coming_soon)", s.ID)
		}
		if s.Status == "coming_soon" {
			byID[s.ID] = s
			continue
		}
		// Image must be digest-pinned. The freshness workflow computes
		// DATE_TAG="$(date -u +%Y-%m)" and pushes to that SAME tag every week,
		// so a bare `:2026-09` means "whatever the latest September build
		// produced" — a cron rebuild would change what production launches with
		// no code change and no review, and any CUDA verification of the
		// catalog would expire within days. The digest is what resolves at pull
		// time; the tag rides along for readability. Mirrors the base-image
		// pinning (docker/base-image/README.md) and tools/bump_template_image.py's
		// _IMAGE_RE, which enforces the same shape in CI — kept in lockstep so
		// the two validators cannot drift apart.
		if !imageDigestRe.MatchString(s.Image) {
			return nil, fmt.Errorf(
				"catalog: template %q image %q is not digest-pinned (want <repo>:<tag>@sha256:<64-hex>); "+
					"a bare tag is mutable and lets an image rebuild change what production launches",
				s.ID, s.Image)
		}
		// Port validation: exactly one Primary, each port in range, known proto.
		primaryCount := 0
		for i, p := range s.Ports {
			if p.Primary {
				primaryCount++
			}
			if p.Port < 1 || p.Port > 65535 {
				return nil, fmt.Errorf("catalog: template %q port[%d] %d out of range (1-65535)", s.ID, i, p.Port)
			}
			switch p.Protocol {
			case "http", "tcp":
				// ok
			default:
				return nil, fmt.Errorf("catalog: template %q port[%d] unknown protocol %q (want http|tcp)", s.ID, i, p.Protocol)
			}
		}
		if primaryCount != 1 {
			return nil, fmt.Errorf("catalog: template %q must have exactly one primary port (got %d)", s.ID, primaryCount)
		}
		// Env validation: every entry needs a non-empty key.
		for i, e := range s.Env {
			if e.Key == "" {
				return nil, fmt.Errorf("catalog: template %q env[%d] has empty key", s.ID, i)
			}
			// MD-05: reject keys reserved by the GPU.ai bootstrap (the GPUAI_*
			// control vars + PUBLIC_KEY). The provider adapter merges these
			// internal vars with "internal wins", so a colliding template key
			// would be SILENTLY dropped at deploy time and the app would boot
			// without the env it declared. Fail here, at the source of truth,
			// rather than letting the drop pass unobserved.
			if e.Key == "PUBLIC_KEY" || strings.HasPrefix(e.Key, "GPUAI_") {
				return nil, fmt.Errorf("catalog: template %q env[%d] key %q is reserved by the GPU.ai bootstrap", s.ID, i, e.Key)
			}
		}
		// Resource-floor sanity (Pitfall 2: containerDisk must be set or the
		// service ENOSPCs on boot). MinVRAM/MinGPU must be at least 1.
		if s.ContainerDiskGB < MinDiskGB {
			return nil, fmt.Errorf("catalog: template %q container_disk_gb %d below floor (>=%d)", s.ID, s.ContainerDiskGB, MinDiskGB)
		}
		if s.MinVRAMGB < 1 {
			return nil, fmt.Errorf("catalog: template %q min_vram_gb %d below floor (>=1)", s.ID, s.MinVRAMGB)
		}
		if s.MinGPUCount < 1 {
			return nil, fmt.Errorf("catalog: template %q min_gpu_count %d below floor (>=1)", s.ID, s.MinGPUCount)
		}
		// min_cuda is optional (0 = no template floor), but a typo — 128 for
		// 12.8 — would load clean and silently take this template's capacity to
		// zero, surfacing only as ErrNoCapacity long after the fact. Bound it to
		// plausible CUDA versions so the mistake fails at load instead.
		if s.MinCUDA != 0 && (s.MinCUDA < minPlausibleCUDA || s.MinCUDA > maxPlausibleCUDA) {
			return nil, fmt.Errorf("catalog: template %q min_cuda %g outside the plausible range (%g-%g)", s.ID, s.MinCUDA, minPlausibleCUDA, maxPlausibleCUDA)
		}
		byID[s.ID] = s
	}
	return &Catalog{byID: byID, specs: f.Templates}, nil
}

// Get resolves an id to a template. Returns (nil, false) for unknown ids —
// the handler emits a 404 in that case.
func (c *Catalog) Get(id string) (*TemplateSpec, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.byID[id]
	if !ok || s == nil {
		return nil, false
	}
	return s, true
}

// List returns a copy of the loaded templates. Templates have no modality
// axis, so unlike the inference catalog this takes no filter argument.
func (c *Catalog) List() []*TemplateSpec {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*TemplateSpec, len(c.specs))
	copy(out, c.specs)
	return out
}

// Reload atomically swaps the catalog contents. Used by the SIGHUP handler in
// cmd/gpuctl/serve.go. On a bad payload it returns the Load error and leaves
// the existing catalog intact — in-flight requests holding the old
// *TemplateSpec pointer continue safely.
func (c *Catalog) Reload(raw []byte) error {
	next, err := Load(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.byID = next.byID
	c.specs = next.specs
	c.mu.Unlock()
	return nil
}
