package hub

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type actorContextKey struct{}

type authKind int

const (
	authNone authKind = iota
	authTailscale
	authDevToken
	authDevOpen
)

func withActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func actorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorContextKey{}).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

// ValidateListenAddr enforces that when Tailscale-User-Login auth is active (!DevOpen),
// orbit-hub MUST bind strictly to a loopback interface (127.0.0.1 / ::1 / localhost) so
// untrusted peers cannot spoof Tailscale-User-Login headers by connecting directly.
func ValidateListenAddr(cfg protocol.HubConfig) error {
	if cfg.DevOpen {
		return nil
	}
	addr := strings.TrimSpace(cfg.ListenAddr)
	if addr == "" {
		return fmt.Errorf("listenAddr is required")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listenAddr %q: %w", addr, err)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return fmt.Errorf("non-loopback listenAddr %q is forbidden when Tailscale header auth is enabled (bind 127.0.0.1 or ::1)", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("non-loopback listenAddr %q is forbidden when Tailscale header auth is enabled (bind 127.0.0.1 or ::1)", addr)
	}
	return nil
}

// securityHeadersMiddleware applies baseline security headers and denies cross-origin CORS.
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if r.Method == http.MethodOptions && r.Header.Get("Origin") != "" {
			if !s.isAllowedOrigin(r.Header.Get("Origin"), r) {
				writeAPIError(w, http.StatusForbidden, "ERR_CORS_FORBIDDEN", "cross-origin requests are not allowed")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware enforces cookie-less Tailscale auth + exact Origin + X-Orbit-Request write protection.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, kind, ok := s.authenticate(r)
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "authentication required")
			return
		}

		// Cross-site defense for state-changing requests
		if isWriteMethod(r.Method) {
			if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
				writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "cross-site write request rejected")
				return
			}
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if kind == authTailscale {
				// Tailscale-authenticated writes MUST carry an exact matching Origin header
				// (mirroring WS handshake rules). Local CLI uses X-Orbit-Dev-Token instead.
				if origin == "" || !s.isAllowedOrigin(origin, r) {
					writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "write requests authenticated via Tailscale require exact matching Origin header")
					return
				}
			} else if origin != "" && !s.isAllowedOrigin(origin, r) {
				writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "origin does not match configured hub origin")
				return
			}
			if r.Header.Get("X-Orbit-Request") != "1" {
				writeAPIError(w, http.StatusForbidden, "ERR_MISSING_REQUEST_HEADER", "write requests require X-Orbit-Request: 1 header")
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), actor)))
	})
}

func (s *Server) authenticate(r *http.Request) (string, authKind, bool) {
	cfg := s.hubConfigSnapshot()

	// 1. Tailscale Serve injected identity header (no cookies ever trusted for auth)
	if tsLogin := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); tsLogin != "" {
		for _, allowed := range cfg.AllowedTailscaleLogins {
			if strings.EqualFold(strings.TrimSpace(allowed), tsLogin) {
				return tsLogin, authTailscale, true
			}
		}
		return "", authNone, false
	}

	// 2. Local loopback development token or --dev-open (loopback ONLY)
	if isLoopbackAddr(r.RemoteAddr) {
		if cfg.DevToken != "" {
			provided := r.Header.Get("X-Orbit-Dev-Token")
			if provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.DevToken)) == 1 {
				return "dev-token", authDevToken, true
			}
		}
		if cfg.DevOpen {
			return "dev-local", authDevOpen, true
		}
	}

	return "", authNone, false
}

func (s *Server) validateWSRequest(r *http.Request) (string, int, string) {
	actor, kind, ok := s.authenticate(r)
	if !ok {
		return "", http.StatusUnauthorized, "authentication required"
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		if !s.isAllowedOrigin(origin, r) {
			return "", http.StatusForbidden, "websocket origin does not match configured hub origin"
		}
		return actor, http.StatusOK, ""
	}

	// Non-browser loopback clients without Origin allowed only under devToken / devOpen
	if (kind == authDevToken || kind == authDevOpen) && isLoopbackAddr(r.RemoteAddr) {
		return actor, http.StatusOK, ""
	}
	return "", http.StatusForbidden, "missing Origin header on websocket handshake"
}

func (s *Server) isAllowedOrigin(origin string, r *http.Request) bool {
	cfg := s.hubConfigSnapshot()
	normOrigin := normalizeOrigin(origin)
	if normOrigin == "" {
		return false
	}
	if cfg.HubOrigin != "" && normOrigin == normalizeOrigin(cfg.HubOrigin) {
		return true
	}
	if cfg.DevOpen && isLoopbackAddr(r.RemoteAddr) {
		u, err := url.Parse(normOrigin)
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			host := u.Hostname()
			if host == "127.0.0.1" || host == "localhost" || host == "::1" {
				reqHost := r.Host
				if _, reqPort, err := net.SplitHostPort(reqHost); err == nil {
					if u.Port() == reqPort {
						return true
					}
				} else if u.Port() == "" {
					return true
				}
			}
		}
	}
	return false
}

func normalizeOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
