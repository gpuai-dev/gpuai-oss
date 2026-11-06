package reconcile

import (
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/instanceid"
)

// parseOurInstanceID returns the canonical instance ID an upstream Name carries
// iff that Name is one THIS environment minted during Provision — exactly
// "<env>-gpu-<8hex>" in demo/prod, or bare "gpu-<8hex>" in development. Returns
// "" otherwise: the safety guard that keeps the reconciler from acting on
// instances launched outside gpu-ai OR by a DIFFERENT environment sharing the
// same upstream API key (e.g. a staging and a production deployment sharing one supplier account — without
// env scoping each reaped the other's fresh pods as orphans, 2026-06-20).
//
// The returned value is the bare "gpu-<8hex>" string (the env tag stripped),
// byte-identical to what the DB stores in instance_id (VARCHAR(12)), so
// detectOrphans can compare it directly against instance.InstanceID.
//
// The grammar and env-scoping live in package instanceid — the single source of
// truth shared with the adapters' ListProvisioned ownership filters. It
// matches ONLY our exact format for THIS env: a third-party instance, or another
// env's instance, under a shared upstream API key must not be treated as ours,
// or the enforce path could terminate it (WR-04).
func parseOurInstanceID(name string) string {
	id, _ := instanceid.OwnedHere(name)
	return id
}

// isNotFoundErr reports whether an error from a provider call indicates the
// upstream resource is gone. Mirrors the duck-typing used elsewhere in the
// codebase (e.g. provision.Engine.Terminate's "not found" check).
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "HTTP 404")
}
