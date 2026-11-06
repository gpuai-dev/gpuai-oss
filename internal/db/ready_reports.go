package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"
)

const MaxReadyReportsPerInstance = 64

var readyReportValue = regexp.MustCompile(`^[A-Za-z0-9.:_/@-]+$`)
var readyReportKeys = map[string]bool{"driver_version": true, "cuda_version": true, "certified_image_digest": true, "container_id": true, "docker_up": true,
	// The startup GPU compute check (PROVISION_GPU_CHECK) reports with the
	// ready callback in shadow and enforce mode. Its result is what the shadow
	// period exists to collect, so it is stored with the rest of the report.
	"gpu_check": true, "gpu_check_mode": true, "gpus_seen": true, "gpu_check_detail": true}

// readyReportCheckDetail admits the check's reason sentence, already reduced
// by the callback handler (sanitizeCheckDetail) to letters, digits, spaces and
// . , : ; = / ( ) + - _ and at most 160 characters.
var readyReportCheckDetail = regexp.MustCompile(`^[A-Za-z0-9 .,:;=/()+_-]{1,160}$`)

// readyReportClosed holds the fields whose values come from a closed set.
var readyReportClosed = map[string]map[string]bool{
	"docker_up":      {"true": true, "false": true},
	"gpu_check":      {"ok": true, "fail": true, "warn": true, "skipped": true},
	"gpu_check_mode": {"shadow": true, "enforce": true},
}

var readyReportGPUsSeen = regexp.MustCompile(`^[0-9]{1,3}$`)

// ReadyReport is ADMIN-ONLY bootstrap self-report, not independent attestation
// or a successful computation. Provider/upstream identify the placement in the
// database AT RECEIPT; an old callback cannot attest a newly rebound host.
// Identical snapshots are folded, so callbacks_received is not a sample size.
type ReadyReport struct {
	ReportID          int64             `json:"report_id"`
	Provider          string            `json:"provider"`
	UpstreamID        string            `json:"upstream_id"`
	Report            map[string]string `json:"report"`
	FirstObservedAt   time.Time         `json:"first_observed_at"`
	LastObservedAt    time.Time         `json:"last_observed_at"`
	CallbacksReceived int64             `json:"callbacks_received"`
}

func normalizeReadyReport(report map[string]string) (map[string]string, error) {
	if len(report) > len(readyReportKeys) {
		return nil, errors.New("too many environment fields")
	}
	out := make(map[string]string)
	for key, value := range report {
		if !readyReportKeys[key] {
			return nil, errors.New("unknown environment field")
		}
		if value == "" {
			continue
		}
		switch {
		case key == "gpu_check_detail":
			if !readyReportCheckDetail.MatchString(value) {
				return nil, errors.New("invalid environment value")
			}
		case readyReportClosed[key] != nil:
			if !readyReportClosed[key][value] {
				return nil, fmt.Errorf("invalid %s value", key)
			}
		case key == "gpus_seen":
			if !readyReportGPUsSeen.MatchString(value) {
				return nil, errors.New("invalid environment value")
			}
		default:
			if len(value) > 128 || !readyReportValue.MatchString(value) {
				return nil, errors.New("invalid environment value")
			}
		}
		out[key] = value
	}
	return out, nil
}

// RecordReadyReport implements the production callback hook. Failure to save
// diagnostic evidence must not change readiness, billing, or the callback's
// response. The timeout bounds additional callback work, including lock waits.
func (p *Pool) RecordReadyReport(ctx context.Context, instanceID string, report map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := p.SaveReadyReport(ctx, instanceID, report); err != nil {
		slog.Warn("ready environment report not saved", "instance_id", instanceID, "error", err)
	}
}

// SaveReadyReport is the error-returning storage seam for tests and explicit
// internal callers. No free-form payloads, customer identifiers, tokens, or
// provider identity supplied by the callback are accepted here.
func (p *Pool) SaveReadyReport(ctx context.Context, instanceID string, report map[string]string) error {
	fields, err := normalizeReadyReport(report)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return nil
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var found bool
	// PostgreSQL performs the parent lock, placement snapshot, dedupe and
	// bounded retention atomically in a single round trip.
	err = p.pool.QueryRow(ctx, `SELECT save_instance_ready_report($1,$2)`, instanceID, payload).Scan(&found)
	if err != nil {
		return fmt.Errorf("save ready report: %w", err)
	}
	if !found {
		return ErrNotFound
	}
	return nil
}
