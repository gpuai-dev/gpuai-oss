package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// LocalhostOnly restricts access to requests originating from loopback addresses
// (127.0.0.1 or ::1). Non-loopback requests receive a 404 Not Found to avoid
// revealing the endpoint's existence to external scanners.
func LocalhostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if host != "127.0.0.1" && host != "::1" {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// InternalAuthMiddleware protects internal endpoints with a shared token.
// It checks the Authorization header for "Bearer <token>" and returns
// 403 Forbidden if the token is missing or does not match.
func InternalAuthMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// InstanceTokenAuth validates per-instance Bearer tokens against the database.
// Each GPU instance authenticates itself using the unique internal_token assigned
// during provisioning. The token is sent in the Authorization: Bearer header
// by the cloud-init callback script.
func InstanceTokenAuth(dbPool *db.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Extract instance ID from path.
		instanceID := r.PathValue("id")
		if instanceID == "" {
			writeProblem(w, http.StatusBadRequest, "missing-id", "Instance ID is required")
			return
		}

		// 2. Extract Bearer token from Authorization header.
		auth := r.Header.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			writeProblem(w, http.StatusUnauthorized, "unauthorized", "Authorization required")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		// 3. Look up instance in the database.
		inst, err := dbPool.GetInstance(r.Context(), instanceID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			writeProblem(w, http.StatusInternalServerError, "internal-error",
				"Failed to verify instance token")
			return
		}

		// 4. Verify the token matches the instance's stored internal_token.
		if inst.InternalToken == nil || token != *inst.InternalToken {
			writeProblem(w, http.StatusForbidden, "forbidden", "Invalid instance token")
			return
		}

		// 5. Token valid -- proceed to handler.
		next.ServeHTTP(w, r)
	})
}
