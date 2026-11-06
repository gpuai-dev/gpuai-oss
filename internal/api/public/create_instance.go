// CreateInstance is the HTTP-free create-instance path (D-28).
//
// handleCreateInstance used to BE this code: ~700 lines of validation gates
// interleaved with problem-details writes to a ResponseWriter and
// `r.Context()` reads.
// That shape has exactly one caller — an inbound HTTP request — and a second
// caller (the auto-launch firer, which replays a customer's frozen launch spec
// when a price alert trips) cannot reach it without fabricating an
// `http.Request` and parsing a problem-details body back out of a
// `ResponseWriter`.
//
// The alternative — hand-rolling "the important checks" in the firer — is the
// GPU-55 lesson verbatim: the subset silently drifts from the real gate set,
// and a stale image / revoked SSH key / removed template that a live create
// would refuse gets launched at 3 a.m. instead (D-24 requires those to FAIL
// LOUDLY, never to be silently substituted). So the gates live here, once, and
// both callers run all of them in the same order.
//
// The split is deliberately drawn at the HTTP boundary and nowhere else:
//
//   - handleCreateInstance keeps what is genuinely HTTP — reading the API key
//     off the request context, decoding the JSON body, and turning the result
//     (or a *CreateInstanceError) back into a status code and problem body.
//   - CreateInstance keeps every gate, in the original order, with every
//     status/type/detail string unchanged. TestCreateInstanceValidationParity
//     asserts mechanically that the two callers reject identical input
//     identically, so a future gate added to one is a test failure, not a
//     production divergence.
//
// Operation-id seam for the firer (review B CR-01): CreateInstanceWithOperationID
// lets a background caller supply the operation's primary key. It exists because
// this function creates the operation row and then kicks the engine on a
// DETACHED goroutine, so by the time it returns the provider call is already in
// flight. A caller that must record "this rule started this launch" durably
// BEFORE any money can move cannot wait for the returned id — it commits to an
// id, writes its own link, and passes the id in. A checkpoint that fails before
// the call aborts for $0; one that fails after it cannot undo a purchase.
// CreateInstance delegates with an empty id, so no existing caller and no HTTP
// surface changes, and the id is deliberately NOT a field on
// CreateInstanceRequest — it is not customer-supplied.
//
// KEYLESS PRINCIPAL (review A CR-02). apiKey.APIKeyID may legitimately be "".
// A background caller replaying a rule that was consented to on the DASHBOARD
// holds no API key — the consent was an org admin's role, autolaunch_rules
// stores creator_api_key_id as NULL for that surface by schema CHECK, and the
// firer builds its principal accordingly. So the operation row this function
// writes binds api_key_id NULL (migration v100 made the column nullable), and
// that row is deliberately invisible to the issuer-scoped GET /v1/operations/{id};
// the firer reads it back through the org-scoped auto-launch read instead.
// Do NOT "fix" the empty id into a required one, and do not borrow some other
// key to fill it: before v100 the empty string was bound against a
// `UUID NOT NULL` column, which failed the cast on EVERY dashboard-consented
// launch and expired the customer's standing purchase order with a generic 500.
// OrgID and UserID are still required and still carry the authority.
//
// Scope note for the firer (T-86-08): scope enforcement for the HTTP surface
// lives in the route's middleware chain (chainWrite), NOT in this function. A
// background caller must therefore populate APIKey.Scopes from the rule's
// stored creator key and apply its own scope check — CreateInstance will not
// do it for you. It does enforce org identity: apiKey is required, and its
// OrgID/UserID/Role thread into provision.ProvisionRequest, so the burn cap and
// spending limit apply to a fired launch exactly as to a typed one (T-86-06).
package public

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/authz"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/imageref"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
	"github.com/gpuai-dev/gpuai-oss/internal/registrycheck"
	tmplcatalog "github.com/gpuai-dev/gpuai-oss/internal/template/catalog"
)

// CreateInstanceError is a rejected create carrying the HTTP problem shape the
// handler used to write inline. Status/Type/Detail are the exact three
// arguments the corresponding problem-details write passed, so the thin handler
// reproduces the pre-extraction response byte-for-byte.
//
// A background caller reads the same three fields as structured data: Type is
// the stable machine code to branch on (e.g. "image_unavailable" for a deleted
// image, "validation_failed" for a removed template) and Detail is the sentence
// to put in the abort notification. Do NOT parse Detail — it is prose and it
// changes.
type CreateInstanceError struct {
	// Status is the HTTP status the API surface returns (400/422/402/502/...).
	Status int
	// Type is the stable problem-details type token ("validation_failed",
	// "insufficient_balance", "image_unavailable", ...).
	Type string
	// Detail is the human-readable explanation.
	Detail string
}

