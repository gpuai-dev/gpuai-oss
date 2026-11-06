package generic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/supplier"
)

// Compile-time check: Adapter implements provider.Provider.
var _ provider.Provider = (*Adapter)(nil)

// defaultDockerImage is launched when the request carries neither a
// customer image nor a pinned certified ref. Suppliers run the image
// verbatim and require it (the supplier spec marks docker_image optional,
// but real suppliers reject an absent one), so raw launches get a
// vendor-neutral CUDA image.
// CUDA 12.8 build: generic suppliers sell Blackwell (sm_120) cards, and
// torch builds before cu128 have no sm_120 kernels — the GPU shows up in
// nvidia-smi but every kernel launch fails ("no kernel image is available").
// Verified live on a supplier's RTX PRO 6000 with the previous 2.4.1-cu124 tag.
const defaultDockerImage = "pytorch/pytorch:2.7.1-cuda12.8-cudnn9-devel"

// SupplierLister is the subset of supplier.Store needed by the adapter.
type SupplierLister interface {
	ListActive(ctx context.Context) ([]supplier.Supplier, error)
	GetBySlug(ctx context.Context, slug string) (*supplier.Supplier, error)
}

// TokenFunc retrieves a bearer token for authenticating to a supplier.
// This is injected at construction time; Plan 03 wires the OAuth token flow.
// For now, a nil TokenFunc causes empty bearer tokens to be sent.
type TokenFunc func(ctx context.Context, s supplier.Supplier) (string, error)

// Adapter implements provider.Provider as a multiplexing adapter over
// all active generic suppliers. It reads supplier records from the store,
// makes HTTP calls to each supplier's REST endpoints, and maps responses
// to the standard provider types.
type Adapter struct {
	store     SupplierLister
	client    *Client
	logger    *slog.Logger
	tokenFunc TokenFunc

	// supplierOfferingMap caches which supplier offers a given GPU type + region + tier
	// combination. Populated by ListAvailable, read by Provision.
	// Key format: "gpuType|region|tier" -> supplierSlug
	supplierOfferingMap sync.Map
}

// NewAdapter creates a new generic supplier adapter.
// The store provides active supplier records. Logger may be nil (defaults to slog.Default).
// Client options are passed through to the underlying HTTP client.
func NewAdapter(store SupplierLister, logger *slog.Logger, opts ...ClientOption) *Adapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{
		store:  store,
		client: NewClient(opts...),
		logger: logger,
	}
}

// SetTokenFunc injects the function used to retrieve bearer tokens for supplier calls.
// This avoids a constructor dependency on the OAuth token cache (wired in Plan 03).
func (a *Adapter) SetTokenFunc(fn TokenFunc) {
	a.tokenFunc = fn
}

// Name returns the provider identifier. Always "generic" -- never leaks supplier identity.
func (a *Adapter) Name() string {
	return "generic"
}

// getToken retrieves a bearer token for a supplier, or returns empty string if no TokenFunc is set.
func (a *Adapter) getToken(ctx context.Context, s supplier.Supplier) string {
	if a.tokenFunc == nil {
		return ""
	}
	token, err := a.tokenFunc(ctx, s)
	if err != nil {
		a.logger.Error("failed to get supplier token",
			slog.String("supplier", s.Slug),
			slog.String("error", err.Error()),
		)
		return ""
	}
	return token
}

// offeringKey builds the sync.Map key for supplier offering routing.
func offeringKey(gpuType provider.GPUType, region string, tier provider.InstanceTier) string {
	return string(gpuType) + "|" + region + "|" + string(tier)
}

