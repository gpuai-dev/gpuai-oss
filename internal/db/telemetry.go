package db

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// GPUTelemetrySample is one in-pod nvidia-smi sample for one instance at one
// instant (TELEM-01). The metric fields are nullable pointers: a malformed or
// partial nvidia-smi line stores NULLs rather than failing the ingest, so a
// sparse sampler (an image without full --query-gpu support) degrades
// gracefully. TS is populated on reads (the DB defaults it to now() on insert).
type GPUTelemetrySample struct {
	TS          time.Time `json:"ts"`
	SMUtil      *int      `json:"sm_util,omitempty"`       // utilization.gpu (%)
	VRAMUsedMB  *int      `json:"vram_used_mb,omitempty"`  // memory.used (MiB)
	VRAMTotalMB *int      `json:"vram_total_mb,omitempty"` // memory.total (MiB)
	PowerW      *int      `json:"power_w,omitempty"`       // power.draw (W)
}

// ParseTelemetrySample parses one nvidia-smi
// `--query-gpu=utilization.gpu,memory.used,memory.total,power.draw
// --format=csv,noheader,nounits` line into a GPUTelemetrySample.
//
// The line is comma-separated, e.g. "98, 1024, 81920, 250". Parsing is
// tolerant: a missing, empty, or non-numeric field (including "[N/A]" which
// nvidia-smi emits when a metric is unsupported) leaves the corresponding
// pointer nil — that field stores NULL. power.draw may carry a decimal
// (e.g. "249.83"); it is rounded to the nearest int. An entirely empty line
// yields a zero-value sample (all NULLs).
func ParseTelemetrySample(line string) GPUTelemetrySample {
	var s GPUTelemetrySample
	fields := strings.Split(line, ",")

	// Positional: [0]=sm_util, [1]=vram_used_mb, [2]=vram_total_mb, [3]=power_w.
	targets := []**int{&s.SMUtil, &s.VRAMUsedMB, &s.VRAMTotalMB, &s.PowerW}
	for i, t := range targets {
		if i >= len(fields) {
			break
		}
		if v, ok := parseMetricInt(fields[i]); ok {
			*t = &v
		}
	}
	return s
}

// parseMetricInt parses one nvidia-smi metric token tolerantly. Empty / "[N/A]"
// / non-numeric -> (0, false). A decimal value (power.draw) is rounded.
func parseMetricInt(tok string) (int, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.EqualFold(tok, "[N/A]") || strings.EqualFold(tok, "N/A") {
		return 0, false
	}
	if n, err := strconv.Atoi(tok); err == nil {
		return n, true
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		// Round to nearest int (power.draw is the only fractional metric).
		if f >= 0 {
			return int(f + 0.5), true
		}
		return int(f - 0.5), true
	}
	return 0, false
}

// InsertGPUTelemetry appends one telemetry row for instanceID with ts=now()
// (TELEM-01). Self-transacting: this is the per-instance-token ingest path, not
// an admin mutation, so it needs no audit coupling. Nullable metric pointers
// store NULL when nil. Uses bound $N params (never interpolate).
func (p *Pool) InsertGPUTelemetry(ctx context.Context, instanceID string, s GPUTelemetrySample) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO gpu_telemetry (instance_id, sm_util, vram_used_mb, vram_total_mb, power_w)
		 VALUES ($1, $2, $3, $4, $5)`,
		instanceID, s.SMUtil, s.VRAMUsedMB, s.VRAMTotalMB, s.PowerW,
	)
	return err
}

// LatestGPUTelemetryTS returns the newest telemetry timestamp for instanceID,
// with ok=false when the instance has never reported telemetry. The health
// monitor's unreachable escalation uses it as a liveness veto: telemetry
// arrives outside the customer SSH tunnel, so a fresh sample proves the host
// is up even when the tunnel is dead. Uses the (instance_id, ts DESC) index
// from v44, so this is an index-only single-row read.
func (p *Pool) LatestGPUTelemetryTS(ctx context.Context, instanceID string) (time.Time, bool, error) {
	var ts time.Time
	err := p.pool.QueryRow(ctx,
		`SELECT ts FROM gpu_telemetry WHERE instance_id = $1 ORDER BY ts DESC LIMIT 1`,
		instanceID,
	).Scan(&ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return ts, true, nil
}
