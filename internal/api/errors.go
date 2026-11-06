package api

import (
	"encoding/json"
	"net/http"
)

// ProblemDetail represents an RFC 7807 Problem Details response.
type ProblemDetail struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// writeProblem writes an RFC 7807 Problem Details JSON response.
// errType is appended to the base URI "https://api.gpu.ai/errors/".
func writeProblem(w http.ResponseWriter, status int, errType, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ProblemDetail{
		Type:   "https://api.gpu.ai/errors/" + errType,
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
}

// writeProblemExt writes an RFC 7807 Problem Details response with additional
// extension members (RFC 7807 §3.2) merged onto the top-level object — e.g. a
// current_price_per_hour the client re-quotes against on a price-changed 409.
// The reserved members (type/title/status/detail) always win over extras.
func writeProblemExt(w http.ResponseWriter, status int, errType, detail string, extra map[string]any) {
	body := make(map[string]any, len(extra)+4)
	for k, v := range extra {
		body[k] = v
	}
	body["type"] = "https://api.gpu.ai/errors/" + errType
	body["title"] = http.StatusText(status)
	body["status"] = status
	body["detail"] = detail
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
