package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type wsTicketEntry struct {
	actor     string
	kind      authKind
	expiresAt time.Time
}

type actorContextKey struct{}
type actorKindContextKey struct{}

type authKind int

const (
	authNone authKind = iota
	authTailscale
	authDevToken
	authDevOpen
)

func withActor(ctx context.Context, actor string, kind protocol.ActorKind) context.Context {
	ctx = context.WithValue(ctx, actorContextKey{}, actor)
	return context.WithValue(ctx, actorKindContextKey{}, kind)
}

func actorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorContextKey{}).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

func actorKindFromContext(ctx context.Context) protocol.ActorKind {
	if v, ok := ctx.Value(actorKindContextKey{}).(protocol.ActorKind); ok && v != "" {
		return v
	}
	return protocol.ActorHuman
}

// ValidateListenAddr unconditionally enforces that orbit-hub binds strictly to a loopback
// interface (127.0.0.1 / ::1 / localhost), even in devOpen mode, so untrusted peers can
// never connect directly or spoof Tailscale-User-Login headers.
func ValidateListenAddr(cfg protocol.HubConfig) error {
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
		return fmt.Errorf("non-loopback listenAddr %q is forbidden (bind 127.0.0.1, ::1, or localhost)", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("non-loopback listenAddr %q is forbidden (bind 127.0.0.1, ::1, or localhost)", addr)
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

		actorKind := protocol.ActorHuman
		// Allow explicit downward privilege scoping for PA-initiated calls (never upward!)
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Orbit-Actor-Kind")), "pa") {
			actorKind = protocol.ActorPA
		}

		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), actor, actorKind)))
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

// blobAuthMiddleware authenticates /api/blobs/{sha256} for either normal human/CLI API callers
// OR authenticated Runners (X-Orbit-Machine-Id + Authorization: Bearer <token>, no browser Origin) per B12.
func (s *Server) blobAuthMiddleware(next http.Handler) http.Handler {
	standard := s.authMiddleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		machineID := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
		if machineID != "" && extractBearerToken(r.Header.Get("Authorization")) != "" {
			if _, statusCode, errMsg := s.verifyRunnerAuth(r, machineID); statusCode != http.StatusOK {
				code := "ERR_UNAUTHORIZED"
				if statusCode == http.StatusForbidden {
					code = "ERR_FORBIDDEN"
				}
				writeAPIError(w, statusCode, code, errMsg)
				return
			}
			next.ServeHTTP(w, r.WithContext(withActor(r.Context(), "runner:"+machineID, protocol.ActorRunner)))
			return
		}
		standard.ServeHTTP(w, r)
	})
}

func (s *Server) issueWSTicket(actor string, kind authKind) (string, int64) {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	ticket := "wst_" + hex.EncodeToString(raw[:])
	now := time.Now()
	exp := now.Add(30 * time.Second)

	s.wsTicketsMu.Lock()
	if s.wsTickets == nil {
		s.wsTickets = map[string]wsTicketEntry{}
	}
	for k, v := range s.wsTickets {
		if now.After(v.expiresAt) {
			delete(s.wsTickets, k)
		}
	}
	s.wsTickets[ticket] = wsTicketEntry{actor: actor, kind: kind, expiresAt: exp}
	s.wsTicketsMu.Unlock()
	return ticket, exp.UnixMilli()
}

func (s *Server) consumeWSTicket(ticket string) (string, authKind, bool) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", authNone, false
	}
	now := time.Now()
	s.wsTicketsMu.Lock()
	defer s.wsTicketsMu.Unlock()
	entry, ok := s.wsTickets[ticket]
	if !ok {
		return "", authNone, false
	}
	delete(s.wsTickets, ticket)
	if now.After(entry.expiresAt) {
		return "", authNone, false
	}
	return entry.actor, entry.kind, true
}

func (s *Server) handleCreateWSTicket(w http.ResponseWriter, r *http.Request) {
	actor, kind, ok := s.authenticate(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "authentication required")
		return
	}
	ticket, expiresAt := s.issueWSTicket(actor, kind)
	writeJSON(w, http.StatusOK, protocol.WSTicketResponse{
		Ticket:    ticket,
		ExpiresAt: expiresAt,
	})
}

func (s *Server) validateWSRequest(r *http.Request) (string, int, string) {
	actor, kind, ok := s.authenticate(r)
	if !ok {
		if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
			actor, kind, ok = s.consumeWSTicket(ticket)
		}
	}
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
	if (cfg.DevOpen || cfg.DevToken != "") && isLoopbackAddr(r.RemoteAddr) {
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

// verifyRunnerAuth validates runner authentication for a machineId against machines.yaml (B5):
// - Unconditionally rejects any browser Origin header (runners are never browser clients).
// - BOTH local_ws (loopback-only) and remote_ws unconditionally require a configured tokenHash
//   and a matching Authorization: Bearer <token> verified in constant time.
func (s *Server) verifyRunnerAuth(r *http.Request, machineID string) (protocol.MachineConfig, int, string) {
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		return protocol.MachineConfig{}, http.StatusForbidden, "browser Origin header is forbidden on runner endpoints"
	}

	s.mu.RLock()
	mCfg, ok := s.machines.Machines[machineID]
	s.mu.RUnlock()
	if !ok {
		return protocol.MachineConfig{}, http.StatusUnauthorized, "unknown machineId"
	}

	bearer := extractBearerToken(r.Header.Get("Authorization"))
	switch mCfg.Transport {
	case protocol.TransportLocalWS:
		if !isLoopbackAddr(r.RemoteAddr) {
			return protocol.MachineConfig{}, http.StatusForbidden, "local_ws runner must connect from loopback"
		}
		if strings.TrimSpace(mCfg.TokenHash) == "" {
			return protocol.MachineConfig{}, http.StatusForbidden, "local_ws machine missing tokenHash in machines.yaml"
		}
		if !matchTokenHash(bearer, mCfg.TokenHash) {
			return protocol.MachineConfig{}, http.StatusUnauthorized, "invalid runner bearer token"
		}
		return mCfg, http.StatusOK, ""

	case protocol.TransportRemoteWS:
		if strings.TrimSpace(mCfg.TokenHash) == "" {
			return protocol.MachineConfig{}, http.StatusForbidden, "remote_ws machine missing tokenHash in machines.yaml"
		}
		if !matchTokenHash(bearer, mCfg.TokenHash) {
			return protocol.MachineConfig{}, http.StatusUnauthorized, "invalid runner bearer token"
		}
		return mCfg, http.StatusOK, ""

	default:
		return protocol.MachineConfig{}, http.StatusForbidden, "machine transport does not support runner websocket"
	}
}

func extractBearerToken(authHeader string) string {
	authHeader = strings.TrimSpace(authHeader)
	if len(authHeader) < 7 || !strings.EqualFold(authHeader[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(authHeader[7:])
}

func matchTokenHash(rawToken, expectedHash string) bool {
	if rawToken == "" || expectedHash == "" {
		return false
	}
	expected := strings.TrimSpace(expectedHash)
	expected = strings.TrimPrefix(strings.ToLower(expected), "sha256:")
	sum := sha256.Sum256([]byte(rawToken))
	actual := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}
