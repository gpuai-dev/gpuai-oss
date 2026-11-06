package supplier

import "time"

// SupplierStatus represents the operational status of a supplier.
type SupplierStatus string

const (
	StatusActive    SupplierStatus = "active"
	StatusSuspended SupplierStatus = "suspended"
)

// Supplier represents a GPU provider that implements the GPU.ai Supplier Integration API.
// Per D-06: minimal profile with name, base URL, OAuth credentials, status, and regions.
type Supplier struct {
	SupplierID       string         `json:"supplier_id"`
	Slug             string         `json:"slug"`
	Name             string         `json:"name"`
	BaseURL          string         `json:"base_url"`
	ClientID         string         `json:"client_id"`
	ClientSecretHash string         `json:"-"` // never serialized
	Status           SupplierStatus `json:"status"`
	Regions          []string       `json:"regions"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}
