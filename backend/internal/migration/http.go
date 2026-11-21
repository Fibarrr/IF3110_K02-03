package migration

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/telemetry"
)

type AuthGuard interface {
	RequireCSRF(next http.Handler) http.Handler
	RequireDatabaseAccess(databaseID func(*http.Request) string, required accesscontrol.DatabaseAccess, next http.Handler) http.Handler
}

type HTTPHandler struct {
	http.Handler
	store *Store
	auth  AuthGuard
}

func NewHTTPHandler(store *Store, auth AuthGuard) *HTTPHandler {
	h := &HTTPHandler{store: store, auth: auth}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	h.Handler = mux
	return h
}

func (h *HTTPHandler) RegisterRoutes(mux *http.ServeMux) {
	databaseID := func(r *http.Request) string { return normalizeDatabaseID(r.PathValue("databaseID")) }
	withTelemetry := func(handler http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if user, ok := accesscontrol.UserFromContext(ctx); ok {
				ctx = telemetry.WithActor(ctx, telemetry.Actor{
					UserID: user.ID, Username: user.Username, PlatformAdmin: user.PlatformAdmin,
				})
			}
			if dbID := databaseID(r); dbID != "" {
				ctx = telemetry.WithDatabaseID(ctx, dbID)
			}
			handler(w, r.WithContext(ctx))
		})
	}
	view := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireDatabaseAccess(databaseID, accesscontrol.AccessViewOnly, withTelemetry(handler))
	}
	viewMutate := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireDatabaseAccess(databaseID, accesscontrol.AccessViewOnly, h.auth.RequireCSRF(withTelemetry(handler)))
	}
	edit := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireDatabaseAccess(databaseID, accesscontrol.AccessViewEdit, h.auth.RequireCSRF(withTelemetry(handler)))
	}

	mux.Handle("GET /api/databases/{databaseID}/approvers", view(h.listApprovers))
	mux.Handle("GET /api/databases/{databaseID}/migrations", view(h.list))
	mux.Handle("POST /api/databases/{databaseID}/migrations", edit(h.create))
	mux.Handle("GET /api/databases/{databaseID}/migrations/{migrationID}", view(h.get))
	mux.Handle("PUT /api/databases/{databaseID}/migrations/{migrationID}", edit(h.update))
	mux.Handle("POST /api/databases/{databaseID}/migrations/{migrationID}/execute", edit(h.legacyExecute))

	mux.Handle("POST /api/databases/{databaseID}/migrations/{migrationID}/review-requests", edit(h.createReviewRequest))
	mux.Handle("GET /api/databases/{databaseID}/review-requests", view(h.listReviewRequests))
	mux.Handle("GET /api/databases/{databaseID}/review-requests/{requestID}", view(h.getReviewRequest))
	mux.Handle("PUT /api/databases/{databaseID}/review-requests/{requestID}/approvals/{groupID}", viewMutate(h.recordApproval))
	mux.Handle("POST /api/databases/{databaseID}/review-requests/{requestID}/queue", edit(h.queueReviewRequest))

	mux.Handle("GET /api/databases/{databaseID}/jobs", view(h.listJobs))
	mux.Handle("GET /api/databases/{databaseID}/jobs/{jobID}", view(h.getJob))
	mux.Handle("POST /api/databases/{databaseID}/jobs/{jobID}/resume", edit(h.resumeJob))
	mux.Handle("POST /api/databases/{databaseID}/jobs/{jobID}/resolve-uncertain", edit(h.resolveUncertainJob))
}

func (h *HTTPHandler) listApprovers(w http.ResponseWriter, r *http.Request) {
	approvers, err := h.store.ListDatabaseApprovers(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvers": approvers})
}

func (h *HTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("databaseID")
	hideRollback := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("hideRollback")), "true")
	migrations, err := h.store.List(r.Context(), dbID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengambil daftar migration")
		return
	}
	history, err := h.store.ListHistory(r.Context(), dbID, hideRollback)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengambil riwayat eksekusi migration")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"migrations": migrations, "history": history, "hideRollback": hideRollback,
	})
}

