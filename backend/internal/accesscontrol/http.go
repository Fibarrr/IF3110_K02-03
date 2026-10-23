package accesscontrol

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const sessionCookieName = "if3110_session"
const csrfCookieName = "if3110_csrf"

type requestContextKey uint8

const sessionContextKey requestContextKey = 1

type requestSession struct {
	ActiveSession
	Token string
}

type HTTPHandler struct {
	http.Handler
	store        *Store
	sessions     *SessionManager
	cookieSecure bool
}

func NewHTTPHandler(store *Store, sessions *SessionManager, cookieSecure bool) *HTTPHandler {
	h := &HTTPHandler{store: store, sessions: sessions, cookieSecure: cookieSecure}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.Handle("POST /api/auth/logout", h.requireUser(http.HandlerFunc(h.logout)))
	mux.Handle("GET /api/auth/me", h.requireUser(http.HandlerFunc(h.me)))
	mux.Handle("GET /api/auth/databases/{databaseID}/access", h.requireUser(http.HandlerFunc(h.getMyDatabaseAccess)))

	admin := func(handler http.Handler) http.Handler {
		return h.requireUser(h.requirePlatformAdmin(h.requireCSRF(handler)))
	}
	mux.Handle("GET /api/admin/users", h.requireUser(h.requirePlatformAdmin(http.HandlerFunc(h.listUsers))))
	mux.Handle("POST /api/admin/users", admin(http.HandlerFunc(h.createUser)))
	mux.Handle("PATCH /api/admin/users/{userID}/password", admin(http.HandlerFunc(h.setUserPassword)))
	mux.Handle("PATCH /api/admin/users/{userID}/disabled", admin(http.HandlerFunc(h.setUserDisabled)))
	mux.Handle("PATCH /api/admin/users/{userID}/platform-admin", admin(http.HandlerFunc(h.setUserPlatformAdmin)))
	mux.Handle("GET /api/admin/groups", h.requireUser(h.requirePlatformAdmin(http.HandlerFunc(h.listGroups))))
	mux.Handle("POST /api/admin/groups", admin(http.HandlerFunc(h.createGroup)))
	mux.Handle("DELETE /api/admin/groups/{groupID}", admin(http.HandlerFunc(h.deleteGroup)))
	mux.Handle("GET /api/admin/groups/{groupID}/members", h.requireUser(h.requirePlatformAdmin(http.HandlerFunc(h.listMembers))))
	mux.Handle("GET /api/admin/groups/{groupID}/databases", h.requireUser(h.requirePlatformAdmin(http.HandlerFunc(h.listDatabaseGrants))))
	mux.Handle("PUT /api/admin/groups/{groupID}/members/{userID}", admin(http.HandlerFunc(h.addMember)))
	mux.Handle("DELETE /api/admin/groups/{groupID}/members/{userID}", admin(http.HandlerFunc(h.removeMember)))
	mux.Handle("PUT /api/admin/groups/{groupID}/databases/{databaseID}/access", admin(http.HandlerFunc(h.setDatabaseAccess)))
	mux.Handle("DELETE /api/admin/groups/{groupID}/databases/{databaseID}/access", admin(http.HandlerFunc(h.removeDatabaseAccess)))

	h.Handler = mux
	return h
}

// RequireUser dipakai modul lain supaya endpoint-nya ikut memakai sesi Access Control.
func (h *HTTPHandler) RequireUser(next http.Handler) http.Handler {
	return h.requireUser(next)
}

// RequirePlatformAdmin ngecek user dari context, jadi bungkus dengan RequireUser dulu.
func (h *HTTPHandler) RequirePlatformAdmin(next http.Handler) http.Handler {
	return h.requirePlatformAdmin(next)
}

// RequireCSRF ngecek origin dan token untuk request yang mengubah data.
func (h *HTTPHandler) RequireCSRF(next http.Handler) http.Handler {
	return h.requireCSRF(next)
}

// RequireDatabaseAccess mengecek sesi dan izin user sebelum handler dijalankan.
func (h *HTTPHandler) RequireDatabaseAccess(databaseID func(*http.Request) string, required DatabaseAccess, next http.Handler) http.Handler {
	return h.requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := sessionFromRequest(r)
		access, err := h.store.EffectiveAccess(r.Context(), current.User.ID, databaseID(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "gagal mengecek akses")
			return
		}
		allowed := required == AccessViewOnly && access.CanView() || required == AccessViewEdit && access.CanEdit()
		if !allowed {
			writeError(w, http.StatusForbidden, "akses ke database ditolak")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// UserFromContext mengambil user yang sudah lolos pemeriksaan sesi.
func UserFromContext(ctx context.Context) (User, bool) {
	current, ok := ctx.Value(sessionContextKey).(requestSession)
	return current.User, ok
}

func (h *HTTPHandler) login(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin tidak valid")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	user, err := h.store.VerifyCredentials(r.Context(), input.Username, input.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "username atau password salah")
		} else {
			writeError(w, http.StatusInternalServerError, "gagal memproses login")
		}
		return
	}
	token, csrf, expiresAt, err := h.sessions.Create(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal membuat sesi")
		return
	}
	h.setSessionCookies(w, r, token, csrf, expiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "csrfToken": csrf})
}

func (h *HTTPHandler) logout(w http.ResponseWriter, r *http.Request) {
	current := sessionFromRequest(r)
	if !validCSRF(r, current.CSRFToken) {
		writeError(w, http.StatusForbidden, "token CSRF tidak valid")
		return
	}
	if err := h.sessions.Revoke(r.Context(), current.Token); err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengakhiri sesi")
		return
	}
	h.clearSessionCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) me(w http.ResponseWriter, r *http.Request) {
	current := sessionFromRequest(r)
	writeJSON(w, http.StatusOK, map[string]any{"user": current.User, "csrfToken": current.CSRFToken, "expiresAt": current.ExpiresAt})
}

