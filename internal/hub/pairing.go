package hub

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

type authHandoffEntry struct {
	actor           string
	machineID       string
	sourceTokenHash string
	expiresAt       time.Time
}

// pairingRateLimiter enforces:
// - at most 10 claim attempts per rolling minute (429), reserved atomically before DB lookup
// - 50 failed claim attempts within 1 hour triggers a 15-minute lockout circuit breaker
type pairingRateLimiter struct {
	mu           sync.Mutex
	attemptTimes []time.Time
	failedTimes  []time.Time
	lockedUntil  time.Time
}

func newPairingRateLimiter() *pairingRateLimiter {
	return &pairingRateLimiter{}
}

func (l *pairingRateLimiter) checkAndReserve(now time.Time) (blocked bool, circuitOpen bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.lockedUntil) {
		return true, true
	}
	l.pruneLocked(now)
	if len(l.attemptTimes) >= 10 {
		return true, false
	}
	l.attemptTimes = append(l.attemptTimes, now)
	return false, false
}

func (l *pairingRateLimiter) recordFailure(now time.Time) (trippedCircuit bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	l.failedTimes = append(l.failedTimes, now)
	if len(l.failedTimes) >= 50 && !now.Before(l.lockedUntil) {
		l.lockedUntil = now.Add(15 * time.Minute)
		return true
	}
	return false
}

func (l *pairingRateLimiter) pruneLocked(now time.Time) {
	minCutoff := now.Add(-1 * time.Minute)
	keepAttempts := l.attemptTimes[:0]
	for _, ts := range l.attemptTimes {
		if ts.After(minCutoff) {
			keepAttempts = append(keepAttempts, ts)
		}
	}
	l.attemptTimes = keepAttempts

	hourCutoff := now.Add(-1 * time.Hour)
	keepFails := l.failedTimes[:0]
	for _, ts := range l.failedTimes {
		if ts.After(hourCutoff) {
			keepFails = append(keepFails, ts)
		}
	}
	l.failedTimes = keepFails
}

// generatePairingCode returns an 8-character Crockford Base32 code (40 bits of entropy)
// formatted as XXXX-XXXX.
func generatePairingCode() (formatted string, normalized string, err error) {
	max := big.NewInt(int64(len(crockfordAlphabet)))
	var buf [8]byte
	for i := range buf {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", "", err
		}
		buf[i] = crockfordAlphabet[n.Int64()]
	}
	normalized = string(buf[:])
	formatted = normalized[:4] + "-" + normalized[4:]
	return formatted, normalized, nil
}

// normalizePairingCode normalizes user input (strip separators/whitespace, uppercase, O->0, I/L->1).
func normalizePairingCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch r {
		case '-', ' ', '\t', '\n', '\r', '_':
			continue
		case 'O':
			b.WriteByte('0')
		case 'I', 'L':
			b.WriteByte('1')
		default:
			if strings.ContainsRune(crockfordAlphabet, r) {
				b.WriteRune(r)
			} else {
				return ""
			}
		}
	}
	out := b.String()
	if len(out) != 8 {
		return ""
	}
	return out
}

func isLoopbackOrigin(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Host)
}

func (s *Server) liveEffectiveHubOrigin(tsShared bool, tsOrigin string) string {
	tsOrigin = strings.TrimRight(strings.TrimSpace(tsOrigin), "/")
	if tsShared && tsOrigin != "" {
		return tsOrigin
	}
	s.mu.RLock()
	hubOrigin := strings.TrimRight(strings.TrimSpace(s.hubCfg.HubOrigin), "/")
	listenAddr := strings.TrimSpace(s.hubCfg.ListenAddr)
	s.mu.RUnlock()

	if hubOrigin != "" {
		return hubOrigin
	}
	if strings.HasPrefix(listenAddr, "http://") || strings.HasPrefix(listenAddr, "https://") {
		return strings.TrimRight(listenAddr, "/")
	}
	if listenAddr == "" {
		listenAddr = "127.0.0.1:4310"
	}
	return "http://" + listenAddr
}

