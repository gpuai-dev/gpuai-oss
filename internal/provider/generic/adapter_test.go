package generic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/supplier"
)

// mockStore implements SupplierLister for testing.
type mockStore struct {
	suppliers []supplier.Supplier
}

func (m *mockStore) ListActive(_ context.Context) ([]supplier.Supplier, error) {
	return m.suppliers, nil
}

func (m *mockStore) GetBySlug(_ context.Context, slug string) (*supplier.Supplier, error) {
	for i := range m.suppliers {
		if m.suppliers[i].Slug == slug {
			return &m.suppliers[i], nil
		}
	}
	return nil, db.ErrNotFound
}

// newTestServer creates an httptest server with the given handler and returns the URL.
func newTestServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// --- Tests ---

func TestAdapterName(t *testing.T) {
	store := &mockStore{}
	adapter := NewAdapter(store, nil)
	if adapter.Name() != "generic" {
		t.Errorf("expected Name() = %q, got %q", "generic", adapter.Name())
	}
}

func TestListAvailable_TwoSuppliers(t *testing.T) {
	// Supplier 1: returns 1 offering
	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(availableResponse{
			Offerings: []offeringJSON{
				{
					GPUType:        "h100_sxm",
					GPUCount:       1,
					VRAMPerGPUGB:   80,
					PricePerHour:   3.50,
					Tier:           "on_demand",
					Region:         "us-central",
					StockStatus:    "High",
					AvailableCount: 10,
				},
			},
		})
	})
	defer s1.Close()

	// Supplier 2: returns 1 offering
	s2 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(availableResponse{
			Offerings: []offeringJSON{
				{
					GPUType:        "a100_80gb",
					GPUCount:       1,
					VRAMPerGPUGB:   80,
					PricePerHour:   2.50,
					Tier:           "spot",
					Region:         "eu-west",
					StockStatus:    "Medium",
					AvailableCount: 5,
				},
			},
		})
	})
	defer s2.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme", Name: "ACME GPU", BaseURL: s1.URL, Status: supplier.StatusActive},
			{Slug: "beta", Name: "Beta Compute", BaseURL: s2.URL, Status: supplier.StatusActive},
		},
	}

	adapter := NewAdapter(store, nil)
	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable returned error: %v", err)
	}
	if len(offerings) != 2 {
		t.Fatalf("expected 2 offerings, got %d", len(offerings))
	}

	// Both offerings should have Provider="generic" (not the supplier slug).
	for _, o := range offerings {
		if o.Provider != "generic" {
			t.Errorf("expected Provider=%q, got %q", "generic", o.Provider)
		}
	}
}

func TestListAvailable_OneFailingSupplier(t *testing.T) {
	// Supplier 1: returns 400 error (non-retryable, keeps test fast)
	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	})
	defer s1.Close()

	// Supplier 2: returns 1 offering
	s2 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(availableResponse{
			Offerings: []offeringJSON{
				{
					GPUType:        "rtx_4090",
					GPUCount:       1,
					VRAMPerGPUGB:   24,
					PricePerHour:   0.80,
					Tier:           "on_demand",
					Region:         "us-west",
					StockStatus:    "High",
					AvailableCount: 20,
				},
			},
		})
	})
	defer s2.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "bad-supplier", Name: "Bad Supplier", BaseURL: s1.URL, Status: supplier.StatusActive},
			{Slug: "good-supplier", Name: "Good Supplier", BaseURL: s2.URL, Status: supplier.StatusActive},
		},
	}

	adapter := NewAdapter(store, nil)
	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable should not return error when one supplier fails: %v", err)
	}
	if len(offerings) != 1 {
		t.Fatalf("expected 1 offering from good supplier, got %d", len(offerings))
	}
	if offerings[0].GPUType != "rtx_4090" {
		t.Errorf("expected GPU type rtx_4090, got %s", offerings[0].GPUType)
	}
}

func TestListAvailable_NoSuppliers(t *testing.T) {
	store := &mockStore{suppliers: nil}
	adapter := NewAdapter(store, nil)

	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable returned error: %v", err)
	}
	if len(offerings) != 0 {
		t.Errorf("expected 0 offerings, got %d", len(offerings))
	}
}