func (h *HTTPHandler) getMyDatabaseAccess(w http.ResponseWriter, r *http.Request) {
	current := sessionFromRequest(r)
	access, err := h.store.EffectiveAccess(r.Context(), current.User.ID, r.PathValue("databaseID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengecek akses")
		return
	}
	writeJSON(w, http.StatusOK, map[string]DatabaseAccess{"access": access})
}

func (h *HTTPHandler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengambil daftar pengguna")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (h *HTTPHandler) createUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	user, err := h.store.CreateUser(r.Context(), input.Username, input.Password)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (h *HTTPHandler) setUserDisabled(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	var input struct {
		Disabled bool `json:"disabled"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := h.store.SetUserDisabled(r.Context(), userID, input.Disabled); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) setUserPassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := h.store.SetUserPassword(r.Context(), userID, input.Password); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) setUserPlatformAdmin(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	var input struct {
		PlatformAdmin bool `json:"platformAdmin"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := h.store.SetUserPlatformAdmin(r.Context(), userID, input.PlatformAdmin); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.store.ListGroups(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengambil daftar grup")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (h *HTTPHandler) listMembers(w http.ResponseWriter, r *http.Request) {
	groupID, ok := pathID(w, r, "groupID")
	if !ok {
		return
	}
	users, err := h.store.ListGroupMembers(r.Context(), groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": users})
}

func (h *HTTPHandler) listDatabaseGrants(w http.ResponseWriter, r *http.Request) {
	groupID, ok := pathID(w, r, "groupID")
	if !ok {
		return
	}
	grants, err := h.store.ListDatabaseGrants(r.Context(), groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": grants})
}

func (h *HTTPHandler) createGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	group, err := h.store.CreateGroup(r.Context(), input.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, group)
}

func (h *HTTPHandler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := pathID(w, r, "groupID")
	if !ok {
		return
	}
	if err := h.store.DeleteGroup(r.Context(), groupID); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) addMember(w http.ResponseWriter, r *http.Request) {
	groupID, ok := pathID(w, r, "groupID")
	if !ok {
		return
	}
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	if err := h.store.AddMember(r.Context(), groupID, userID); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) removeMember(w http.ResponseWriter, r *http.Request) {
	groupID, ok := pathID(w, r, "groupID")
	if !ok {
		return
	}
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	if err := h.store.RemoveMember(r.Context(), groupID, userID); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) setDatabaseAccess(w http.ResponseWriter, r *http.Request) {
	groupID, ok := pathID(w, r, "groupID")
	if !ok {
		return
	}
	var input struct {
		Access DatabaseAccess `json:"access"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := h.store.SetDatabaseAccess(r.Context(), groupID, r.PathValue("databaseID"), input.Access); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) removeDatabaseAccess(w http.ResponseWriter, r *http.Request) {
	groupID, ok := pathID(w, r, "groupID")
	if !ok {
		return
	}
	if err := h.store.RemoveDatabaseAccess(r.Context(), groupID, r.PathValue("databaseID")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "perlu login")
			return
		}
		active, err := h.sessions.Load(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				h.clearSessionCookies(w, r)
				writeError(w, http.StatusUnauthorized, "sesi tidak berlaku")
			} else {
				writeError(w, http.StatusInternalServerError, "gagal memeriksa sesi")
			}
			return
		}
		if active.RotatedToken != "" {
			h.setSessionCookies(w, r, active.RotatedToken, active.CSRFToken, active.ExpiresAt)
		}
		current := requestSession{ActiveSession: active, Token: cookie.Value}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey, current)))
	})
}

func (h *HTTPHandler) requirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := sessionFromRequest(r)
		if !current.User.PlatformAdmin {
			writeError(w, http.StatusForbidden, "khusus Platform Admin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *HTTPHandler) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.sameOrigin(r) || !validCSRF(r, sessionFromRequest(r).CSRFToken) {
			writeError(w, http.StatusForbidden, "request CSRF ditolak")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *HTTPHandler) setSessionCookies(w http.ResponseWriter, r *http.Request, token, csrf string, expiresAt time.Time) {
	secure := h.cookieSecure || r.TLS != nil
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, Expires: expiresAt})
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: csrf, Path: "/", Secure: secure, SameSite: http.SameSiteStrictMode, Expires: expiresAt})
}

func (h *HTTPHandler) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := h.cookieSecure || r.TLS != nil
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "", Path: "/", Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func validCSRF(r *http.Request, expected string) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		return false
	}
	provided := r.Header.Get("X-CSRF-Token")
	return expected != "" && len(provided) == len(expected) && len(cookie.Value) == len(expected) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1 &&
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(expected)) == 1
}

func (h *HTTPHandler) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	wantScheme := "http"
	if h.cookieSecure || r.TLS != nil {
		wantScheme = "https"
	}
	return err == nil && parsed.User == nil && parsed.Host != "" && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" &&
		strings.EqualFold(parsed.Scheme, wantScheme) && strings.EqualFold(parsed.Host, r.Host)
}

func sessionFromRequest(r *http.Request) requestSession {
	current, _ := r.Context().Value(sessionContextKey).(requestSession)
	return current
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

func pathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "ID tidak valid")
		return 0, false
	}
	return id, true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "data tidak ditemukan")
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
