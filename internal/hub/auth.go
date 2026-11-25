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
	actor         string
	kind          authKind
	machineID     string
	clientTokenID string
	expiresAt     time.Time
}

type actorContextKey struct{}
type actorKindContextKey struct{}
type clientMachineIDContextKey struct{}
type clientTokenIDContextKey struct{}

type authKind int

const (
	authNone authKind = iota
	authClientToken
	authDevToken
	authDevOpen
)

func withActor(ctx context.Context, actor string, kind protocol.ActorKind) context.Context {
	ctx = context.WithValue(ctx, actorContextKey{}, actor)
	return context.WithValue(ctx, actorKindContextKey{}, kind)
}

func withClientMachineID(ctx context.Context, machineID string) context.Context {
	if strings.TrimSpace(machineID) == "" {
		return ctx
	}
	return context.WithValue(ctx, clientMachineIDContextKey{}, strings.TrimSpace(machineID))
}

func clientMachineIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientMachineIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

func withClientTokenID(ctx context.Context, tokenID string) context.Context {
	if strings.TrimSpace(tokenID) == "" {
		return ctx
	}
	return context.WithValue(ctx, clientTokenIDContextKey{}, strings.TrimSpace(tokenID))
}

func clientTokenIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientTokenIDContextKey{}).(string); ok {
		return v
	}
	return ""
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

// authMiddleware enforces cookie-less paired client token / local loopback auth + exact Origin + X-Orbit-Request write protection.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, _, clientMachineID, clientTokenID, ok := s.authenticate(r)
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
			if origin != "" && !s.isAllowedOrigin(origin, r) {
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

		ctx := withActor(r.Context(), actor, actorKind)
		ctx = withClientMachineID(ctx, clientMachineID)
		ctx = withClientTokenID(ctx, clientTokenID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) maybeTouchClientToken(tokenID string) {
	if tokenID == "" {
		return
	}
	now := time.Now()
	s.clientTouchMu.Lock()
	if s.clientLastTouch == nil {
		s.clientLastTouch = map[string]time.Time{}
	}
	if last, ok := s.clientLastTouch[tokenID]; ok && now.Sub(last) < time.Minute {
		s.clientTouchMu.Unlock()
		return
	}
	s.clientLastTouch[tokenID] = now
	s.clientTouchMu.Unlock()

	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.store.TouchClientTokenLastUsed(ctx, tokenID)
	}()
}

func (s *Server) authenticate(r *http.Request) (string, authKind, string, string, bool) {
	cfg := s.hubConfigSnapshot()

	// 1. Explicit Authorization: Bearer ock_... paired client token.
	// If an Authorization header is presented and invalid, reject immediately (NEVER fall through!).
	// Tailscale-User-Login alone NEVER grants access; if an allowlist is configured it acts as an extra restriction.
	if rawAuth := strings.TrimSpace(r.Header.Get("Authorization")); rawAuth != "" {
		bearer := extractBearerToken(rawAuth)
		if bearer == "" || !strings.HasPrefix(bearer, "ock_") {
			return "", authNone, "", "", false
		}
		if tsLogin := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); tsLogin != "" && s.hasConfiguredTailscaleLogins() {
			if !s.isAllowedTailscaleLogin(tsLogin) {
				return "", authNone, "", "", false
			}
		}
		info, valid, err := s.store.VerifyClientToken(r.Context(), bearer)
		if err != nil || !valid {
			return "", authNone, "", "", false
		}
		s.maybeTouchClientToken(info.ID)
		return "client:" + info.ID, authClientToken, info.MachineID, info.ID, true
	}

	// 2. Direct loopback development token or --dev-open (direct loopback ONLY; NEVER proxied/Tailscale requests).
	if isLoopbackAddr(r.RemoteAddr) && !hasReverseProxyHeaders(r) {
		if cfg.DevToken != "" {
			provided := r.Header.Get("X-Orbit-Dev-Token")
			if provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.DevToken)) == 1 {
				return "dev-token", authDevToken, "", "", true
			}
		}
		if cfg.DevOpen && isLoopbackHost(r.Host) {
			return "dev-local", authDevOpen, "", "", true
		}
	}

	return "", authNone, "", "", false
}

