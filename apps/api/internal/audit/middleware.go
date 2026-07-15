package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// Recorder writes audit log entries to the audit_log table. It is safe
// to share across requests: every method either takes a context or
// derives everything it needs from the request.
type Recorder struct {
	DB     *db.DB
	Logger *slog.Logger
}

// New constructs a Recorder. logger may be nil; in that case the default
// slog logger is used.
func New(database *db.DB, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{DB: database, Logger: logger}
}

// Entry is a single audit log row. The zero value uses sensible
// defaults: the org/actor are pulled from the request's principal, the
// request id is generated, and an empty metadata map is serialised as
// `{}`.
type Entry struct {
	// OrgID, ActorID, ActorType: filled from the principal if empty.
	OrgID     string
	ActorID   string
	ActorType string // "user", "apikey", or "system"

	// Action is a dot-separated verb-resource string, e.g.
	// "upload.create". The schema constrains it to the audit_action
	// enum; unknown values are rejected by the database.
	Action string

	// ResourceType is the table-ish noun ("upload", "job", "user").
	// ResourceID is the row's primary key, as a string.
	ResourceType string
	ResourceID   string

	// IP is the caller's IP (X-Forwarded-For aware). UserAgent is the
	// request's User-Agent header.
	IP        string
	UserAgent string

	// RequestID is the per-request correlation id. Defaults to a fresh
	// UUID v4 when empty.
	RequestID string

	// Metadata is opaque JSON. nil is encoded as `{}`.
	Metadata map[string]any
}

// Record inserts an audit log entry. The principal must be in ctx (set
// by the auth middleware via [WithPrincipal]). If the principal is
// missing the call is a no-op and the error is logged at WARN; we do
// not return the error because callers should not change their control
// flow when auditing fails — the request has already been served.
func (r *Recorder) Record(ctx context.Context, e Entry) error {
	if err := r.fillFromPrincipal(ctx, &e); err != nil {
		// No principal = we are not in a request scope (or auth was
		// skipped). Audit is a best-effort side effect; don't fail
		// the caller's transaction.
		r.Logger.Warn("audit.record.no_principal", "err", err)
		return nil
	}
	if e.RequestID == "" {
		e.RequestID = uuid.NewString()
	}

	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("audit.record.marshal_metadata: %w", err)
	}

	// action / actor_type are strongly-typed enums; cast in SQL so the
	// value goes through the same path as a hand-typed literal would.
	const q = `
		INSERT INTO audit_log (
			id, org_id, user_id, actor_type, action,
			resource_type, resource_id, ip, user_agent, request_id, metadata
		)
		VALUES (
			$1, $2,
			$3,
			$4::actor_type,
			$5::audit_action,
			$6, $7,
			$8::inet,
			$9, $10, $11
		)
	`
	// audit_log has FORCE row-level security with an insert policy of
	// `is_service_role() OR org_id = current_org_id()`. Record is called
	// outside any WithTenant scope (from handlers after their own tx, and
	// from the middleware after the handler returns), so we must set the
	// tenant GUC ourselves — a bare pool Exec would be rejected by RLS
	// and every audit row would be silently dropped.
	err = r.DB.WithTenant(ctx, e.OrgID, func(tctx context.Context) error {
		_, execErr := dbtx.Exec(tctx, r.DB, q,
			uuid.NewString(),
			e.OrgID,
			nullableString(e.ActorID),
			e.ActorType,
			e.Action,
			e.ResourceType,
			e.ResourceID,
			nullableString(e.IP),
			nullableString(e.UserAgent),
			nullableString(e.RequestID),
			metaJSON,
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("audit.record.insert: %w", err)
	}
	return nil
}

// fillFromPrincipal copies the principal's identity into any Entry
// field the caller left blank. It is its own method so the standalone
// [Record] path and the [Middleware] path share the same defaults.
func (r *Recorder) fillFromPrincipal(ctx context.Context, e *Entry) error {
	p, err := auth.PrincipalFromContext(ctx)
	if err != nil {
		return err
	}
	if e.OrgID == "" {
		e.OrgID = p.OrgID
	}
	if e.ActorID == "" {
		if p.APIKeyID != "" {
			e.ActorID = p.APIKeyID
		} else {
			e.ActorID = p.UserID
		}
	}
	if e.ActorType == "" {
		switch {
		case p.APIKeyID != "":
			e.ActorType = "apikey"
		case p.UserID != "":
			e.ActorType = "user"
		default:
			e.ActorType = "system"
		}
	}
	return nil
}

// Middleware returns a chi-compatible handler that records every
// state-changing request to the audit log.
//
// It runs *after* the wrapped handler (so we can capture the response
// status), so the audit row reflects what actually happened. Mutations
// (POST/PUT/PATCH/DELETE) are recorded; safe methods (GET/HEAD/OPTIONS)
// are not.
//
// The recorded action and resource are derived from the request method
// and path: e.g. `POST /v1/uploads/abc` becomes action="upload.create"
// resource_type="uploads" resource_id="abc". The action is mapped into
// the audit_action enum where possible; unknown values are coerced to
// the closest enum member or, if the verb is not a mutation, the entry
// is skipped.
func (r *Recorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, req)

		if isSafeMethod(req.Method) {
			return
		}

		action, ok := actionForMethod(req.Method)
		if !ok {
			return
		}
		resourceType, resourceID := deriveResource(req.URL.Path)
		if resourceType == "" {
			return
		}
		action = buildAction(action, resourceType)
		// Generic path→action derivation is lossy: sub-action routes
		// (e.g. .../deliveries/{id}/replay) and verbs the enum does not
		// model (a DELETE on /jobs is a "cancel", not a "delete") yield
		// non-enum shapes. Skip those here — the handler layer audits
		// them with the precise action — rather than issuing an INSERT
		// the DB will reject.
		if !isAuditAction(action) {
			return
		}

		// Best-effort: the request has already been served. If audit
		// fails, log and move on.
		if err := r.Record(req.Context(), Entry{
			Action:       action,
			ResourceType: resourceType,
			ResourceID:   resourceID,
			IP:           clientIP(req),
			UserAgent:    req.UserAgent(),
			Metadata: map[string]any{
				"method": req.Method,
				"path":   req.URL.Path,
				"status": rec.status,
			},
		}); err != nil {
			r.Logger.Error("audit.middleware.record_failed",
				"err", err,
				"path", req.URL.Path,
				"method", req.Method,
			)
		}
	})
}

