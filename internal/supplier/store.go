package supplier

import (
	"context"
	"errors"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/jackc/pgx/v5"
)

// supplierColumns is the ordered list of columns for SELECT queries.
const supplierColumns = `supplier_id, slug, name, base_url, client_id, client_secret_hash,
	status, regions, created_at, updated_at`

// Store provides database operations for the suppliers table.
type Store struct {
	db *db.Pool
}

// NewStore creates a new supplier Store backed by the given database pool.
func NewStore(pool *db.Pool) *Store {
	return &Store{db: pool}
}

// scanSupplier scans a single row into a Supplier struct.
func scanSupplier(row pgx.Row) (*Supplier, error) {
	var sup Supplier
	err := row.Scan(
		&sup.SupplierID, &sup.Slug, &sup.Name, &sup.BaseURL, &sup.ClientID, &sup.ClientSecretHash,
		&sup.Status, &sup.Regions, &sup.CreatedAt, &sup.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &sup, nil
}

// Create inserts a new supplier record into the database.
// The supplier_id is set by the database via gen_random_uuid() and returned via RETURNING.
func (s *Store) Create(ctx context.Context, sup *Supplier) error {
	return s.db.PgxPool().QueryRow(ctx, `
		INSERT INTO suppliers (slug, name, base_url, client_id, client_secret_hash, status, regions)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING supplier_id, created_at, updated_at`,
		sup.Slug, sup.Name, sup.BaseURL, sup.ClientID, sup.ClientSecretHash, sup.Status, sup.Regions,
	).Scan(&sup.SupplierID, &sup.CreatedAt, &sup.UpdatedAt)
}

// ListActive returns all suppliers with status 'active', ordered by name.
func (s *Store) ListActive(ctx context.Context) ([]Supplier, error) {
	rows, err := s.db.PgxPool().Query(ctx,
		`SELECT `+supplierColumns+` FROM suppliers WHERE status = 'active' ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var suppliers []Supplier
	for rows.Next() {
		var sup Supplier
		if err := rows.Scan(
			&sup.SupplierID, &sup.Slug, &sup.Name, &sup.BaseURL, &sup.ClientID, &sup.ClientSecretHash,
			&sup.Status, &sup.Regions, &sup.CreatedAt, &sup.UpdatedAt,
		); err != nil {
			return nil, err
		}
		suppliers = append(suppliers, sup)
	}
	return suppliers, rows.Err()
}

// GetByClientID retrieves a supplier by its OAuth client_id.
// Returns db.ErrNotFound if no matching supplier exists.
func (s *Store) GetByClientID(ctx context.Context, clientID string) (*Supplier, error) {
	row := s.db.PgxPool().QueryRow(ctx,
		`SELECT `+supplierColumns+` FROM suppliers WHERE client_id = $1`,
		clientID,
	)
	sup, err := scanSupplier(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, db.ErrNotFound
	}
	return sup, err
}

// GetBySlug retrieves a supplier by its URL-friendly slug.
// Returns db.ErrNotFound if no matching supplier exists.
func (s *Store) GetBySlug(ctx context.Context, slug string) (*Supplier, error) {
	row := s.db.PgxPool().QueryRow(ctx,
		`SELECT `+supplierColumns+` FROM suppliers WHERE slug = $1`,
		slug,
	)
	sup, err := scanSupplier(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, db.ErrNotFound
	}
	return sup, err
}

// UpdateStatus changes a supplier's status and updates the updated_at timestamp.
func (s *Store) UpdateStatus(ctx context.Context, supplierID string, status SupplierStatus) error {
	_, err := s.db.PgxPool().Exec(ctx,
		`UPDATE suppliers SET status = $1, updated_at = NOW() WHERE supplier_id = $2`,
		status, supplierID,
	)
	return err
}

// Upsert inserts or refreshes a supplier keyed by slug: name, base_url,
// status and regions follow the configuration on every call, so the
// GENERIC_SUPPLIERS environment variable stays the source of truth for the
// suppliers it names. client_id (a NOT NULL unique column kept for schema
// compatibility with inbound supplier auth) is derived from the slug, and no
// client secret is stored.
func (s *Store) Upsert(ctx context.Context, sup *Supplier) error {
	if sup.Regions == nil {
		sup.Regions = []string{}
	}
	if sup.Status == "" {
		sup.Status = StatusActive
	}
	return s.db.PgxPool().QueryRow(ctx, `
		INSERT INTO suppliers (slug, name, base_url, client_id, client_secret_hash, status, regions)
		VALUES ($1, $2, $3, $4, '', $5, $6)
		ON CONFLICT (slug) DO UPDATE
		   SET name = EXCLUDED.name,
		       base_url = EXCLUDED.base_url,
		       status = EXCLUDED.status,
		       regions = EXCLUDED.regions,
		       updated_at = NOW()
		RETURNING supplier_id, created_at, updated_at`,
		sup.Slug, sup.Name, sup.BaseURL, "generic:"+sup.Slug, sup.Status, sup.Regions,
	).Scan(&sup.SupplierID, &sup.CreatedAt, &sup.UpdatedAt)
}