func isLoopbackHost(hostport string) bool {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return false
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func hasReverseProxyHeaders(r *http.Request) bool {
	for _, h := range []string{
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"Forwarded",
		"Tailscale-User-Login",
		"Tailscale-User-Name",
	} {
		if strings.TrimSpace(r.Header.Get(h)) != "" {
			return true
		}
	}
	return false
}

func (s *Server) isTailscaleSharingActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dynNet.TailscaleShared && strings.TrimSpace(s.dynNet.TailscaleOrigin) != ""
}

func (s *Server) isAllowedTailscaleLogin(tsLogin string) bool {
	tsLogin = strings.TrimSpace(tsLogin)
	if tsLogin == "" {
		return false
	}
	s.mu.RLock()
	yamlLogins := s.hubCfg.AllowedTailscaleLogins
	extraLogins := s.dynNet.ExtraLogins
	s.mu.RUnlock()
	for _, allowed := range yamlLogins {
		if strings.EqualFold(strings.TrimSpace(allowed), tsLogin) {
			return true
		}
	}
	for _, allowed := range extraLogins {
		if strings.EqualFold(strings.TrimSpace(allowed), tsLogin) {
			return true
		}
	}
	return false
}

func (s *Server) hasConfiguredTailscaleLogins() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.hubCfg.AllowedTailscaleLogins) > 0 || len(s.dynNet.ExtraLogins) > 0
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

// runnerAuthMiddleware strictly authenticates Runner-only HTTP endpoints (X-Orbit-Machine-Id + runner Bearer token).
func (s *Server) runnerAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		machineID := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
		if machineID == "" {
			writeAPIError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "missing X-Orbit-Machine-Id header")
			return
		}
		if _, statusCode, errMsg := s.verifyRunnerAuth(r, machineID); statusCode != http.StatusOK {
			code := "ERR_UNAUTHORIZED"
			if statusCode == http.StatusForbidden {
				code = "ERR_FORBIDDEN"
			}
			writeAPIError(w, statusCode, code, errMsg)
			return
		}
		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), "runner:"+machineID, protocol.ActorRunner)))
	})
}

func (s *Server) issueWSTicket(actor string, kind authKind, machineID, clientTokenID string) (string, int64) {
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
	s.wsTickets[ticket] = wsTicketEntry{
		actor:         actor,
		kind:          kind,
		machineID:     machineID,
		clientTokenID: clientTokenID,
		expiresAt:     exp,
	}
	s.wsTicketsMu.Unlock()
	return ticket, exp.UnixMilli()
}

func (s *Server) consumeWSTicket(ctx context.Context, ticket string) (string, authKind, string, bool) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", authNone, "", false
	}
	now := time.Now()
	s.wsTicketsMu.Lock()
	entry, ok := s.wsTickets[ticket]
	if ok {
		delete(s.wsTickets, ticket)
	}
	s.wsTicketsMu.Unlock()
	if !ok || now.After(entry.expiresAt) {
		return "", authNone, "", false
	}
	if entry.kind == authClientToken || entry.clientTokenID != "" {
		active, err := s.store.IsClientTokenActive(ctx, entry.clientTokenID)
		if err != nil || !active {
			return "", authNone, "", false
		}
	}
	return entry.actor, entry.kind, entry.clientTokenID, true
}

func (s *Server) revokeWSTicketsForClientToken(clientTokenID string) {
	clientTokenID = strings.TrimSpace(clientTokenID)
	if clientTokenID == "" {
		return
	}
	s.wsTicketsMu.Lock()
	for k, v := range s.wsTickets {
		if v.clientTokenID == clientTokenID {
			delete(s.wsTickets, k)
		}
	}
	s.wsTicketsMu.Unlock()
}