func TestProvision_Success(t *testing.T) {
	var receivedBody string
	var receivedMethod string

	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/gpu/available" {
			json.NewEncoder(w).Encode(availableResponse{
				Offerings: []offeringJSON{
					{
						GPUType:        "h100_sxm",
						GPUCount:       1,
						VRAMPerGPUGB:   80,
						PricePerHour:   3.50,
						Tier:           "on_demand",
						Region:         "us-central",
						AvailableCount: 10,
					},
				},
			})
			return
		}
		if r.URL.Path == "/v1/instances" {
			receivedMethod = r.Method
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)

			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(provisionResponse{
				UpstreamID:            "inst-abc-123",
				Status:                "creating",
				CostPerHour:           3.50,
				EstimatedReadySeconds: 45,
				DatacenterLocation:    "US-TX-3",
				Region:                "us-central",
			})
			return
		}
	})
	defer s1.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme", Name: "ACME GPU", BaseURL: s1.URL, Status: supplier.StatusActive},
		},
	}

	adapter := NewAdapter(store, nil)

	// First call ListAvailable to populate the supplier offering map.
	_, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable returned error: %v", err)
	}

	result, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID:    "gpu-test-001",
		GPUType:       "h100_sxm",
		GPUCount:      1,
		Tier:          provider.TierOnDemand,
		Region:        "us-central",
		SSHPublicKeys: []string{"ssh-ed25519 AAAA..."},
		StartupScript: "#!/bin/bash\necho hello",
	})
	if err != nil {
		t.Fatalf("Provision returned error: %v", err)
	}

	// Verify ProvisionResult has slug:id encoding.
	if result.UpstreamID != "acme:inst-abc-123" {
		t.Errorf("expected UpstreamID=%q, got %q", "acme:inst-abc-123", result.UpstreamID)
	}
	if result.Provider != "generic" {
		t.Errorf("expected Provider=%q, got %q", "generic", result.Provider)
	}
	if result.Status != "creating" {
		t.Errorf("expected Status=%q, got %q", "creating", result.Status)
	}
	if result.CostPerHour != 3.50 {
		t.Errorf("expected CostPerHour=3.50, got %f", result.CostPerHour)
	}

	// Verify request body contains startup_script (D-10).
	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST method, got %s", receivedMethod)
	}
	if !strings.Contains(receivedBody, "startup_script") {
		t.Error("expected startup_script in request body")
	}
	if !strings.Contains(receivedBody, "echo hello") {
		t.Error("expected startup script content in request body")
	}
	if !strings.Contains(receivedBody, "h100_sxm") {
		t.Error("expected gpu_type in request body")
	}
}

func TestProvision_FullPoolMapsToErrNoCapacity(t *testing.T) {
	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/gpu/available" {
			json.NewEncoder(w).Encode(availableResponse{
				Offerings: []offeringJSON{
					{
						GPUType:        "rtx_pro_6000",
						GPUCount:       1,
						VRAMPerGPUGB:   96,
						PricePerHour:   1.89,
						Tier:           "on_demand",
						Region:         "US",
						AvailableCount: 1,
					},
				},
			})
			return
		}
		if r.URL.Path == "/v1/instances" {
			// Best-effort shared pool: a full-pool POST rejects with a
			// no-capacity error instead of queuing.
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "no_capacity",
				"message": "no capacity available for rtx_pro_6000 in US",
			})
			return
		}
	})
	defer s1.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme-gpu", Name: "Acme GPU", BaseURL: s1.URL, Status: supplier.StatusActive},
		},
	}
	adapter := NewAdapter(store, nil)
	if _, err := adapter.ListAvailable(context.Background()); err != nil {
		t.Fatalf("ListAvailable returned error: %v", err)
	}

	_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID: "gpu-test-002",
		GPUType:    "rtx_pro_6000",
		GPUCount:   1,
		Tier:       provider.TierOnDemand,
		Region:     "US",
	})
	if !errors.Is(err, provider.ErrNoCapacity) {
		t.Fatalf("Provision on full pool: want ErrNoCapacity, got %v", err)
	}
	// GPU-61: the community supplier slug must never appear in the returned
	// error — it flows into Operation.Error.Detail and reaches the customer.
	if strings.Contains(err.Error(), "acme-gpu") {
		t.Errorf("capacity error leaks supplier slug: %q", err.Error())
	}
}