func (s *Server) effectiveHubOrigin() string {
	s.mu.RLock()
	tsOrigin := strings.TrimRight(strings.TrimSpace(s.dynNet.TailscaleOrigin), "/")
	tsShared := s.dynNet.TailscaleShared
	s.mu.RUnlock()
	return s.liveEffectiveHubOrigin(tsShared, tsOrigin)
}

func hubOriginToRunnerWS(origin string) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if strings.HasPrefix(origin, "https://") {
		return "wss://" + strings.TrimPrefix(origin, "https://") + "/ws/runner"
	}
	if strings.HasPrefix(origin, "http://") {
		return "ws://" + strings.TrimPrefix(origin, "http://") + "/ws/runner"
	}
	return "ws://" + origin + "/ws/runner"
}

func (s *Server) handleCreatePairingCode(w http.ResponseWriter, r *http.Request) {
	if actorKindFromContext(r.Context()) != protocol.ActorHuman {
		writeAPIError(w, http.StatusForbidden, "ERR_FORBIDDEN", "pairing codes can only be issued by a human operator")
		return
	}
	var req protocol.CreatePairingCodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	scope := req.Scope
	if scope == "" {
		scope = protocol.PairingScopeBoth
	}
	if !scope.IsValid() {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_SCOPE", "scope must be runner, client, or both")
		return
	}
	bindID := strings.TrimSpace(req.BindMachineID)
	if bindID != "" {
		s.mu.RLock()
		_, exists := s.machines.Machines[bindID]
		s.mu.RUnlock()
		if !exists {
			writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", fmt.Sprintf("machine %q not found", bindID))
			return
		}
		if scope == protocol.PairingScopeClient {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_SCOPE", "bindMachineId requires runner or both scope")
			return
		}
	}

	role := req.Role
	if role == "" {
		role = protocol.MachineRoleFull
	}
	if !role.IsValid() {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_ROLE", "role must be full or worker")
		return
	}

	formatted, normalized, err := generatePairingCode()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PAIRING_GEN", err.Error())
		return
	}
	rec, err := s.store.CreatePairingCode(r.Context(), store.CreatePairingCodeParams{
		CodeHash:      store.HashSecret(normalized),
		Scope:         scope,
		Role:          role,
		BindMachineID: bindID,
		CreatedBy:     actorFromContext(r.Context()),
		TTLSeconds:    req.TTLSeconds,
		MaxActive:     5,
	})
	if errors.Is(err, store.ErrTooManyActiveCodes) {
		writeAPIError(w, http.StatusTooManyRequests, "ERR_TOO_MANY_PAIRING_CODES", "too many active pairing codes; revoke an unused code or wait for expiry")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PAIRING_CREATE", err.Error())
		return
	}

	discID, _ := s.store.EnsureDiscoveryID(r.Context())
	fp := derivePairingFingerprint(normalized, discID)
	_, tsShared, tsOrigin, _, _ := s.probeTailscaleState(r.Context())
	if tsShared && tsOrigin != "" {
		s.mu.RLock()
		needsSync := !s.dynNet.TailscaleShared || strings.TrimRight(s.dynNet.TailscaleOrigin, "/") != strings.TrimRight(tsOrigin, "/")
		s.mu.RUnlock()
		if needsSync {
			s.recordTailscaleSharing(context.WithoutCancel(r.Context()), tsOrigin, actorFromContext(r.Context()))
		}
	}
	origin := s.liveEffectiveHubOrigin(tsShared, tsOrigin)
	joinCmd := fmt.Sprintf("orbit join %s %s", origin, formatted)
	joinLink := fmt.Sprintf("%s/#/join/%s", origin, formatted)
	switch scope {
	case protocol.PairingScopeClient:
		joinCmd += " --no-runner"
		joinLink += "?scope=client"
	case protocol.PairingScopeRunner:
		joinCmd += " --runner-only"
		joinLink += "?scope=runner"
	}
	if role == protocol.MachineRoleWorker {
		joinCmd += " --role worker"
	}

	writeJSON(w, http.StatusCreated, protocol.CreatePairingCodeResponse{
		ID:            rec.ID,
		Code:          formatted,
		Scope:         rec.Scope,
		Role:          rec.Role,
		DiscoveryID:   discID,
		Fingerprint:   fp,
		BindMachineID: rec.BindMachineID,
		ExpiresAt:     rec.ExpiresAt,
		JoinCommand:   joinCmd,
		JoinLink:      joinLink,
		LoopbackOnly:  isLoopbackOrigin(origin),
	})
}