func (s *Server) handleCreateWSTicket(w http.ResponseWriter, r *http.Request) {
	actor, kind, machineID, clientTokenID, ok := s.authenticate(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "authentication required")
		return
	}
	ticket, expiresAt := s.issueWSTicket(actor, kind, machineID, clientTokenID)
	writeJSON(w, http.StatusOK, protocol.WSTicketResponse{
		Ticket:    ticket,
		ExpiresAt: expiresAt,
	})
}

func (s *Server) validateWSRequest(r *http.Request) (string, string, int, string) {
	actor, kind, _, clientTokenID, ok := s.authenticate(r)
	if !ok {
		if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
			actor, kind, clientTokenID, ok = s.consumeWSTicket(r.Context(), ticket)
		}
	}
	if !ok {
		return "", "", http.StatusUnauthorized, "authentication required"
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		if !s.isAllowedOrigin(origin, r) {
			return "", "", http.StatusForbidden, "websocket origin does not match configured hub origin"
		}
		return actor, clientTokenID, http.StatusOK, ""
	}

	// Non-browser clients without Origin allowed under clientToken / devToken / devOpen
	if kind == authClientToken || ((kind == authDevToken || kind == authDevOpen) && isLoopbackAddr(r.RemoteAddr)) {
		return actor, clientTokenID, http.StatusOK, ""
	}
	return "", "", http.StatusForbidden, "missing Origin header on websocket handshake"
}

func (s *Server) isAllowedOrigin(origin string, r *http.Request) bool {
	s.mu.RLock()
	cfg := s.hubCfg
	extraOrigins := append([]string(nil), s.dynNet.ExtraOrigins...)
	tsOrigin := s.dynNet.TailscaleOrigin
	s.mu.RUnlock()

	normOrigin := normalizeOrigin(origin)
	if normOrigin == "" {
		return false
	}
	if cfg.HubOrigin != "" && normOrigin == normalizeOrigin(cfg.HubOrigin) {
		return true
	}
	if tsOrigin != "" && normOrigin == normalizeOrigin(tsOrigin) {
		return true
	}
	for _, eo := range extraOrigins {
		if eo != "" && normOrigin == normalizeOrigin(eo) {
			return true
		}
	}
	if isLoopbackAddr(r.RemoteAddr) {
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

// verifyRunnerAuth validates runner authentication for a machineId against BOTH
// static machines.yaml tokenHash and dynamic SQLite machine_credentials:
// - Unconditionally rejects any browser Origin header (runners are never browser clients).
// - Rejects client tokens (ock_ prefix cannot open runner endpoints).
// - BOTH local_ws (loopback-only) and remote_ws require a matching bearer token.
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
	if strings.HasPrefix(bearer, "ock_") {
		return protocol.MachineConfig{}, http.StatusUnauthorized, "client token cannot be used on runner endpoints"
	}

	verifyToken := func() (int, string) {
		if strings.TrimSpace(mCfg.TokenHash) != "" && matchTokenHash(bearer, mCfg.TokenHash) {
			return http.StatusOK, ""
		}
		if ok, err := s.store.VerifyMachineCredential(r.Context(), machineID, bearer); err == nil && ok {
			return http.StatusOK, ""
		}
		if strings.TrimSpace(mCfg.TokenHash) == "" {
			if creds, err := s.store.MachinesWithDBCredentials(r.Context()); err == nil && !creds[machineID] {
				return http.StatusForbidden, "machine has no configured runner credentials"
			}
		}
		return http.StatusUnauthorized, "invalid runner bearer token"
	}

	switch mCfg.Transport {
	case protocol.TransportLocalWS:
		if !isLoopbackAddr(r.RemoteAddr) {
			return protocol.MachineConfig{}, http.StatusForbidden, "local_ws runner must connect from loopback"
		}
		if st, msg := verifyToken(); st != http.StatusOK {
			return protocol.MachineConfig{}, st, msg
		}
		return mCfg, http.StatusOK, ""

	case protocol.TransportRemoteWS:
		if st, msg := verifyToken(); st != http.StatusOK {
			return protocol.MachineConfig{}, st, msg
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