// GPU-61: a non-capacity supplier failure must not leak the supplier slug (or
// any brand the supplier's own API returns) into the customer-facing error.
func TestProvision_NonCapacityErrorDoesNotLeakSupplier(t *testing.T) {
	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/gpu/available" {
			json.NewEncoder(w).Encode(availableResponse{
				Offerings: []offeringJSON{
					{GPUType: "rtx_pro_6000", GPUCount: 1, VRAMPerGPUGB: 96, PricePerHour: 1.89, Tier: "on_demand", Region: "US", AvailableCount: 1},
				},
			})
			return
		}
		if r.URL.Path == "/v1/instances" {
			// A non-capacity failure whose body even names the supplier brand.
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "internal",
				"message": "acme-gpu backend exploded",
			})
			return
		}
	})
	defer s1.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme-gpu", Name: "Acme GPU", BaseURL: s1.URL, Status: supplier.StatusActive},
		},
	}
	adapter := NewAdapter(store, nil)
	if _, err := adapter.ListAvailable(context.Background()); err != nil {
		t.Fatalf("ListAvailable returned error: %v", err)
	}

	_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID: "gpu-test-003", GPUType: "rtx_pro_6000", GPUCount: 1,
		Tier: provider.TierOnDemand, Region: "US",
	})
	if err == nil {
		t.Fatal("expected a non-capacity provision error, got nil")
	}
	if errors.Is(err, provider.ErrNoCapacity) {
		t.Fatalf("non-capacity failure misclassified as ErrNoCapacity: %v", err)
	}
	if strings.Contains(err.Error(), "acme-gpu") {
		t.Errorf("non-capacity error leaks supplier slug/brand: %q", err.Error())
	}
}

func TestGetStatus_RoutesToCorrectSupplier(t *testing.T) {
	var acmeCalled atomic.Int32
	var betaCalled atomic.Int32

	acmeServer := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		acmeCalled.Add(1)
		if strings.HasPrefix(r.URL.Path, "/v1/instances/") {
			json.NewEncoder(w).Encode(statusResponse{
				UpstreamID:    "inst-123",
				Status:        "running",
				IP:            "10.0.0.1",
				CostPerHour:   3.50,
				UptimeSeconds: 600,
			})
		}
	})
	defer acmeServer.Close()

	betaServer := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		betaCalled.Add(1)
		json.NewEncoder(w).Encode(statusResponse{
			UpstreamID:    "inst-456",
			Status:        "running",
			IP:            "10.0.0.2",
			CostPerHour:   2.50,
			UptimeSeconds: 300,
		})
	})
	defer betaServer.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme", Name: "ACME GPU", BaseURL: acmeServer.URL, Status: supplier.StatusActive},
			{Slug: "beta", Name: "Beta Compute", BaseURL: betaServer.URL, Status: supplier.StatusActive},
		},
	}

	adapter := NewAdapter(store, nil)

	status, err := adapter.GetStatus(context.Background(), "acme:inst-123", "")
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if status.Status != "running" {
		t.Errorf("expected status running, got %s", status.Status)
	}
	if status.IP != "10.0.0.1" {
		t.Errorf("expected IP 10.0.0.1, got %s", status.IP)
	}
	if status.UpstreamID != "acme:inst-123" {
		t.Errorf("expected UpstreamID=%q, got %q", "acme:inst-123", status.UpstreamID)
	}

	// Verify only acme was called, not beta.
	if acmeCalled.Load() != 1 {
		t.Errorf("expected acme called once, got %d", acmeCalled.Load())
	}
	if betaCalled.Load() != 0 {
		t.Errorf("expected beta not called, got %d calls", betaCalled.Load())
	}
}

func TestGetStatus_UnknownSlug(t *testing.T) {
	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme", Name: "ACME GPU", BaseURL: "http://localhost:1", Status: supplier.StatusActive},
		},
	}

	adapter := NewAdapter(store, nil)

	_, err := adapter.GetStatus(context.Background(), "unknown:inst-123", "")
	if err == nil {
		t.Fatal("expected error for unknown slug, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestTerminate_Success(t *testing.T) {
	var receivedMethod string
	var receivedPath string

	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	defer s1.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme", Name: "ACME GPU", BaseURL: s1.URL, Status: supplier.StatusActive},
		},
	}

	adapter := NewAdapter(store, nil)

	err := adapter.Terminate(context.Background(), "acme:inst-789", "")
	if err != nil {
		t.Fatalf("Terminate returned error: %v", err)
	}

	// Verify DELETE method used.
	if receivedMethod != http.MethodDelete {
		t.Errorf("expected DELETE method, got %s", receivedMethod)
	}
	if receivedPath != "/v1/instances/inst-789" {
		t.Errorf("expected path /v1/instances/inst-789, got %s", receivedPath)
	}
}