func derivePairingFingerprint(normalizedCode, discoveryID string) string {
	h := store.HashSecret(normalizedCode + ":" + discoveryID)
	var num uint32
	for i := 0; i < 8 && i < len(h); i++ {
		num = num*16 + uint32(hexNib(h[i]))
	}
	six := num % 1000000
	return fmt.Sprintf("%03d %03d", six/1000, six%1000)
}

func hexNib(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 0
	}
}

func (s *Server) handleGetPairingCode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "pairingId")
	rec, err := s.store.GetPairingCode(r.Context(), id, time.Now().UnixMilli())
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PAIRING_NOT_FOUND", "pairing code not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PAIRING_LOOKUP", err.Error())
		return
	}
	online := rec.ClaimedMachineID != "" && s.runners.IsConnected(rec.ClaimedMachineID)
	writeJSON(w, http.StatusOK, protocol.PairingCodeStatusResponse{
		ID:            rec.ID,
		Scope:         rec.Scope,
		Role:          rec.Role,
		BindMachineID: rec.BindMachineID,
		State:         rec.State,
		MachineID:     rec.ClaimedMachineID,
		Online:        online,
		ExpiresAt:     rec.ExpiresAt,
		ClaimedAt:     rec.ClaimedAt,
	})
}

func (s *Server) handleRevokePairingCode(w http.ResponseWriter, r *http.Request) {
	if actorKindFromContext(r.Context()) != protocol.ActorHuman {
		writeAPIError(w, http.StatusForbidden, "ERR_FORBIDDEN", "pairing codes can only be revoked by a human operator")
		return
	}
	id := chi.URLParam(r, "pairingId")
	rec, ev, err := s.store.RevokePairingCode(r.Context(), id, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PAIRING_NOT_FOUND", "pairing code not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PAIRING_REVOKE", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, protocol.PairingCodeStatusResponse{
		ID:            rec.ID,
		Scope:         rec.Scope,
		Role:          rec.Role,
		BindMachineID: rec.BindMachineID,
		State:         rec.State,
		MachineID:     rec.ClaimedMachineID,
		Online:        rec.ClaimedMachineID != "" && s.runners.IsConnected(rec.ClaimedMachineID),
		ExpiresAt:     rec.ExpiresAt,
		ClaimedAt:     rec.ClaimedAt,
	})
}