func (e *CreateInstanceError) Error() string {
	return e.Type + ": " + e.Detail
}

// CreateInstance runs every create-instance gate and, if all of them pass,
// creates the operation row and kicks off the provisioning goroutine — exactly
// what POST /v1/instances does, minus the HTTP.
//
// apiKey is required and must carry the org the instance is billed to; a nil
// key is refused with the same 401 the handler emits (a background caller must
// synthesize one from the stored creator, never a wildcard — T-86-06).
//
// req is taken BY VALUE: the template gpu_count default below mutates it, and a
// caller replaying a frozen spec must not see its stored spec change underneath
// it.
//
// The returned *Operation is the accepted 202 body, including any fail-open
// advisories in Warnings. A rejection is a *CreateInstanceError; anything else
// is an internal failure.
func (deps PublicDeps) CreateInstance(ctx context.Context, apiKey *APIKey, req CreateInstanceRequest) (*Operation, error) {
	return deps.CreateInstanceWithOperationID(ctx, apiKey, req, "")
}

// CreateInstanceWithOperationID is CreateInstance with the operation's primary
// key supplied by the caller.
//
// An empty operationID means "assign one", which is what CreateInstance passes
// and therefore what every HTTP request gets. A non-empty one must be a UUID the
// caller has ALREADY committed to somewhere durable — see the header note: the
// auto-launch firer writes the rule-to-operation link before this call so that a
// kill or a database outage between here and the provider cannot leave a spend
// with no record of it. The id is not read from, and cannot be set through, the
// request body.
func (deps PublicDeps) CreateInstanceWithOperationID(ctx context.Context, apiKey *APIKey, req CreateInstanceRequest, operationID string) (*Operation, error) {
	// The HTTP handler checks this first and never reaches here with nil, but a
	// background caller has no middleware in front of it. Every gate below
	// scopes to apiKey.OrgID, so a nil key is an unauthenticated spend path,
	// not a nil-pointer bug (T-86-06).
	if apiKey == nil {
		return nil, &CreateInstanceError{Status: http.StatusUnauthorized, Type: "unauthenticated", Detail: "Authentication required"}
	}
	// Template deploys default gpu_count to 1 when the client omits it. The
	// deploy DTO (apiclient.DeployTemplateRequest) carries no gpu_count field —
	// a `gpu deploy comfyui` would otherwise arrive with gpu_count=0 and be
	// rejected by Validate's 1..8 check, so the headline `gpu deploy` path
	// would 422 for every single-GPU template. This defaults BEFORE Validate
	// so the request still satisfies the 1..8 invariant (not a bypass), and
	// only when the client omitted the count — an explicit multi-GPU template
	// deploy (gpu_count >= 1) is left untouched. Raw instances (TemplateID nil)
	// are unaffected and still require gpu_count. (BLOCK-08 contract fix —
	// proven by cmd/gpu/deploy_integration_test.go.)
	if req.TemplateID != nil && req.GPUCount == 0 {
		req.GPUCount = 1
	}
	if err := req.Validate(); err != nil {
		return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: err.Error()}
	}
	// Phase 63 (T-63-01): cheap format gate for environment selection. A bad
	// `environment` enum or malformed `offering_id` is a 400 client-format error,
	// checked before the catalog-dependent capability gate (422) below.
	if err := req.validateEnvironmentFormat(); err != nil {
		return nil, &CreateInstanceError{Status: http.StatusBadRequest, Type: "validation_failed", Detail: err.Error()}
	}
	// Custom image (Phase 1): the ref grammar is the same class of cheap,
	// catalog-independent format gate — a malformed reference is a 400. The
	// parse is also the FIRST trust gate: past here the value is structurally
	// incapable of carrying shell/TOML/JSON metacharacters (see imageref).
	var customImage *imageref.Ref
	if req.Image != nil {
		ref, perr := imageref.Parse(*req.Image)
		if perr != nil {
			return nil, &CreateInstanceError{Status: http.StatusBadRequest, Type: "validation_failed", Detail: perr.Error()}
		}
		customImage = &ref
	}
	// Disk ceiling. Same 422 validation_failed class as the floor in Validate();
	// it sits here only because it needs config to know the bound. Still cheap
	// (no I/O), so it runs with the other field-level checks rather than down
	// with the catalog gates. A nil Config resolves the documented default via
	// the nil-safe accessor, so DB-less unit tests bound at 250 rather than 0.
	if req.DiskGB != nil {
		if maxDisk := deps.Config.MaxDiskGB(); *req.DiskGB > maxDisk {
			return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: fmt.Sprintf("disk_gb must be at most %d", maxDisk)}
		}
	}

	// Reject an unknown gpu_type synchronously (issue #99) instead of accepting
	// it with a 202 that fails ~7s later in the async provision path with the
	// misleading "no suitable provider available". The valid set is the live
	// availability cache — identical to GET /v1/gpu-types. Skipped for template
	// deploys (the engine auto-selects the GPU) and when gpu_type is omitted.
	// Fails OPEN on a cache miss/error so a transient catalog outage can't block
	// every launch; a genuinely-offered-but-sold-out type stays listed and still
	// flows to the async path. Runs BEFORE the operation row is created, so a
	// rejected request leaves no operation or instance row.
	if req.TemplateID == nil && req.GPUType != "" && deps.AvailCache != nil {
		if known, err := deps.AvailCache.KnownGPUModels(ctx); err == nil && len(known) > 0 {
			if _, ok := known[req.GPUType]; !ok {
				return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: "Unknown gpu_type \"" + req.GPUType + "\" — not currently offered; see GET /v1/gpu-types for available types"}
			}
		}
	}

	// Custom image capability gate (Phases 1+3): a 422 when NO offering of
	// the requested gpu_type can host a customer image — container-native
	// capacity runs it natively; VM capacity qualifies when it can take the
	// bootstrap docker-run path (the same server-derived CertifiedImage
	// classification the environment gate uses), but NOT for private images
	// (a credential must never ride the staged bootstrap, so those stay
	// container-only — mirroring the engine's narrowing). Fails OPEN on a
	// cache miss/error (the engine is the real boundary; this is UX), and
	// container-native-but-excluded providers still pass here — the
	// provider-anonymized offering rows can't distinguish them.
	if customImage != nil && deps.AvailCache != nil {
		if offs, err := deps.AvailCache.Read(ctx); err == nil {
			var sawModel, canHonor bool
			for i := range offs {
				if offs[i].GPUModel != req.GPUType {
					continue
				}
				sawModel = true
				if offs[i].DeploymentType == "container" {
					canHonor = true
					break
				}
				if req.RegistryCredentialID == nil && offs[i].DeploymentType == "vm" && offs[i].CertifiedImage {
					canHonor = true
					break
				}
			}
			if sawModel && !canHonor {
				msg := "custom images aren't available for this machine — pick another GPU type"
				if req.RegistryCredentialID != nil {
					msg = "private custom images need container-native capacity, and this machine only has VM capacity — pick a container-native GPU type or make the image public"
				}
				return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "image_unavailable", Detail: msg}
			}
		}
	}

	// Phase 63 (T-63-01): environment capability gate. `environment` is untrusted
	// client input; a well-formed value that the PLACED provider cannot honor is
	// a 422 — re-validated here against the availability catalog's server-derived
	// classification (DeploymentType / CertifiedImage, computed at cache-write
	// time from provider capability, never from client input). raw_vm needs a VM
	// offering we can bootstrap; certified needs an offering the Certified Image
	// can run on; provider_template is always honorable. The gate rejects only
	// when NO offering of the requested gpu_type can honor the class — if any can,
	// the engine picks a capable one. Fails OPEN on a cache miss/error so a
	// transient catalog outage can't block launches; the engine re-derives
	// capability independently (defense in depth, T-63-09). Skipped for template
	// deploys (engine auto-selects the GPU) and the nil-default path.
	if req.EnvironmentSelection != nil && req.TemplateID == nil && deps.AvailCache != nil {
		// A certified:<framework> sub-choice (D-64-01) rides the certified class
		// for capability purposes — the framework layer runs anywhere the
		// certified base runs. A raw_vm:<os> sub-choice (the launch OS picker)
		// narrows further: the offering must also list the os id in its
		// server-computed os_options.
		envClass, envSub := provision.ParseEnvironmentSelection(*req.EnvironmentSelection)
		if offs, err := deps.AvailCache.Read(ctx); err == nil {
			var sawModel, canHonor bool
			for i := range offs {
				o := offs[i]
				if o.GPUModel != req.GPUType {
					continue
				}
				sawModel = true
				switch envClass {
				case "raw_vm":
					if o.DeploymentType == "vm" && (envSub == "" || slices.Contains(o.OSOptions, envSub)) {
						canHonor = true
					}
				case "certified":
					if o.CertifiedImage {
						canHonor = true
					}
				case "provider_template":
					canHonor = true
				}
			}
			if sawModel && !canHonor {
				msg := "the selected environment isn't available for this machine"
				switch envClass {
				case "raw_vm":
					msg = "this machine is container-native — root VM access isn't available"
					if envSub != "" {
						msg = "the selected operating system isn't available for this machine — pick another OS or the provider default"
					}
				case "certified":
					msg = "this provider launches from its own template; the GPU.ai Certified Image isn't available"
				}
				return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "environment_unavailable", Detail: msg}
			}
		}
	}

	// Phase 64 (D-64-01): framework roster gate. A well-formed
	// certified:<framework> whose framework the server-controlled manifest
	// cannot resolve for this GPU (unknown id, or image not yet published) is
	// rejected synchronously with a 422 — not a 202 that dies minutes later in
	// the async provision path. Fails OPEN when the manifest or the GPU's
	// manifest row is unavailable: the engine re-derives the pinned ref and is
	// the trust boundary (V5); this gate is early-reject UX only.
	if req.EnvironmentSelection != nil && req.TemplateID == nil {
		// The sub token is a framework ONLY on the certified class — a raw_vm
		// sub is an os id and has its own gates (os_options above, vm_images
		// below), so gate on the parsed class, never on sub != "" alone.
		if class, sub := provision.ParseEnvironmentSelection(*req.EnvironmentSelection); class == "certified" && sub != "" {
			fw, ver := provision.ParseFrameworkVersion(sub)
			if m := loadedManifest(); m != nil {
				if ge, ok := m.Lookup(strings.ReplaceAll(req.GPUType, "_", " ")); ok {
					// A `certified:<fw>@<ver>` pick (the launch version picker) must
					// resolve for THIS GPU's CUDA variant; a bare `certified:<fw>`
					// checks the family roster as before. Either miss is a 422.
					published := false
					if ver != "" {
						_, published = m.FrameworkImageRefForVersion(fw, ge.CUDAMin, ver)
					} else {
						_, published = m.FrameworkImageRef(fw, ge.CUDAMin)
					}
					if !published {
						return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "environment_unavailable", Detail: "the selected framework environment isn't available for this machine — pick another framework, version, or the certified base image"}
					}
				}
			}
		}
	}

	// OS catalog gate (the launch OS picker): a well-formed raw_vm:<os> whose
	// os id the server-controlled vm_images catalog doesn't know at all is
	// rejected synchronously with a 422 — not a 202 that dies in the async
	// provision path. Per-provider availability is the os_options capability
	// gate above; this catches ids outside the catalog entirely. Fails OPEN
	// when the manifest is unavailable: the engine re-derives and refuses (V5);
	// this gate is early-reject UX only.
	if req.EnvironmentSelection != nil && req.TemplateID == nil {
		if class, osID := provision.ParseEnvironmentSelection(*req.EnvironmentSelection); class == "raw_vm" && osID != "" {
			if m := loadedManifest(); m != nil && !m.HasVMImage(osID) {
				return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "environment_unavailable", Detail: "the selected operating system isn't available — pick another OS or the provider default"}
			}
		}
	}

	// Disk capability gate. An explicit disk_gb that NO offering of the requested
	// gpu_type can honor is a 422 now, rather than a 202 that dies minutes later
	// in the async provision path with the same ErrDiskUnavailable. Shaped exactly
	// like the environment capability gate above:
	//
	//   - It reads disk_configurable, a SERVER-derived catalog attribute computed
	//     at cache-write from provider capability — never client input.
	//   - It rejects only when no offering of the model can honor the request; if
	//     any can, the engine narrows placement to it.
	//   - It fails OPEN on a cache miss/error, and is skipped when the model is
	//     absent from the catalog entirely. The engine re-derives configurability
	//     and is the trust boundary; this gate is early-reject UX only.
	//   - Template deploys are skipped (the engine auto-selects the GPU).
	//
	// The message is provision.ErrDiskUnavailable verbatim, so the synchronous
	// rejection and the asynchronous one are the same sentence to the customer —
	// and the dashboard handler sources the same var, so the two surfaces cannot
	// drift into two different explanations of one condition.
	if req.DiskGB != nil && deps.AvailCache != nil {
		// One catalog read serves both disk gates below — this is the hot
		// launch path, and the second gate re-reading the cache would be a
		// gratuitous extra round-trip.
		if offs, err := deps.AvailCache.Read(ctx); err == nil {
			if req.TemplateID == nil && req.GPUType != "" {
				var sawModel, canHonor bool
				for i := range offs {
					if offs[i].GPUModel != req.GPUType {
						continue
					}
					sawModel = true
					if offs[i].DiskConfigurable {
						canHonor = true
						break
					}
				}
				if sawModel && !canHonor {
					return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "disk_unavailable", Detail: provision.ErrDiskUnavailable.Error()}
				}
			}

			// Pinned-offering disk cap gate (the 2026-08-11 marketplace clamp
			// defect): a launch pinned to one exact offering whose displayed
			// storage_gb cannot fit the explicit disk_gb is refused NOW — the
			// upstream would not refuse it for us (it accepts the create, bills
			// the requested size, and silently clamps the container to the
			// host's capacity). Shaped like its neighbours: the catalog row is
			// SERVER-derived, the engine re-derives the same cap and is the
			// trust boundary (ErrDiskExceedsOffering), and every uncertain case
			// fails OPEN — pin absent from this poll cycle (whether the pin
			// still exists is the engine's call, D-63-08), or a row with no
			// storage figure (StorageGB == 0 is "unreported", never "none").
			// Rows that cannot configure disk at all are the OTHER gate's
			// condition — firing this one there would tell the customer to
			// lower disk_gb on a machine where no disk_gb works. The message is
			// provision.ErrDiskExceedsOffering verbatim so the synchronous and
			// asynchronous rejections are the same sentence.
			if req.OfferingID != nil {
				for i := range offs {
					if offs[i].OfferingID != *req.OfferingID {
						continue
					}
					if offs[i].DiskConfigurable && offs[i].StorageGB > 0 && *req.DiskGB > offs[i].StorageGB {
						return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "disk_exceeds_offering", Detail: provision.ErrDiskExceedsOffering.Error()}
					}
					break
				}
			}
		}
	}

	// Template deploys resolve the spec, validate env overrides against the
	// user_overridable allowlist, and build the effective env. The resolved
	// spec also drives the SECRET REDACTION of the metadata snapshot (TMPL-13)
	// and the template threading into the provision request below. metaReq is
	// what gets marshalled into operation.metadata — for a template deploy it
	// is a sanitized copy with every secret env value replaced by [REDACTED].
	metaReq := req
	var spec *tmplcatalog.TemplateSpec
	var templateEnv []provider.EnvVar
	// tmplGPUCount is the effective GPU count for a template deploy: the
	// requested count floored to the template's min_gpu_count (at least 1).
	// Computed here because both the model-VRAM guard and the provReq
	// threading below need the same number (CR-01).
	var tmplGPUCount int
	// guardWarnings/guardMinVRAMGB carry the model-size guard's verdict from
	// the template block into the provision request and the 202 response.
	var guardWarnings []string
	var guardMinVRAMGB int
	if req.TemplateID != nil {
		var ok bool
		spec, ok = deps.TemplateCatalog.Get(*req.TemplateID)
		if !ok {
			return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: "Unknown template " + *req.TemplateID}
		}
		effEnv, verr := buildTemplateEnv(spec, req.Env)
		if verr != nil {
			return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: verr.Error()}
		}
		templateEnv = effEnv
		// TMPL-13: marshal a SANITIZED copy of the request for the snapshot.
		// The raw secret env still flows into provReq below for the real
		// deploy — only the persisted/logged snapshot is redacted.
		metaReq = redactSecretEnv(req, spec)

		// HG-02: a service template (one with a primary HTTP port) is
		// unreachable without a configured apps host — the vhost tunnel would
		// come up but no app_url could ever be produced, so the deploy would
		// "succeed" with a basic-auth credential for a URL that never exists.
		// Fail fast with a clear 503 BEFORE creating the operation row (no
		// dangling op) rather than half-deploying. Raw provisioning, and
		// SSH-only templates with no app port, are unaffected.
		if primaryPort(spec.Ports) != 0 && deps.appsHost() == "" {
			return nil, &CreateInstanceError{Status: http.StatusServiceUnavailable, Type: "service_unavailable", Detail: "Template deploys are unavailable: the apps host (APPS_HOST) is not configured"}
		}

		tmplGPUCount = req.GPUCount
		if tmplGPUCount < spec.MinGPUCount {
			tmplGPUCount = spec.MinGPUCount
		}
		if tmplGPUCount < 1 {
			tmplGPUCount = 1
		}

		// GPU-87: template VRAM floor vs. an EXPLICIT gpu_type. Auto-select has
		// filtered by the floor since TMPL-04, but a named gpu_type took the
		// engine's exact-match branch and skipped it — so sglang (min_vram_gb
		// 24) was accepted onto 16 GB rtx_a4000s with a 202 and died on boot.
		// Reject synchronously, before the operation row exists, with a message
		// naming both numbers AND the compatible types so the fix does not
		// require guessing.
		//
		// The floor used is the same effective floor threaded to
		// provReq.MinVRAMGB below — same function, same inputs as the engine's
		// backstop, so the two layers cannot disagree.
		if req.GPUType != "" {
			effFloor := spec.MinVRAMGB
			if guardMinVRAMGB > effFloor {
				effFloor = guardMinVRAMGB
			}
			if verr := provision.TemplateVRAMShortfall(*req.TemplateID, effFloor, provider.GPUType(req.GPUType)); verr != nil {
				return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "gpu_vram_too_small", Detail: verr.Error()}
			}
		}
	}

	// Custom image (Phase 1): env gate + registry pre-flight, synchronously and
	// BEFORE the operation row exists — a bad image name, a private image, or
	// an arm-only image is a request defect, not a provisioning failure, and
	// refusing it here means no dangling operation, no reserved GPU, and no
	// bill. The pre-flight also captures the image config's Entrypoint/Cmd
	// (the bootstrap launches them — the SSH contract) and the manifest digest
	// (the launch is pinned to exactly the bytes verified here, so a tag
	// re-push between pre-flight and pull cannot swap the image: TOCTOU).
	var imageStart *registrycheck.Result
	var customEnv []provider.EnvVar
	var customImagePinned string
	var registryAuth *provider.RegistryAuth
	if customImage != nil {
		env, verr := BuildCustomImageEnv(req.Env)
		if verr != nil {
			return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: verr.Error()}
		}
		customEnv = env
		// Private registry credential (Phase 2): resolve + unseal the stored
		// login. The plaintext exists only in this request's memory — it rides
		// the pre-flight and the provision request, never the operation
		// snapshot (which carries the id alone) and never a log line.
		var imageCreds *registrycheck.Credentials
		if req.RegistryCredentialID != nil {
			if deps.RegistryCredSealer == nil || !deps.RegistryCredSealer.Enabled() {
				return nil, &CreateInstanceError{Status: http.StatusServiceUnavailable, Type: "registry_credentials_unavailable", Detail: "Private registry credentials are not enabled on this environment yet"}
			}
			cred, sealed, gerr := deps.resolveRegistryCredentialStore().GetRegistryCredentialSealed(ctx, apiKey.OrgID, *req.RegistryCredentialID)
			if errors.Is(gerr, db.ErrRegistryCredentialNotFound) {
				return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: "Unknown registry_credential_id"}
			}
			if gerr != nil {
				return nil, &CreateInstanceError{Status: http.StatusInternalServerError, Type: "internal_error", Detail: "Failed to load registry credential"}
			}
			// The credential must belong to the image's registry — a Hub login
			// sent to ghcr.io would fail the pull anyway; refuse it up front.
			if cred.Registry != customImage.Registry {
				return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "validation_failed", Detail: fmt.Sprintf("registry credential %q is for %s, but the image is on %s", cred.Name, cred.Registry, customImage.Registry)}
			}
			password, oerr := deps.RegistryCredSealer.Open(cred.CredentialID, sealed)
			if oerr != nil {
				slog.ErrorContext(ctx, "instances: registry credential unseal failed", "credential_id", cred.CredentialID)
				return nil, &CreateInstanceError{Status: http.StatusInternalServerError, Type: "internal_error", Detail: "Failed to load registry credential"}
			}
			imageCreds = &registrycheck.Credentials{Username: cred.Username, Password: password}
			registryAuth = &provider.RegistryAuth{Registry: cred.Registry, Username: cred.Username, Password: password}
		}
		// HG-02 for custom images: a primary http port needs the apps host to
		// ever produce an app_url — fail fast BEFORE the operation row, the
		// same guard service templates get.
		for _, p := range req.Ports {
			if p.Primary && deps.appsHost() == "" {
				return nil, &CreateInstanceError{Status: http.StatusServiceUnavailable, Type: "apps_host_unconfigured", Detail: "App URLs are not available on this environment; retry without a primary port"}
			}
		}
		res, cerr := deps.registryChecker().Check(ctx, *customImage, imageCreds)
		if cerr != nil {
			if errors.Is(cerr, registrycheck.ErrUnreachable) {
				// The registry being down is neither our fault nor the
				// customer's — a 502 tells them (and SDK retry logic) to try
				// again rather than fix the request.
				return nil, &CreateInstanceError{Status: http.StatusBadGateway, Type: "registry_unreachable", Detail: cerr.Error()}
			}
			return nil, &CreateInstanceError{Status: http.StatusUnprocessableEntity, Type: "image_unavailable", Detail: cerr.Error()}
		}
		imageStart = &res
		// Pin UNCONDITIONALLY: Check() computes the digest from manifest
		// content, so an empty digest is an internal invariant break — fail
		// the request rather than fall back to an unpinned (mutable-tag)
		// launch, which would silently void the TOCTOU guarantee.
		if res.Digest == "" {
			slog.ErrorContext(ctx, "instances: registry pre-flight returned empty digest", "image", customImage.String())
			return nil, &CreateInstanceError{Status: http.StatusInternalServerError, Type: "internal_error", Detail: "Failed to verify image"}
		}
		customImagePinned = customImage.Pinned(res.Digest)
	}

	// Snapshot the (sanitized for template deploys) request body into
	// operation.metadata so operators can reproduce a failed provision from the
	// FSM row alone (no log dive needed). Marshal failure is unreachable for a
	// valid struct — but guard the error so a future field that breaks
	// json.Marshal does not panic the goroutine.
	meta, mErr := json.Marshal(metaReq)
	if mErr != nil {
		slog.ErrorContext(ctx, "instances: marshal request snapshot failed", "error", mErr.Error())
		return nil, &CreateInstanceError{Status: http.StatusInternalServerError, Type: "internal_error", Detail: "Failed to start operation"}
	}

	store := deps.resolveOperationStore()
	op := &db.Operation{
		// Empty for every HTTP caller, so Postgres assigns the id exactly as
		// before. Non-empty only for a background caller that already wrote this
		// id into its own durable record.
		OperationID: operationID,
		// Empty for the dashboard-consented auto-launch principal, which holds
		// no key by design; the column is NULLABLE since v100 and the binder
		// maps "" to NULL. See the KEYLESS PRINCIPAL note in the file header.
		APIKeyID: apiKey.APIKeyID,
		OrgID:    apiKey.OrgID,
		Kind:     "instance.create",
		Metadata: meta,
	}
	if err := store.CreateOperation(ctx, op); err != nil {
		slog.ErrorContext(ctx, "instances: create operation failed", "error", err.Error())
		return nil, &CreateInstanceError{Status: http.StatusInternalServerError, Type: "internal_error", Detail: "Failed to start operation"}
	}

	// Kick off engine asynchronously with INDEPENDENT context so a
	// disconnected client does not abort provisioning.
	engine := deps.resolveEngineRunner()
	provReq := provision.ProvisionRequest{
		OrgID:           apiKey.OrgID,
		UserID:          apiKey.UserID,
		IsAdmin:         authz.IsAdminPlus(apiKey.Role),
		GPUType:         provider.GPUType(req.GPUType),
		GPUCount:        req.GPUCount,
		Tier:            provider.InstanceTier(req.Tier),
		Regions:         regionsFromRequest(req),
		Name:            req.Name,
		SSHKeyIDs:       req.SSHKeyIDs,
		MaxPricePerHour: req.MaxPricePerHour,
		// The price the caller displayed before launching. nil / <= 0 is absent
		// and changes nothing; set, it can only refuse the launch — never raise
		// the cap or the bill (provision.ProvisionRequest.ViewedPricePerHour).
		ViewedPricePerHour: req.ViewedPricePerHour,
		// nil => the engine fills the platform default and narrows nothing; a
		// value => a requirement the engine enforces by narrowing candidates.
		// Threaded OUTSIDE the spec block below on purpose: a customer's disk
		// request is independent of whether this is a template deploy, and the
		// engine folds the template's container_disk_gb in as a FLOOR.
		RequestedDiskGB: req.DiskGB,
		// The per-instance runtime bound (GPU-60), validated above. The engine
		// persists the hours; the deadline is armed at the running transition.
		AutoTerminateHours: req.AutoTerminateHours,
	}
	// Phase 63: thread the validated environment selection + pinned offering into
	// the engine request. Both are re-derived/re-validated engine-side (T-63-01,
	// T-63-09) — the handler-side gates above are early-reject UX, not the trust
	// boundary. Empty string => legacy default path (server-chosen environment,
	// price-sorted placement).
	if req.EnvironmentSelection != nil {
		provReq.Environment = *req.EnvironmentSelection
	}
	if req.OfferingID != nil {
		provReq.SelectedOfferingID = *req.OfferingID
	}
	if spec != nil {
		// Thread the resolved template into the engine-internal request. The raw
		// (un-redacted) effective env flows here so the deployed container gets
		// real secret values; the metadata snapshot above carries the redacted
		// copy. AppPort != 0 switches the engine into vhost-proxy + basic-auth
		// mode (Task 2).
		provReq.TemplateID = req.TemplateID
		provReq.Image = spec.Image
		provReq.Ports = templatePortsToProvider(spec.Ports)
		provReq.Env = templateEnv
		provReq.ContainerDiskGB = spec.ContainerDiskGB
		provReq.AppReadyTimeout = spec.ReadyTimeoutSecond
		provReq.AppPort = primaryPort(spec.Ports)
		// GPU-112: the image's own driver requirement, enforced at placement so
		// the template cannot land on a host too old to run it. Mirrors the
		// dashboard path in internal/api/handlers.go — a template launched
		// through the public API must get the same guarantee. Ported here when
		// phase 86 extracted this block out of handleCreateInstance.
		provReq.MinCUDA = spec.MinCUDA
		// CR-01 GPU auto-selection. The template's per-GPU VRAM floor reaches the
		// engine for the eligibility filter (used only when gpu_type is omitted);
		// the GPU-count floor is folded into GPUCount so the engine's existing
		// candidate filter enforces it. A deploy MAY omit gpu_count (Validate
		// allows 0 for templates) — default it to the template's min_gpu_count
		// (at least 1). A larger explicit count is honored; a too-small one is
		// floored to the template's minimum so a template never lands under-spec.
		// GPU-40: the model-size guard may have raised the effective floor above
		// the template's static one, so auto-select only considers GPUs the
		// requested MODEL actually fits.
		provReq.MinVRAMGB = spec.MinVRAMGB
		if guardMinVRAMGB > provReq.MinVRAMGB {
			provReq.MinVRAMGB = guardMinVRAMGB
		}
		provReq.GPUCount = tmplGPUCount
	}
	// Custom image (Phase 1): thread the digest-pinned ref, the pre-flight's
	// captured Entrypoint/Cmd, and the customer env. CustomerImage — not a
	// non-empty Image — is what narrows placement (SupportsCustomerImage) and
	// switches the bootstrap's launch block, so the template path above stays
	// byte-for-byte unchanged. Ports stay empty: Phase 1 instances are
	// SSH/terminal-only (port declarations come with Phase 2).
	if customImage != nil {
		provReq.Image = customImagePinned
		provReq.CustomerImage = true
		provReq.Env = customEnv
		provReq.RegistryAuth = registryAuth
		// Entrypoint/Cmd (Phase 2): docker semantics — an entrypoint override
		// replaces the image ENTRYPOINT and drops the image CMD unless cmd is
		// also given; a cmd override alone replaces just the CMD. A CLEAR
		// (explicit empty array) keeps the image CMD, exactly like docker's
		// `--entrypoint ""` — dropping it too rendered an EMPTY launch
		// script, an SSH-only instance billing with its workload silently
		// never started (review 790).
		effEntrypoint, effCmd := imageStart.Entrypoint, imageStart.Cmd
		if req.Entrypoint != nil {
			effEntrypoint = *req.Entrypoint
			if len(effEntrypoint) > 0 {
				effCmd = nil
			}
		}
		if req.Cmd != nil {
			effCmd = *req.Cmd
		}
		provReq.ImageEntrypoint = effEntrypoint
		provReq.ImageCmd = effCmd
		// The CONFIG entrypoint threads separately (un-overridden) for the
		// engine's ENTRYPOINT placement steer: a container runtime that rides
		// the image entrypoint applies the CONFIG's, so a request override
		// changes what our launch script execs but never what placement must
		// avoid.
		provReq.ImageConfigEntrypoint = imageStart.Entrypoint
		// Ports (Phase 2): forwarded to the provider; the primary http port
		// additionally rides the template app plumbing (vhost proxy + basic
		// auth + app-ready probe) and is persisted for app_url derivation.
		if len(req.Ports) > 0 {
			ports := make([]provider.Port, 0, len(req.Ports))
			for _, p := range req.Ports {
				ports = append(ports, provider.Port{Port: p.Port, Protocol: p.Protocol, Primary: p.Primary})
				if p.Primary {
					provReq.AppPort = p.Port
				}
			}
			provReq.Ports = ports
		}
	}
	go runCreateInstance(store, engine, op, provReq)
	resp := operationToResponse(op)
	// GPU-40 fail-open advisories ride the synchronous 202 only (never
	// persisted — see Operation.Warnings).
	resp.Warnings = guardWarnings
	return &resp, nil
}