// TestPinnedImage asserts the server-controlled, digest-pinned manifest image
// (ProvisionRequest.PinnedImageRef) is sent as the launch image and wins over
// any customer-supplied DockerImage (Phase 63, V5 / T-63-02 supply-chain
// tamper). Wave 0 RED: the adapter currently forwards req.DockerImage and
// ignores PinnedImageRef.
func TestProvision_EmptyImageSendsDefault(t *testing.T) {
	var receivedBody string
	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/gpu/available" {
			json.NewEncoder(w).Encode(availableResponse{
				Offerings: []offeringJSON{
					{GPUType: "rtx_pro_6000", GPUCount: 1, PricePerHour: 1.89,
						Tier: "on_demand", Region: "US", AvailableCount: 3},
				},
			})
			return
		}
		if r.URL.Path == "/v1/instances" {
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(provisionResponse{
				UpstreamID: "inst-img-1", Status: "creating", CostPerHour: 1.89, Region: "US",
			})
			return
		}
	})
	defer s1.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme-gpu", Name: "Acme GPU", BaseURL: s1.URL, Status: supplier.StatusActive},
		},
	}
	adapter := NewAdapter(store, nil)
	if _, err := adapter.ListAvailable(context.Background()); err != nil {
		t.Fatalf("ListAvailable returned error: %v", err)
	}

	// No DockerImage, no PinnedImageRef — a raw launch. Suppliers require
	// docker_image, so the default must be sent.
	if _, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID: "gpu-img-001",
		GPUType:    "rtx_pro_6000",
		GPUCount:   1,
		Tier:       provider.TierOnDemand,
		Region:     "US",
	}); err != nil {
		t.Fatalf("Provision returned error: %v", err)
	}
	if !strings.Contains(receivedBody, defaultDockerImage) {
		t.Errorf("request body missing default docker_image %q: %s", defaultDockerImage, receivedBody)
	}

	// A customer-supplied image must still win over the default.
	if _, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID:  "gpu-img-002",
		GPUType:     "rtx_pro_6000",
		GPUCount:    1,
		Tier:        provider.TierOnDemand,
		Region:      "US",
		DockerImage: "customer/own:1.0",
	}); err != nil {
		t.Fatalf("Provision returned error: %v", err)
	}
	if !strings.Contains(receivedBody, "customer/own:1.0") {
		t.Errorf("request body missing customer image: %s", receivedBody)
	}
}

func TestPinnedImage(t *testing.T) {
	const pinnedRef = "ghcr.io/gpuai/certified@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const customerImage = "attacker/evil:latest"

	var receivedBody string
	s1 := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/gpu/available" {
			json.NewEncoder(w).Encode(availableResponse{
				Offerings: []offeringJSON{
					{
						GPUType:        "h100_sxm",
						GPUCount:       1,
						VRAMPerGPUGB:   80,
						PricePerHour:   3.50,
						Tier:           "on_demand",
						Region:         "us-central",
						AvailableCount: 10,
					},
				},
			})
			return
		}
		if r.URL.Path == "/v1/instances" {
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(provisionResponse{
				UpstreamID:  "inst-pin-1",
				Status:      "creating",
				CostPerHour: 3.50,
				Region:      "us-central",
			})
			return
		}
	})
	defer s1.Close()

	store := &mockStore{
		suppliers: []supplier.Supplier{
			{Slug: "acme", Name: "ACME GPU", BaseURL: s1.URL, Status: supplier.StatusActive},
		},
	}
	adapter := NewAdapter(store, nil)
	if _, err := adapter.ListAvailable(context.Background()); err != nil {
		t.Fatalf("ListAvailable returned error: %v", err)
	}

	_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID:     "gpu-pin-001",
		GPUType:        "h100_sxm",
		GPUCount:       1,
		Tier:           provider.TierOnDemand,
		Region:         "us-central",
		SSHPublicKeys:  []string{"ssh-ed25519 AAAA..."},
		DockerImage:    customerImage,
		PinnedImageRef: pinnedRef,
	})
	if err != nil {
		t.Fatalf("Provision returned error: %v", err)
	}
	if !strings.Contains(receivedBody, pinnedRef) {
		t.Errorf("expected launch payload to use pinned image %q, body: %s", pinnedRef, receivedBody)
	}
	if strings.Contains(receivedBody, customerImage) {
		t.Errorf("customer-supplied image %q must NOT appear in launch payload (V5); body: %s", customerImage, receivedBody)
	}
}