func (s *Server) handleClaimPairingCode(w http.ResponseWriter, r *http.Request) {
	// Reject any browser-originated request unconditionally; pairing claim is server/CLI/Desktop-to-Hub only.
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "browser Origin header is forbidden on pairing claim")
		return
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "cross-site request rejected")
		return
	}
	if r.Header.Get("X-Orbit-Request") != "1" {
		writeAPIError(w, http.StatusForbidden, "ERR_MISSING_REQUEST_HEADER", "pairing claim requires X-Orbit-Request: 1 header")
		return
	}
	// If Tailscale Serve attached a user identity AND Hub has an explicit allowlist, verify it.
	if tsLogin := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); tsLogin != "" && s.hasConfiguredTailscaleLogins() {
		if !s.isAllowedTailscaleLogin(tsLogin) {
			writeAPIError(w, http.StatusForbidden, "ERR_FORBIDDEN", "tailscale login is not allowed")
			return
		}
	}

	now := time.Now()
	if blocked, circuitOpen := s.pairLimiter.checkAndReserve(now); blocked {
		if circuitOpen {
			writeAPIError(w, http.StatusTooManyRequests, "ERR_PAIRING_LOCKED", "pairing temporarily locked due to repeated invalid attempts")
		} else {
			writeAPIError(w, http.StatusTooManyRequests, "ERR_RATE_LIMITED", "too many pairing attempts; try again shortly")
		}
		return
	}

	var req protocol.ClaimPairingRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	if !req.WantRunner && !req.WantClient {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", "must request runner or client credentials")
		return
	}

	normalized := normalizePairingCode(req.Code)
	if normalized == "" {
		if s.pairLimiter.recordFailure(now) {
			s.emitPairingLockedEvent(r.Context())
		}
		writeAPIError(w, http.StatusUnauthorized, "ERR_PAIRING_INVALID", "配对码无效或已过期")
		return
	}

	effYAML := s.effectiveYAMLMachines()
	yamlIDs := make(map[string]bool, len(effYAML))
	for id := range effYAML {
		yamlIDs[id] = true
	}

	res, err := s.store.ClaimPairingCode(r.Context(), store.ClaimPairingCodeParams{
		CodeHash:       store.HashSecret(normalized),
		WantRunner:     req.WantRunner,
		WantClient:     req.WantClient,
		Machine:        req.Machine,
		YAMLMachineIDs: yamlIDs,
		NowMs:          now.UnixMilli(),
	})
	if errors.Is(err, store.ErrPairingInvalid) {
		if s.pairLimiter.recordFailure(now) {
			s.emitPairingLockedEvent(r.Context())
		}
		writeAPIError(w, http.StatusUnauthorized, "ERR_PAIRING_INVALID", "配对码无效或已过期")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PAIRING_CLAIM", err.Error())
		return
	}

	// Refresh in-memory effective registry immediately so /ws/runner accepts the new machine without restart!
	if err := s.refreshEffectiveRegistry(context.WithoutCancel(r.Context())); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_REGISTRY_REFRESH", err.Error())
		return
	}
	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
	}

	origin := s.effectiveHubOrigin()
	writeJSON(w, http.StatusOK, protocol.ClaimPairingResponse{
		MachineID:     res.MachineID,
		DisplayName:   res.DisplayName,
		WorkspaceName: s.effectiveWorkspaceName(),
		HubOrigin:     origin,
		WSURL:         hubOriginToRunnerWS(origin),
		RunnerToken:   res.RunnerToken,
		ClientToken:   res.ClientToken,
	})
}

func (s *Server) emitPairingLockedEvent(ctx context.Context) {
	// Best-effort event notification for UI awareness
	_ = s.store.SetHubSetting(ctx, "pairing_last_lockout_at", map[string]any{"at": time.Now().UnixMilli()})
	s.broker.Notify()
}

// handleCreateAuthHandoff issues a 30-second single-use handoff code for Desktop / CLI browser handoff.
func (s *Server) handleCreateAuthHandoff(w http.ResponseWriter, r *http.Request) {
	actor, kind, machineID, _, ok := s.authenticate(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "authentication required")
		return
	}
	sourceTokenHash := ""
	if kind == authClientToken {
		if bearer := extractBearerToken(r.Header.Get("Authorization")); bearer != "" {
			sourceTokenHash = store.HashSecret(bearer)
		}
	}
	// If the caller didn't come via a machine-bound client token, infer local_ws machineId for local Desktop handoff.
	if machineID == "" {
		s.mu.RLock()
		for id, m := range s.machines.Machines {
			if m.Transport == protocol.TransportLocalWS {
				machineID = id
				break
			}
		}
		s.mu.RUnlock()
	}

	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HANDOFF_GEN", err.Error())
		return
	}
	code := "ahc_" + fmt.Sprintf("%x", raw[:])
	now := time.Now()
	exp := now.Add(30 * time.Second)

	s.handoffMu.Lock()
	if s.handoffCodes == nil {
		s.handoffCodes = map[string]authHandoffEntry{}
	}
	for k, v := range s.handoffCodes {
		if now.After(v.expiresAt) {
			delete(s.handoffCodes, k)
		}
	}
	s.handoffCodes[code] = authHandoffEntry{
		actor:           actor,
		machineID:       machineID,
		sourceTokenHash: sourceTokenHash,
		expiresAt:       exp,
	}
	s.handoffMu.Unlock()

	writeJSON(w, http.StatusOK, protocol.AuthHandoffResponse{
		Code:      code,
		ExpiresAt: exp.UnixMilli(),
	})
}

