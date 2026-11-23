package hub

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type actorContextKey struct{}

func withActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func actorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorContextKey{}).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

// securityHeadersMiddleware applies baseline security headers and denies cross-origin CORS.
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Reject cross-origin CORS preflight requests explicitly.
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
		actor, ok := s.authenticate(r)
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
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
				if !s.isAllowedOrigin(origin, r) {
					writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "origin does not match configured hub origin")
					return
				}
			}
			if r.Header.Get("X-Orbit-Request") != "1" {
				writeAPIError(w, http.StatusForbidden, "ERR_MISSING_REQUEST_HEADER", "write requests require X-Orbit-Request: 1 header")
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), actor)))
	})
}

func (s *Server) authenticate(r *http.Request) (string, bool) {
	cfg := s.hubConfigSnapshot()

	// 1. Tailscale Serve injected identity header (no cookies ever trusted for auth)
	if tsLogin := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); tsLogin != "" {
		for _, allowed := range cfg.AllowedTailscaleLogins {
			if strings.EqualFold(strings.TrimSpace(allowed), tsLogin) {
				return tsLogin, true
			}
		}
		return "", false
	}

	// 2. Local loopback development token or --dev-open (loopback ONLY)
	if isLoopbackAddr(r.RemoteAddr) {
		if cfg.DevToken != "" {
			provided := r.Header.Get("X-Orbit-Dev-Token")
			if provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.DevToken)) == 1 {
				return "dev-token", true
			}
		}
		if cfg.DevOpen {
			return "dev-local", true
		}
	}

	return "", false
}

func (s *Server) validateWSRequest(r *http.Request) (string, int, string) {
	actor, ok := s.authenticate(r)
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

	// Allow non-browser loopback clients (e.g. CLI/tests) with empty Origin only if loopback + dev auth
	cfg := s.hubConfigSnapshot()
	if isLoopbackAddr(r.RemoteAddr) && (cfg.DevOpen || (cfg.DevToken != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Orbit-Dev-Token")), []byte(cfg.DevToken)) == 1)) {
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
	// In devOpen mode on loopback, allow exact same-host loopback HTTP origin (e.g. 127.0.0.1 or localhost)
	if cfg.DevOpen && isLoopbackAddr(r.RemoteAddr) {
		u, err := url.Parse(normOrigin)
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			host := u.Hostname()
			if host == "127.0.0.1" || host == "localhost" || host == "::1" {
				// Enforce port match with the request Host when present, blocking other ports like :27319
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
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	return scheme + "://" + host
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