func (h *HTTPHandler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathInt64(w, r, "migrationID", "ID migration tidak valid")
	if !ok {
		return
	}
	dbID := r.PathValue("databaseID")
	m, err := h.store.Get(r.Context(), dbID, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	revisions, err := h.store.ListRevisions(r.Context(), dbID, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	history, err := h.store.ListMigrationHistory(r.Context(), dbID, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"migration": m, "revisions": revisions, "history": history})
}

func (h *HTTPHandler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromRequest(w, r)
	if !ok {
		return
	}
	var input Input
	if !decodeJSON(w, r, &input) {
		return
	}
	m, err := h.store.Create(r.Context(), r.PathValue("databaseID"), actor.UserID, input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *HTTPHandler) update(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromRequest(w, r)
	if !ok {
		return
	}
	id, ok := parsePathInt64(w, r, "migrationID", "ID migration tidak valid")
	if !ok {
		return
	}
	var input Input
	if !decodeJSON(w, r, &input) {
		return
	}
	m, err := h.store.Update(r.Context(), r.PathValue("databaseID"), id, actor.UserID, input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *HTTPHandler) legacyExecute(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusConflict, ErrDirectExecBlocked.Error())
}

func (h *HTTPHandler) createReviewRequest(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromRequest(w, r)
	if !ok {
		return
	}
	migrationID, ok := parsePathInt64(w, r, "migrationID", "ID migration tidak valid")
	if !ok {
		return
	}
	var input ReviewRequestInput
	if !decodeJSON(w, r, &input) {
		return
	}
	req, err := h.store.CreateReviewRequest(r.Context(), r.PathValue("databaseID"), migrationID, actor, input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

func (h *HTTPHandler) listReviewRequests(w http.ResponseWriter, r *http.Request) {
	reqs, err := h.store.ListReviewRequests(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviewRequests": reqs})
}

func (h *HTTPHandler) getReviewRequest(w http.ResponseWriter, r *http.Request) {
	requestID, ok := parsePathInt64(w, r, "requestID", "ID review request tidak valid")
	if !ok {
		return
	}
	req, err := h.store.GetReviewRequest(r.Context(), r.PathValue("databaseID"), requestID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (h *HTTPHandler) recordApproval(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromRequest(w, r)
	if !ok {
		return
	}
	requestID, ok := parsePathInt64(w, r, "requestID", "ID review request tidak valid")
	if !ok {
		return
	}
	groupID, ok := parsePathInt64(w, r, "groupID", "ID grup tidak valid")
	if !ok {
		return
	}
	var input ApprovalInput
	if !decodeJSON(w, r, &input) {
		return
	}
	req, err := h.store.RecordApproval(r.Context(), r.PathValue("databaseID"), requestID, groupID, actor, input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (h *HTTPHandler) queueReviewRequest(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromRequest(w, r)
	if !ok {
		return
	}
	requestID, ok := parsePathInt64(w, r, "requestID", "ID review request tidak valid")
	if !ok {
		return
	}
	job, err := h.store.EnqueueReviewRequest(r.Context(), r.PathValue("databaseID"), requestID, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (h *HTTPHandler) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.store.ListJobs(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (h *HTTPHandler) getJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parsePathInt64(w, r, "jobID", "ID job tidak valid")
	if !ok {
		return
	}
	job, err := h.store.GetJob(r.Context(), r.PathValue("databaseID"), jobID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	checkpoints, err := h.store.ListJobCheckpoints(r.Context(), jobID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job, "checkpoints": checkpoints})
}

func (h *HTTPHandler) resumeJob(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromRequest(w, r)
	if !ok {
		return
	}
	jobID, ok := parsePathInt64(w, r, "jobID", "ID job tidak valid")
	if !ok {
		return
	}
	var input struct {
		ReviewRequestID int64 `json:"reviewRequestId"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &input) {
			return
		}
	}
	job, err := h.store.EnqueueResumeJob(r.Context(), r.PathValue("databaseID"), jobID, input.ReviewRequestID, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (h *HTTPHandler) resolveUncertainJob(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromRequest(w, r)
	if !ok {
		return
	}
	jobID, ok := parsePathInt64(w, r, "jobID", "ID job tidak valid")
	if !ok {
		return
	}
	var input struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	job, err := h.store.ResolveUncertainJob(r.Context(), r.PathValue("databaseID"), jobID, actor, input.Status, input.Note)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func actorFromRequest(w http.ResponseWriter, r *http.Request) (ActorInfo, bool) {
	user, ok := accesscontrol.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "perlu login")
		return ActorInfo{}, false
	}
	return ActorInfo{UserID: user.ID, Username: user.Username}, true
}

func parsePathInt64(w http.ResponseWriter, r *http.Request, key, msg string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, msg)
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "body JSON tidak valid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body harus berisi satu objek JSON")
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, databasemanagement.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNotGroupLeader):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrAlreadyApplied),
		errors.Is(err, ErrAlreadyRunning),
		errors.Is(err, ErrNotEditable),
		errors.Is(err, ErrOutOfOrder),
		errors.Is(err, ErrIrreversible),
		errors.Is(err, ErrMissingLeader),
		errors.Is(err, ErrNoApproverGroups),
		errors.Is(err, ErrApprovalInvalid),
		errors.Is(err, ErrNotApproved),
		errors.Is(err, ErrDatabaseBlocked):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, databasemanagement.ErrTargetConnection):
		writeError(w, http.StatusBadGateway, "gagal terhubung ke database target")
	default:
		var inputErr InputError
		if errors.As(err, &inputErr) {
			writeError(w, http.StatusBadRequest, inputErr.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "terjadi kesalahan di server")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengenkode respons JSON")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
