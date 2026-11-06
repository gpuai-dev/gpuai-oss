package db

import (
	"context"
	"encoding/json"
	"time"
)

// AuditEntry represents a row in the audit_log table.
type AuditEntry struct {
	ID           string          `json:"id"`
	OrgID        string          `json:"org_id"`
	UserID       *string         `json:"user_id,omitempty"`
	Action       string          `json:"action"`
	ResourceType *string         `json:"resource_type,omitempty"`
	ResourceID   *string         `json:"resource_id,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	// ActorName is populated by ListAuditLogForUser / ListMemberLimitAuditLog /
	// ListInstanceAuditLog via LEFT JOIN on users.user_id. Nil when audit_log.user_id
	// is NULL (system actor — e.g. instance.stopped emitted by the ticker) or when
	// the row was fetched via a query that does not perform the JOIN.
	ActorName *string `json:"actor_name,omitempty"`
}

// CreateAuditEntry inserts a new audit log entry.
func (p *Pool) CreateAuditEntry(ctx context.Context, entry *AuditEntry) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO audit_log (org_id, user_id, action, resource_type, resource_id, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		entry.OrgID, entry.UserID, entry.Action, entry.ResourceType, entry.ResourceID, entry.Metadata,
	)
	return err
}