// isSafeMethod reports whether the method is read-only and should not
// produce an audit row.
func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// actionForMethod maps the HTTP verb to the audit_action verb segment
// and reports whether the verb is a mutation.
func actionForMethod(m string) (string, bool) {
	switch m {
	case http.MethodPost:
		return "create", true
	case http.MethodPut, http.MethodPatch:
		return "update", true
	case http.MethodDelete:
		return "delete", true
	default:
		return "", false
	}
}

// deriveResource splits the URL path into (resource, id). It assumes
// the canonical `/v1/<resource>[/<id>...]` shape; sub-resources are
// concatenated with dots so `/v1/users/abc/api_keys/xyz` becomes
// ("users.api_keys", "xyz").
func deriveResource(path string) (resourceType, resourceID string) {
	const prefix = "/v1/"
	trimmed := strings.TrimPrefix(path, prefix)
	if trimmed == path {
		// Not a v1 path; treat the first segment as the resource.
		trimmed = strings.TrimPrefix(path, "/")
	}
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	resourceType = strings.Join(parts[:len(parts)-1], ".")
	resourceID = parts[len(parts)-1]
	return resourceType, resourceID
}

// buildAction composes a dotted `resource.verb` action to match the
// audit_action enum in 0001_init.sql (e.g. `upload.create`, not
// `create.upload`). The resource segment is normalised — hyphens
// stripped and the trailing plural 's' dropped — so `api-keys` becomes
// `apikey`. The result is only meaningful if it is a valid enum member;
// callers must gate on [isAuditAction] and skip the row otherwise
// (many generic routes, e.g. `/webhooks/{id}/deliveries/{id}/replay`,
// derive shapes that are not enum members and are audited by the
// handler layer instead).
func buildAction(verb, resource string) string {
	return normaliseResource(resource) + "." + verb
}

// normaliseResource turns a URL resource segment into its enum noun:
// strips hyphens (`api-keys` -> `apikeys`) then drops a trailing plural
// 's' (`apikeys` -> `apikey`, `uploads` -> `upload`). Irregular plurals
// are not handled; the API does not currently have any.
func normaliseResource(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	if strings.HasSuffix(s, "s") && len(s) > 1 {
		s = strings.TrimSuffix(s, "s")
	}
	return s
}

// auditActions is the set of valid audit_action enum members, in the
// enum's own `resource.verb` order. Keep this in sync with the
// audit_action enum in the migrations.
var auditActions = map[string]struct{}{
	"org.create": {}, "org.update": {}, "org.delete": {},
	"user.invite": {}, "user.join": {}, "user.leave": {},
	"user.update": {}, "user.remove": {},
	"apikey.create": {}, "apikey.update": {}, "apikey.revoke": {},
	"upload.create": {}, "upload.complete": {},
	"upload.abort": {}, "upload.expire": {},
	"artifact.create": {}, "artifact.update": {}, "artifact.delete": {},
	"job.create": {}, "job.cancel": {}, "job.retry": {}, "job.update": {},
	"webhook.create": {}, "webhook.update": {}, "webhook.delete": {},
	"webhook.deliver": {}, "webhook.delivery_fail": {}, "webhook.delivery_exhausted": {},
	"workflow.create": {}, "workflow.update": {}, "workflow.cancel": {},
	"billing.plan_change": {}, "billing.payment": {}, "billing.refund": {},
	"auth.login": {}, "auth.logout": {}, "auth.token_refresh": {},
	"rbac.role_grant": {}, "rbac.role_revoke": {},
	"settings.update": {},
}

// isAuditAction reports whether s is a valid audit_action enum member.
func isAuditAction(s string) bool {
	_, ok := auditActions[s]
	return ok
}

// IsValidAction reports whether s is a valid audit_action enum value.
// Exported so handlers can validate an ?action= filter before it reaches
// a SQL enum cast (which would 500 on an unknown value).
func IsValidAction(s string) bool {
	return isAuditAction(s)
}

// clientIP returns the first hop from X-Forwarded-For, falling back to
// req.RemoteAddr. This is best-effort: a hostile client can spoof
// X-Forwarded-For. Phase 1+ will plug in a vetted real-IP helper once
// a load balancer is in front of the service (the chi v5.3.1 advisory
// makes the v4 real_ip middleware unsafe in the meantime).
func clientIP(req *http.Request) string {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// nullableString returns nil for "", &s otherwise. The pgx driver
// converts *string into a SQL NULL when the column is nullable, which
// is what the audit_log columns want.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// statusRecorder is a tiny http.ResponseWriter wrapper that captures
// the status code so the audit middleware can log it.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}