// ListAvailable polls all active suppliers concurrently for GPU offerings.
// Per-supplier failures are isolated: one slow/dead supplier does not block others (Pitfall 1).
// Returns combined offerings from all successful suppliers.
func (a *Adapter) ListAvailable(ctx context.Context) ([]provider.GPUOffering, error) {
	suppliers, err := a.store.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active suppliers: %w", err)
	}
	if len(suppliers) == 0 {
		return []provider.GPUOffering{}, nil
	}

	type result struct {
		offerings []provider.GPUOffering
		slug      string
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []result
	)

	for _, s := range suppliers {
		s := s // capture loop variable
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Per-supplier 10s timeout (Pitfall 1).
			sCtx, cancel := context.WithTimeout(ctx, listTimeout)
			defer cancel()

			token := a.getToken(sCtx, s)
			resp, err := a.client.GetAvailable(sCtx, s, token)
			if err != nil {
				a.logger.Error("supplier poll failed",
					slog.String("supplier", s.Slug),
					slog.String("error", err.Error()),
				)
				return
			}

			offerings := toGPUOfferings(resp, s.Slug)
			if len(offerings) == 0 {
				return
			}

			mu.Lock()
			results = append(results, result{offerings: offerings, slug: s.Slug})
			mu.Unlock()
		}()
	}

	wg.Wait()

	// Collect all offerings and populate the routing map.
	var all []provider.GPUOffering
	for _, r := range results {
		for _, o := range r.offerings {
			all = append(all, o)
			// Cache which supplier provides this offering for Provision routing.
			key := offeringKey(o.GPUType, o.Region, o.Tier)
			a.supplierOfferingMap.Store(key, r.slug)
		}
	}

	a.logger.Info("listed generic supplier offerings",
		slog.Int("total_offerings", len(all)),
		slog.Int("suppliers_polled", len(suppliers)),
	)

	return all, nil
}

// Provision creates a new GPU instance on the appropriate supplier.
// It uses the supplierOfferingMap (populated by ListAvailable) to route the request
// to the correct supplier based on GPU type, region, and tier.
func (a *Adapter) Provision(ctx context.Context, req provider.ProvisionRequest) (*provider.ProvisionResult, error) {
	// Look up which supplier offers this GPU type + region + tier.
	key := offeringKey(req.GPUType, req.Region, req.Tier)
	slugVal, ok := a.supplierOfferingMap.Load(key)
	if !ok {
		return nil, fmt.Errorf("no supplier found for %s in %s (%s): %w",
			req.GPUType, req.Region, req.Tier, provider.ErrNoCapacity)
	}
	slug := slugVal.(string)

	// Resolve supplier record.
	sup, err := a.store.GetBySlug(ctx, slug)
	if err != nil {
		// GPU-61: the community supplier slug is an upstream identity and must
		// never reach the customer (this error flows into
		// Operation.Error.Detail). Keep the slug out of the message.
		return nil, fmt.Errorf("supplier resolution failed: %w", err)
	}

	// V5: a server-controlled PinnedImageRef (digest-pinned certified image)
	// ALWAYS overrides the customer-supplied DockerImage — the customer image
	// must never reach the launch payload on a certified/normalized placement.
	image := req.DockerImage
	if req.PinnedImageRef != "" {
		image = req.PinnedImageRef
	}
	// Raw launches carry no image; suppliers require docker_image (a live
	// supplier returns 400 without it), so fall back to the vendor-neutral
	// default.
	if image == "" {
		image = defaultDockerImage
	}

	// Build the provision request with startup_script included (D-10).
	pReq := provisionRequest{
		InstanceID:    req.InstanceID,
		GPUType:       string(req.GPUType),
		GPUCount:      req.GPUCount,
		Tier:          string(req.Tier),
		Region:        req.Region,
		SSHPublicKeys: req.SSHPublicKeys,
		DockerImage:   image,
		StartupScript: req.StartupScript,
	}
	if req.RegistryAuth != nil {
		pReq.RegistryAuth = &registryAuth{
			Registry: req.RegistryAuth.Registry,
			Username: req.RegistryAuth.Username,
			Password: req.RegistryAuth.Password,
		}
	}

	token := a.getToken(ctx, *sup)
	resp, err := a.client.Provision(ctx, *sup, token, pReq)
	if err != nil {
		// Suppliers with best-effort shared pools (e.g. on-demand capacity)
		// reject a full-pool POST with a no-capacity error instead of
		// queuing. Surface that as ErrNoCapacity so the engine falls
		// through to the next candidate rather than hard-failing the launch.
		if isCapacityError(err) {
			// Preserve the ErrNoCapacity sentinel (engine fall-through depends on
			// errors.Is), but drop the slug — GPU-61.
			return nil, fmt.Errorf("provision failed: %w", provider.ErrNoCapacity)
		}
		// A non-capacity supplier error may embed the supplier's own hostname or
		// brand (it came from the supplier's API). Log the full detail for ops;
		// return a brand-free error to the customer. Only ErrNoCapacity drives
		// engine fall-through, so a neutral error here is behaviour-equivalent.
		a.logger.Error("generic: supplier provision failed",
			slog.String("supplier", slug), slog.String("error", err.Error()))
		return nil, errors.New("provision failed on the selected supplier")
	}

	result := toProvisionResult(resp, slug)

	a.logger.Info("provisioned instance via generic supplier",
		slog.String("supplier", slug),
		slog.String("upstream_id", result.UpstreamID),
		slog.Float64("cost_per_hour", result.CostPerHour),
	)

	return result, nil
}

