package accesscontrol

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name   string
		secure bool
		origin string
		want   bool
	}{
		{name: "same local origin", origin: "http://app.local", want: true},
		{name: "different host", origin: "http://other.local", want: false},
		{name: "secure origin", secure: true, origin: "https://app.local", want: true},
		{name: "wrong scheme", secure: true, origin: "http://app.local", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://app.local/api", nil)
			r.Header.Set("Origin", test.origin)
			if got := (&HTTPHandler{cookieSecure: test.secure}).sameOrigin(r); got != test.want {
				t.Fatalf("sameOrigin() = %v, mau %v", got, test.want)
			}
		})
	}
}

func TestValidCSRF(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://app.local/api", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "token-123"})
	r.Header.Set("X-CSRF-Token", "token-123")
	if !validCSRF(r, "token-123") {
		t.Fatal("token CSRF yang cocok seharusnya diterima")
	}
	r.Header.Set("X-CSRF-Token", "beda")
	if validCSRF(r, "token-123") {
		t.Fatal("token CSRF yang berbeda seharusnya ditolak")
	}
}

func TestSessionCookieFlags(t *testing.T) {
	h := &HTTPHandler{cookieSecure: true}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "https://app.local", nil)
	h.setSessionCookies(w, r, "sesi-rahasia", "csrf", time.Now().Add(time.Hour))

	cookies := w.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("dapat %d cookie, mau 2", len(cookies))
	}
	if cookies[0].Name != sessionCookieName || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie sesi tidak sesuai: %#v", cookies[0])
	}
	if cookies[1].Name != csrfCookieName || cookies[1].HttpOnly || !cookies[1].Secure || cookies[1].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie CSRF tidak sesuai: %#v", cookies[1])
	}
}

func TestDefaultSessionPolicy(t *testing.T) {
	policy := DefaultSessionPolicy()
	if policy.IdleTimeout != 30*time.Minute || policy.AbsoluteExpiry != 12*time.Hour || policy.RotateEvery != 15*time.Minute || policy.GracePeriod != 30*time.Second {
		t.Fatalf("aturan sesi tidak sesuai: %#v", policy)
	}
}

func TestHTTPHandlerRejectsRequestsWithoutSession(t *testing.T) {
	handler := NewHTTPHandler(nil, nil, false)
	r := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, mau %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHTTPHandlerRejectsCrossOriginLogin(t *testing.T) {
	handler := NewHTTPHandler(nil, nil, false)
	r := httptest.NewRequest(http.MethodPost, "http://app.local/api/auth/login", nil)
	r.Header.Set("Origin", "http://other.local")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, mau %d", w.Code, http.StatusForbidden)
	}
}
