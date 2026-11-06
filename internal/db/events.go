package db

import (
	"context"
	"encoding/json"
	"time"
)

// InstanceEvent represents an event in the instance lifecycle.
type InstanceEvent struct {
	EventID    string `json:"event_id"`
	InstanceID string `json:"instance_id"`
	OrgID      string `json:"org_id"`
	// EventType is one of the values the instance_events CHECK constraint
	// allows: ready, interrupted, failed, terminated, unreachable (v80), rescued
	// (v87), preemption_warned + preemption_reclaimed (v91), auto_terminated
	// (v92). 'rescued' is ADMIN-ONLY — see ListInstanceEventsByOrg;
	// 'auto_terminated' is customer-visible, because it carries only the
	// customer's own request and marks a terminate as the platform enforcing
	// that instance's auto_terminate_hours — something the sibling 'terminated'
	// row cannot express.
	//
	// This list is not documentation-only: an event_type outside the constraint
	// fails every INSERT with SQLSTATE 23514, and callers only log that error
	// (that was GPU-50). instance_events_constraint_test.go cross-checks every
	// EventType literal under internal/ against the newest migration's value
	// list, so a new type cannot ship without its migration.
	EventType string          `json:"event_type"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// CreateInstanceEvent inserts a new event into the instance_events table.
func (p *Pool) CreateInstanceEvent(ctx context.Context, event *InstanceEvent) error {
	return p.pool.QueryRow(ctx,
		`INSERT INTO instance_events (instance_id, org_id, event_type, metadata)
		 VALUES ($1, $2, $3, $4)
		 RETURNING event_id, created_at`,
		event.InstanceID, event.OrgID, event.EventType, event.Metadata,
	).Scan(&event.EventID, &event.CreatedAt)
}