// GetStatus returns the current status of a supplier-provisioned instance.
// Routes to the correct supplier using the slug:id encoding in upstreamID (Pitfall 2).
// region is unused — generic adapter routes via the supplier map keyed on
// the offering's region at provision time, not the call-site region.
func (a *Adapter) GetStatus(ctx context.Context, upstreamID, region string) (*provider.InstanceStatus, error) {
	_ = region
	slug, instanceID, err := splitUpstreamID(upstreamID)
	if err != nil {
		return nil, err
	}

	sup, err := a.store.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("supplier resolution failed: %w", err)
	}

	token := a.getToken(ctx, *sup)
	resp, err := a.client.GetStatus(ctx, *sup, token, instanceID)
	if err != nil {
		// GPU-61: supplier API errors may name the supplier — log, return neutral.
		a.logger.Error("generic: supplier status fetch failed",
			slog.String("supplier", slug), slog.String("error", err.Error()))
		return nil, errors.New("status fetch failed on the selected supplier")
	}

	return toInstanceStatus(resp, slug), nil
}

// Terminate destroys a supplier-provisioned instance.
// Routes to the correct supplier using the slug:id encoding in upstreamID.
// region is unused — see GetStatus.
func (a *Adapter) Terminate(ctx context.Context, upstreamID, region string) error {
	_ = region
	slug, instanceID, err := splitUpstreamID(upstreamID)
	if err != nil {
		return err
	}

	sup, err := a.store.GetBySlug(ctx, slug)
	if err != nil {
		return fmt.Errorf("supplier resolution failed: %w", err)
	}

	token := a.getToken(ctx, *sup)
	if err := a.client.Terminate(ctx, *sup, token, instanceID); err != nil {
		// GPU-61: supplier API errors may name the supplier — log, return neutral.
		a.logger.Error("generic: supplier terminate failed",
			slog.String("supplier", slug), slog.String("error", err.Error()))
		return errors.New("terminate failed on the selected supplier")
	}

	a.logger.Info("terminated instance via generic supplier",
		slog.String("supplier", slug),
		slog.String("instance_id", instanceID),
	)

	return nil
}

// ListProvisioned is unsupported for the generic multiplex adapter — there
// is no per-supplier "list my instances" contract in the generic supplier
// REST shape. The reconciler skips orphan detection for this provider and
// relies on DB-side drift checks (GetStatus per active row) instead.
func (a *Adapter) ListProvisioned(ctx context.Context) ([]provider.ProvisionedInstance, error) {
	return nil, provider.ErrNotImplemented
}