// handleClaimAuthHandoff exchanges a 30-second single-use handoff code for a browser client token (ock_).
func (s *Server) handleClaimAuthHandoff(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "cross-site request rejected")
		return
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !s.isAllowedOrigin(origin, r) {
		writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "origin not allowed")
		return
	}
	if r.Header.Get("X-Orbit-Request") != "1" {
		writeAPIError(w, http.StatusForbidden, "ERR_MISSING_REQUEST_HEADER", "handoff claim requires X-Orbit-Request: 1")
		return
	}

	var req protocol.ClaimAuthHandoffRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeAPIError(w, http.StatusUnauthorized, "ERR_HANDOFF_INVALID", "invalid or expired auth code")
		return
	}

	now := time.Now()
	s.handoffMu.Lock()
	entry, ok := s.handoffCodes[code]
	if ok {
		delete(s.handoffCodes, code)
	}
	s.handoffMu.Unlock()

	if !ok || now.After(entry.expiresAt) {
		writeAPIError(w, http.StatusUnauthorized, "ERR_HANDOFF_INVALID", "invalid or expired auth code")
		return
	}

	rawToken, _, err := s.store.CreateClientTokenChecked(r.Context(), "desktop-browser", "browser", entry.machineID, entry.sourceTokenHash)
	if errors.Is(err, store.ErrForbidden) {
		writeAPIError(w, http.StatusUnauthorized, "ERR_HANDOFF_REVOKED", "source credential or machine is no longer active")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TOKEN_CREATE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.ClaimAuthHandoffResponse{
		ClientToken: rawToken,
		MachineID:   entry.machineID,
	})
}

// handlePairClient redeems a one-time pairing code from a same-origin browser/client into a persistent
// client-only device login session (ock_ token). It NEVER registers a runner/host machine.
func (s *Server) handlePairClient(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "cross-site request rejected")
		return
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if !s.isAllowedOrigin(origin, r) {
			// Refresh live Tailscale Serve origin once if a fresh hub hasn't cached it yet.
			_, tsShared, tsOrigin, _, _ := s.probeTailscaleState(r.Context())
			if tsShared && tsOrigin != "" {
				s.recordTailscaleSharing(context.WithoutCancel(r.Context()), tsOrigin, "system")
			}
			if !s.isAllowedOrigin(origin, r) {
				writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "origin not allowed")
				return
			}
		}
	}
	if r.Header.Get("X-Orbit-Request") != "1" {
		writeAPIError(w, http.StatusForbidden, "ERR_MISSING_REQUEST_HEADER", "client pairing requires X-Orbit-Request: 1 header")
		return
	}
	if tsLogin := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); tsLogin != "" && s.hasConfiguredTailscaleLogins() {
		if !s.isAllowedTailscaleLogin(tsLogin) {
			writeAPIError(w, http.StatusForbidden, "ERR_FORBIDDEN", "tailscale login is not allowed")
			return
		}
	}

	now := time.Now()
	if blocked, circuitOpen := s.pairLimiter.checkAndReserve(now); blocked {
		if circuitOpen {
			writeAPIError(w, http.StatusTooManyRequests, "ERR_PAIRING_LOCKED", "pairing temporarily locked due to repeated invalid attempts")
		} else {
			writeAPIError(w, http.StatusTooManyRequests, "ERR_RATE_LIMITED", "too many pairing attempts; try again shortly")
		}
		return
	}

	var req protocol.PairClientRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	normalized := normalizePairingCode(req.Code)
	if normalized == "" {
		if s.pairLimiter.recordFailure(now) {
			s.emitPairingLockedEvent(r.Context())
		}
		writeAPIError(w, http.StatusUnauthorized, "ERR_PAIRING_INVALID", "配对码无效或已过期")
		return
	}

	deviceName := strings.TrimSpace(req.DeviceName)
	if deviceName == "" {
		deviceName = inferClientDeviceName(r.UserAgent())
	}
	if len(deviceName) > 64 {
		deviceName = deviceName[:64]
	}

	res, err := s.store.ClaimPairingCode(r.Context(), store.ClaimPairingCodeParams{
		CodeHash:    store.HashSecret(normalized),
		WantRunner:  false,
		WantClient:  true,
		ClientName:  deviceName,
		ClientScope: "browser",
		NowMs:       now.UnixMilli(),
	})
	if errors.Is(err, store.ErrPairingInvalid) {
		if s.pairLimiter.recordFailure(now) {
			s.emitPairingLockedEvent(r.Context())
		}
		writeAPIError(w, http.StatusUnauthorized, "ERR_PAIRING_INVALID", "配对码无效或已过期")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PAIRING_CLAIM", err.Error())
		return
	}
	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
	}

	writeJSON(w, http.StatusOK, protocol.PairClientResponse{
		ClientToken:   res.ClientToken,
		TokenID:       res.ClientTokenID,
		DeviceName:    res.ClientName,
		WorkspaceName: s.effectiveWorkspaceName(),
	})
}

