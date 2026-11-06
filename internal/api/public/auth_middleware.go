package public

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// ApiKeyLookup is the minimal interface ApiKeyAuthMiddleware needs from
// the database layer. *db.Pool satisfies it; tests provide a stub.
//
// GetUserRoleByUserID (Phase 39, SEC-01) resolves the key owner's role from
// users.role — on the API-key surface the DB is authoritative for the role
// bound, never any JWT. Errors (including db.ErrNotFound) are treated by the
// middlewares as role "member" (fail-closed, T-39-07).
type ApiKeyLookup interface {
	LookupAPIKeyByHash(ctx context.Context, keyHash string) (*db.APIKey, error)
	TouchAPIKeyLastUsed(ctx context.Context, apiKeyID string) error
	GetUserRoleByUserID(ctx context.Context, userID string) (string, error)
}

// touchThrottle is the minimum gap between consecutive last_used_at bumps
// for a single API key. A 100rps key would issue 100 UPDATEs/sec without
// throttling; 30s keeps DB pressure bounded while staying well below any
// common cron/dashboard refresh interval.
const touchThrottle = 30 * time.Second

// ApiKeyAuthMiddleware verifies the Authorization: Bearer header against
// the api_keys table, populates *APIKey on context, and bumps
// last_used_at fire-and-forget (throttled to once per touchThrottle per key).
// Refer to RESEARCH.md §3 for the canonical shape (lines 322-365).
//
// Failure modes:
//   - Missing/malformed Authorization header → 401 "unauthenticated".
//   - Bearer with non-`gpuai_live_` prefix    → 401 "invalid_api_key".
//   - Bearer not found / revoked / expired    → 401 "invalid_api_key".
//   - DB lookup returns non-ErrNotFound error → 500 "internal_error".
//   - api_keys.scopes column is malformed     → 500 "internal_error".
//
// On success the request context carries:
//   - the parsed Scopes (consumed by RequireScope below) via withScopes;
//   - a *APIKey populated for the rate limiter (Plan 18-07) via WithAPIKey.
func ApiKeyAuthMiddleware(lookup ApiKeyLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") {
				WriteProblem(w, r, http.StatusUnauthorized, "unauthenticated",
					"Missing or malformed Authorization header — expect 'Bearer gpuai_live_…'")
				return
			}
			raw := strings.TrimPrefix(authz, "Bearer ")
			if err := ParseRawKey(raw); err != nil {
				WriteProblem(w, r, http.StatusUnauthorized, "invalid_api_key",
					"API key is invalid")
				return
			}
			hash := HashKey(raw)
			row, err := lookup.LookupAPIKeyByHash(r.Context(), hash)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					WriteProblem(w, r, http.StatusUnauthorized, "invalid_api_key",
						"API key is invalid, revoked, or expired")
					return
				}
				slog.ErrorContext(r.Context(), "api key lookup failed", "error", err.Error())
				WriteProblem(w, r, http.StatusInternalServerError, "internal_error",
					"Authentication failed; please retry")
				return
			}
			scopes, perr := ParseScopes(row.Scopes)
			if perr != nil {
				slog.ErrorContext(r.Context(), "api key scopes malformed",
					"api_key_id", row.APIKeyID, "error", perr.Error())
				WriteProblem(w, r, http.StatusInternalServerError, "internal_error",
					"API key scopes are malformed; contact support")
				return
			}
			// Resolve the caller's DB role (Phase 39, SEC-01): on the
			// API-key surface the DB — never a JWT — is authoritative for
			// the role bound. One extra cheap indexed SELECT per request on
			// a surface that already hits Postgres for LookupAPIKeyByHash;
			// kept as a separate query (not a JOIN) for clarity. Fail
			// CLOSED to "member" on ANY error (including a missing users
			// row): a transient DB hiccup degrades the caller to least
			// privilege — it never escalates and never breaks auth
			// (T-39-07).
			role, rerr := lookup.GetUserRoleByUserID(r.Context(), row.UserID)
			if rerr != nil || role == "" {
				if rerr != nil {
					slog.WarnContext(r.Context(), "user role lookup failed; defaulting to member",
						"user_id", row.UserID, "error", rerr.Error())
				}
				role = "member"
			}

			// Stash both the parsed scopes (for RequireScope) and the
			// APIKey shape (for the rate limiter) on context.
			ctx := withScopes(r.Context(), scopes)
			ctx = WithAPIKey(ctx, &APIKey{
				APIKeyID:       row.APIKeyID,
				OrgID:          row.OrgID,
				UserID:         row.UserID,
				Role:           role,
				Scopes:         row.Scopes,
				RateLimitRPS:   row.RateLimitRPS,
				RateLimitBurst: row.RateLimitBurst,
			})

			// THROTTLED fire-and-forget last-used bump. Skip if last_used_at
			// was already updated within touchThrottle — prevents hammering the
			// DB on hot keys (a 100rps key would issue 100 UPDATEs/sec
			// without throttling). 30s is the operator-visible resolution we
			// care about for "is this key still in use?".
			shouldTouch := row.LastUsedAt == nil || time.Since(*row.LastUsedAt) > touchThrottle
			if shouldTouch {
				go func(id string) {
					// INDEPENDENT 5s timeout via context.Background() so the
					// bookkeeping is never cancelled by request shutdown.
					bumpCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := lookup.TouchAPIKeyLastUsed(bumpCtx, id); err != nil {
						slog.Warn("touch api_key.last_used_at failed",
							"api_key_id", id, "error", err.Error())
					}
				}(row.APIKeyID)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// scopesCtxKey is the unexported context-key type carrying parsed scopes.
type scopesCtxKey struct{}

// withScopes returns a child context carrying the parsed Scopes.
func withScopes(ctx context.Context, s Scopes) context.Context {
	return context.WithValue(ctx, scopesCtxKey{}, s)
}

// ScopesFromContext returns the parsed scopes for the authenticated key,
// or a zero Scopes value (denies everything) when absent.
func ScopesFromContext(ctx context.Context) Scopes {
	if v, ok := ctx.Value(scopesCtxKey{}).(Scopes); ok {
		return v
	}
	return Scopes{}
}

// RequireScope returns middleware that enforces the given resource:level on
// the parsed scopes from context. ApiKeyAuthMiddleware MUST run first.
// On insufficient scope, returns 403 with code "insufficient_scope".
func RequireScope(resource, level string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sc := ScopesFromContext(r.Context())
			if !sc.Allows(resource, level) {
				WriteProblem(w, r, http.StatusForbidden, "insufficient_scope",
					scopeDenialDetail(resource, level))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// scopeDenialDetail builds the 403 body. For an ordinary resource the message
// is the historical one, unchanged. For an EXPLICIT-GRANT-ONLY resource
// (scopes.go's explicitWriteGrantResources) it additionally says so, because
// the default message is actively misleading there: the holder of a
// full_access key reading "API key lacks autolaunch:write scope" would
// reasonably conclude we had a bug, retry, and file a ticket. Naming the
// carve-out turns a dead end into an instruction, and gives the CLI and MCP
// surfaces something actionable to render.
//
// THE ADVICE MUST BE FOLLOWABLE, which is why it changed. It used to end
// "Mint a key whose scopes include {"autolaunch":"write"}" — two steps of which
// now fail:
//
//   - a NON-ADMIN can no longer mint it at all (handleCreateAPIKey's hard 403),
//     so "mint a key" sends an ordinary member into a second refusal; and
//   - adding the grant to an existing full_access body is refused by
//     ValidateMintScopes, because a shortcut plus a per-resource map is an
//     ambiguous shape — it used to mint a 201 whose key still could not create
//     rules, with no signal anywhere.
//
// So the sentence now names WHO can mint it and WHAT SHAPE to send. A refusal
// that describes an impossible remedy is worse than one that describes none:
// the customer spends their time proving our instruction wrong.
func scopeDenialDetail(resource, level string) string {
	detail := fmt.Sprintf("API key lacks %s:%s scope", resource, level)
	if explicitWriteGrantResources[resource] && level == string(ScopeWrite) {
		detail += fmt.Sprintf(
			"; %s:write is never granted by full_access, standard or read_only —"+
				" it authorizes UNATTENDED spending and must be granted explicitly."+
				" An ORGANIZATION ADMIN must mint the key, sending the grant on its own"+
				" as a per-resource scope map ({\"%s\":\"write\"}) — combining it with"+
				" full_access, standard or read_only in one body is refused",
			resource, resource)
	}
	return detail
}

// WriteProblem is the canonical RFC 7807 problem-details writer for the
// public-API package. It is exported so future plans (and this Plan 18-08
// middleware) can produce consistent error envelopes without redefining the
// helper. Internally delegates to writeProblem (Plan 18-05) — there is no
// duplicate logic, only the public name.
//
// The plan must_haves contract states "WriteProblem is the SAME function
// across the whole package — there is no separate writeProblemPublic
// duplicate"; this exported wrapper preserves that invariant while letting
// the existing lowercase callers (middleware_ratelimit.go, errors_test.go)
// continue to work unchanged.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	writeProblem(w, r, status, code, detail)
}