func inferClientDeviceName(ua string) string {
	low := strings.ToLower(ua)
	browser := "浏览器"
	switch {
	case strings.Contains(low, "edg/"):
		browser = "Edge"
	case strings.Contains(low, "chrome/") && !strings.Contains(low, "chromium"):
		browser = "Chrome"
	case strings.Contains(low, "firefox/"):
		browser = "Firefox"
	case strings.Contains(low, "safari/") && !strings.Contains(low, "chrome/"):
		browser = "Safari"
	}
	osName := ""
	switch {
	case strings.Contains(low, "iphone"):
		osName = "iPhone"
	case strings.Contains(low, "ipad"):
		osName = "iPad"
	case strings.Contains(low, "android"):
		osName = "Android"
	case strings.Contains(low, "mac os x") || strings.Contains(low, "macintosh"):
		osName = "macOS"
	case strings.Contains(low, "windows"):
		osName = "Windows"
	case strings.Contains(low, "linux"):
		osName = "Linux"
	}
	if osName != "" {
		return browser + " · " + osName
	}
	return browser + "客户端"
}

func (s *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.store.ListActiveClientTokens(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LIST_CLIENTS", err.Error())
		return
	}
	curID := clientTokenIDFromContext(r.Context())
	for i := range clients {
		if curID != "" && clients[i].ID == curID {
			clients[i].IsCurrent = true
		}
	}
	writeJSON(w, http.StatusOK, protocol.ListClientsResponse{Clients: clients})
}

func (s *Server) handleRevokeClient(w http.ResponseWriter, r *http.Request) {
	if actorKindFromContext(r.Context()) != protocol.ActorHuman {
		writeAPIError(w, http.StatusForbidden, "ERR_FORBIDDEN", "only a human operator can revoke client devices")
		return
	}
	clientID := chi.URLParam(r, "clientId")
	revoked, err := s.store.RevokeClientToken(r.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_CLIENT_NOT_FOUND", "client device not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_REVOKE_CLIENT", err.Error())
		return
	}
	s.revokeClientTokenSessions(clientID)
	writeJSON(w, http.StatusOK, protocol.RevokeClientResponse{
		ClientID: clientID,
		Revoked:  revoked,
	})
}

func (s *Server) handleLogoutClient(w http.ResponseWriter, r *http.Request) {
	tokenID := clientTokenIDFromContext(r.Context())
	if tokenID != "" {
		_, _ = s.store.RevokeClientToken(r.Context(), tokenID)
		s.revokeClientTokenSessions(tokenID)
	}
	writeJSON(w, http.StatusOK, protocol.LogoutClientResponse{LoggedOut: true})
}
