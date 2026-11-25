package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// DynamicNetworkSettings persists Hub-managed sharing and display settings in hub_settings
// without modifying hub.yaml.
type DynamicNetworkSettings struct {
	WorkspaceName          string   `json:"workspaceName,omitempty"`
	DiscoveryID            string   `json:"discoveryId,omitempty"`
	DutyPriority           []string `json:"dutyPriority,omitempty"`
	ExternalGatewayEnabled *bool    `json:"externalGatewayEnabled,omitempty"`
	ExtraOrigins           []string `json:"extraOrigins,omitempty"`
	ExtraLogins            []string `json:"extraLogins,omitempty"`
	TailscaleOrigin        string   `json:"tailscaleOrigin,omitempty"`
	TailscaleShared        bool     `json:"tailscaleShared,omitempty"`
}

// ClientTokenInfo identifies an authenticated client token record.
type ClientTokenInfo struct {
	ID        string
	Name      string
	Scope     string
	MachineID string
}

// PairingCodeRecord represents one row in pairing_codes.
type PairingCodeRecord struct {
	ID               string
	Scope            protocol.PairingScope
	Role             protocol.MachineRole
	BindMachineID    string
	CreatedBy        string
	CreatedAt        int64
	ExpiresAt        int64
	State            protocol.PairingCodeState
	ClaimedAt        *int64
	ClaimedMachineID string
}

// CreatePairingCodeParams inserts a new hashed pairing code.
type CreatePairingCodeParams struct {
	CodeHash      string
	Scope         protocol.PairingScope
	Role          protocol.MachineRole
	BindMachineID string
	CreatedBy     string
	TTLSeconds    int
	MaxActive     int
}

// ClaimPairingCodeParams atomically claims a pairing code and issues durable credentials.
type ClaimPairingCodeParams struct {
	CodeHash       string
	WantRunner     bool
	WantClient     bool
	Machine        *protocol.ClaimMachineInfo
	YAMLMachineIDs map[string]bool
	NowMs          int64
}

// ClaimPairingCodeResult returns the claimed pairing record, allocated machineId, minted tokens, and events.
type ClaimPairingCodeResult struct {
	CodeID      string
	MachineID   string
	DisplayName string
	RunnerToken string
	ClientToken string
	Events      []protocol.EventRecord
}

// ErrPairingInvalid is returned when a pairing code is unknown, already claimed, revoked, or expired.
var ErrPairingInvalid = errors.New("pairing code is invalid or expired")

// ErrTooManyActiveCodes is returned when the active pairing code cap is reached.
var ErrTooManyActiveCodes = errors.New("too many active pairing codes")

// HashSecret returns lowercase hex SHA-256 of raw secret bytes.
func HashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomSecretHex(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GetHubSetting reads a raw JSON setting value by key.
func (s *Store) GetHubSetting(ctx context.Context, key string) (string, bool, error) {
	var raw string
	err := s.ro.QueryRowContext(ctx, `SELECT value_json FROM hub_settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return raw, true, nil
}

// SetHubSetting writes a JSON setting value by key.
func (s *Store) SetHubSetting(ctx context.Context, key string, val any) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO hub_settings (key, value_json, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
		`, key, string(raw), now)
		return err
	})
}

// IsOnboardingDone reports whether first-run onboarding has been completed or auto-migrated.
func (s *Store) IsOnboardingDone(ctx context.Context) (bool, error) {
	raw, ok, err := s.GetHubSetting(ctx, "onboarding_done")
	if err != nil || !ok {
		return false, err
	}
	var doc struct {
		Done bool `json:"done"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return false, nil
	}
	return doc.Done, nil
}

// MarkOnboardingDone persists onboarding completion in hub_settings.
func (s *Store) MarkOnboardingDone(ctx context.Context, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "completed"
	}
	return s.SetHubSetting(ctx, "onboarding_done", map[string]any{
		"done":   true,
		"reason": reason,
		"at":     time.Now().UnixMilli(),
	})
}

// GetDynamicNetworkSettings returns Hub-managed dynamic network settings stored in SQLite.
func (s *Store) GetDynamicNetworkSettings(ctx context.Context) (DynamicNetworkSettings, error) {
	var out DynamicNetworkSettings
	raw, ok, err := s.GetHubSetting(ctx, "network_settings")
	if err != nil || !ok {
		return out, err
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return DynamicNetworkSettings{}, err
	}
	return out, nil
}

// SaveDynamicNetworkSettings persists updated dynamic network settings and emits an event.
func (s *Store) SaveDynamicNetworkSettings(ctx context.Context, settings DynamicNetworkSettings, actorID string) (protocol.EventRecord, error) {
	raw, err := json.Marshal(settings)
	if err != nil {
		return protocol.EventRecord{}, err
	}
	now := time.Now().UnixMilli()
	var ev protocol.EventRecord
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO hub_settings (key, value_json, updated_at)
			VALUES ('network_settings', ?, ?)
			ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
		`, string(raw), now); err != nil {
			return err
		}
		var err2 error
		ev, err2 = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventNetworkSettingsChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"workspaceName":   settings.WorkspaceName,
				"tailscaleShared": settings.TailscaleShared,
				"tailscaleOrigin": settings.TailscaleOrigin,
			},
			CreatedAt: now,
		})
		return err2
	})
	return ev, err
}

// CreateClientToken mints and persists a new ock_ client bearer token.
func (s *Store) CreateClientToken(ctx context.Context, name, scope, machineID string) (string, string, error) {
	if strings.TrimSpace(scope) == "" {
		scope = "client"
	}
	if strings.TrimSpace(name) == "" {
		name = "orbit-client"
	}
	secret, err := randomSecretHex(32)
	if err != nil {
		return "", "", err
	}
	rawToken := "ock_" + secret
	tokenHash := HashSecret(rawToken)
	tokenID := newID("ctk")
	now := time.Now().UnixMilli()

	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO client_tokens (id, name, token_hash, scope, machine_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, tokenID, name, tokenHash, scope, strings.TrimSpace(machineID), now)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return rawToken, tokenID, nil
}

// VerifyClientToken verifies an ock_ bearer token against client_tokens.
func (s *Store) VerifyClientToken(ctx context.Context, rawToken string) (ClientTokenInfo, bool, error) {
	var info ClientTokenInfo
	if !strings.HasPrefix(rawToken, "ock_") || len(rawToken) < 20 {
		return info, false, nil
	}
	tokenHash := HashSecret(rawToken)
	var revoked sql.NullInt64
	err := s.ro.QueryRowContext(ctx, `
		SELECT id, name, scope, machine_id, revoked_at
		FROM client_tokens
		WHERE token_hash = ?
	`, tokenHash).Scan(&info.ID, &info.Name, &info.Scope, &info.MachineID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return info, false, nil
	}
	if err != nil {
		return info, false, err
	}
	if revoked.Valid {
		return info, false, nil
	}
	return info, true, nil
}

// VerifyMachineCredential checks whether rawToken matches an active DB credential for machineID.
func (s *Store) VerifyMachineCredential(ctx context.Context, machineID, rawToken string) (bool, error) {
	if strings.TrimSpace(machineID) == "" || strings.TrimSpace(rawToken) == "" {
		return false, nil
	}
	tokenHash := HashSecret(rawToken)
	var id string
	err := s.ro.QueryRowContext(ctx, `
		SELECT id FROM machine_credentials
		WHERE machine_id = ? AND token_hash = ? AND revoked_at IS NULL
		LIMIT 1
	`, machineID, tokenHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// MachinesWithDBCredentials returns the set of machine IDs that have at least one active DB credential.
func (s *Store) MachinesWithDBCredentials(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT DISTINCT machine_id FROM machine_credentials WHERE revoked_at IS NULL
	`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			return out, err
		}
		out[mid] = true
	}
	return out, rows.Err()
}

// CreatePairingCode creates a new single-use hashed pairing code, enforcing maxActive active codes.
func (s *Store) CreatePairingCode(ctx context.Context, p CreatePairingCodeParams) (PairingCodeRecord, error) {
	var rec PairingCodeRecord
	if !p.Scope.IsValid() {
		return rec, fmt.Errorf("%w: invalid pairing scope %q", ErrInvalidInput, p.Scope)
	}
	ttl := p.TTLSeconds
	if ttl <= 0 {
		ttl = 600
	}
	if ttl > 3600 {
		ttl = 3600
	}
	maxActive := p.MaxActive
	if maxActive <= 0 {
		maxActive = 5
	}
	role := p.Role
	if role == "" {
		role = protocol.MachineRoleFull
	}
	if !role.IsValid() {
		return rec, fmt.Errorf("%w: invalid machine role %q", ErrInvalidInput, role)
	}
	now := time.Now().UnixMilli()
	exp := now + int64(ttl)*1000
	id := newID("pair")

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// Mark naturally expired pending codes before counting active codes.
		if _, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes SET state = 'expired'
			WHERE state = 'pending' AND expires_at <= ?
		`, now); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pairing_codes WHERE state = 'pending' AND expires_at > ?
		`, now).Scan(&active); err != nil {
			return err
		}
		if active >= maxActive {
			return ErrTooManyActiveCodes
		}
		if bindID := strings.TrimSpace(p.BindMachineID); bindID != "" {
			var exists string
			if err := tx.QueryRowContext(ctx, `SELECT machine_id FROM machines WHERE machine_id = ?`, bindID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: bound machine %s not found", ErrNotFound, bindID)
			} else if err != nil {
				return err
			}
			var removed sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT removed_at FROM registry_machines WHERE machine_id = ?`, bindID).Scan(&removed)
			if err == nil && removed.Valid {
				return fmt.Errorf("%w: bound machine %s was removed", ErrNotFound, bindID)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pairing_codes (id, code_hash, scope, role, bind_machine_id, created_by, created_at, expires_at, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending')
		`, id, p.CodeHash, string(p.Scope), string(role), strings.TrimSpace(p.BindMachineID), p.CreatedBy, now, exp)
		return err
	})
	if err != nil {
		return rec, err
	}
	rec = PairingCodeRecord{
		ID:            id,
		Scope:         p.Scope,
		Role:          role,
		BindMachineID: strings.TrimSpace(p.BindMachineID),
		CreatedBy:     p.CreatedBy,
		CreatedAt:     now,
		ExpiresAt:     exp,
		State:         protocol.PairingCodePending,
	}
	return rec, nil
}

// GetPairingCode fetches a pairing code record by ID, mapping expired pending rows to expired.
func (s *Store) GetPairingCode(ctx context.Context, id string, nowMs int64) (PairingCodeRecord, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	var rec PairingCodeRecord
	var scope, roleStr, state string
	var claimedAt sql.NullInt64
	err := s.ro.QueryRowContext(ctx, `
		SELECT id, scope, role, bind_machine_id, created_by, created_at, expires_at, state, claimed_at, claimed_machine_id
		FROM pairing_codes WHERE id = ?
	`, id).Scan(
		&rec.ID, &scope, &roleStr, &rec.BindMachineID, &rec.CreatedBy,
		&rec.CreatedAt, &rec.ExpiresAt, &state, &claimedAt, &rec.ClaimedMachineID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	if err != nil {
		return rec, err
	}
	rec.Scope = protocol.PairingScope(scope)
	if roleStr != "" {
		rec.Role = protocol.MachineRole(roleStr)
	} else {
		rec.Role = protocol.MachineRoleFull
	}
	rec.State = protocol.PairingCodeState(state)
	if claimedAt.Valid {
		v := claimedAt.Int64
		rec.ClaimedAt = &v
	}
	if rec.State == protocol.PairingCodePending && rec.ExpiresAt <= nowMs {
		rec.State = protocol.PairingCodeExpired
	}
	return rec, nil
}

// RevokePairingCode transitions a pending pairing code to revoked.
func (s *Store) RevokePairingCode(ctx context.Context, id, actorID string) (PairingCodeRecord, *protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	var ev *protocol.EventRecord
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes SET state = 'revoked'
			WHERE id = ? AND state = 'pending'
		`, id)
		if err != nil {
			return err
		}
		aff, _ := res.RowsAffected()
		if aff == 0 {
			var exists string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM pairing_codes WHERE id = ?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return nil
		}
		e, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventPairingUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"pairingId": id,
				"state":     protocol.PairingCodeRevoked,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		ev = &e
		return nil
	})
	if err != nil {
		return PairingCodeRecord{}, nil, err
	}
	rec, err := s.GetPairingCode(ctx, id, now)
	return rec, ev, err
}

// ClaimPairingCode atomically consumes a single-use pairing code and mints runner/client tokens.
func (s *Store) ClaimPairingCode(ctx context.Context, p ClaimPairingCodeParams) (ClaimPairingCodeResult, error) {
	var out ClaimPairingCodeResult
	now := p.NowMs
	if now <= 0 {
		now = time.Now().UnixMilli()
	}
	if strings.TrimSpace(p.CodeHash) == "" {
		return out, ErrPairingInvalid
	}
	if !p.WantRunner && !p.WantClient {
		return out, fmt.Errorf("%w: must request runner or client credentials", ErrInvalidInput)
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// 1. Read row first to validate scope before atomic consume
		var id, scopeStr, roleStr, bindMachineID, stateStr string
		var expiresAt int64
		err := tx.QueryRowContext(ctx, `
			SELECT id, scope, role, bind_machine_id, state, expires_at
			FROM pairing_codes WHERE code_hash = ?
		`, p.CodeHash).Scan(&id, &scopeStr, &roleStr, &bindMachineID, &stateStr, &expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPairingInvalid
		}
		if err != nil {
			return err
		}
		machineRole := protocol.MachineRole(roleStr)
		if !machineRole.IsValid() {
			machineRole = protocol.MachineRoleFull
		}
		scope := protocol.PairingScope(scopeStr)
		if stateStr != string(protocol.PairingCodePending) || expiresAt <= now {
			return ErrPairingInvalid
		}
		// Enforce scope boundaries strictly
		if p.WantRunner && scope == protocol.PairingScopeClient {
			return ErrPairingInvalid
		}
		if p.WantClient && scope == protocol.PairingScopeRunner {
			return ErrPairingInvalid
		}

		// 2. Atomic single-use claim guarded by state='pending' AND expires_at > now
		updRes, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes
			SET state = 'claimed', claimed_at = ?
			WHERE id = ? AND code_hash = ? AND state = 'pending' AND expires_at > ?
		`, now, id, p.CodeHash, now)
		if err != nil {
			return err
		}
		if aff, _ := updRes.RowsAffected(); aff != 1 {
			return ErrPairingInvalid
		}
		out.CodeID = id

		// 3. Allocate or bind machineId when runner credential is requested
		machineID := strings.TrimSpace(bindMachineID)
		displayName := ""
		if p.Machine != nil {
			displayName = strings.TrimSpace(p.Machine.DisplayName)
		}

		if p.WantRunner {
			if machineID != "" {
				// Bind to existing active machine (YAML or DB) without modifying YAML or resurrecting removed nodes
				var exists string
				err := tx.QueryRowContext(ctx, `SELECT machine_id FROM machines WHERE machine_id = ?`, machineID).Scan(&exists)
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: bound machine %s not found", ErrNotFound, machineID)
				}
				if err != nil {
					return err
				}
				var removed sql.NullInt64
				rErr := tx.QueryRowContext(ctx, `SELECT removed_at FROM registry_machines WHERE machine_id = ?`, machineID).Scan(&removed)
				if rErr == nil && removed.Valid {
					return ErrPairingInvalid
				}
				if rErr != nil && !errors.Is(rErr, sql.ErrNoRows) {
					return rErr
				}
				if displayName == "" {
					displayName = machineID
				}
			} else {
				baseID := ""
				if p.Machine != nil {
					baseID = sanitizeMachineID(p.Machine.ProposedID)
					if baseID == "" {
						baseID = sanitizeMachineID(strings.Split(p.Machine.Hostname, ".")[0])
					}
				}
				if baseID == "" {
					baseID = "node"
				}
				allocated, err := allocateUniqueMachineIDTx(ctx, tx, baseID, p.YAMLMachineIDs)
				if err != nil {
					return err
				}
				machineID = allocated
				if displayName == "" {
					displayName = machineID
				}

				caps := deriveMachineCapabilities(p.Machine)
				capsJSON, _ := json.Marshal(caps)
				adps := []protocol.Adapter{}
				if p.Machine != nil {
					for _, a := range p.Machine.Adapters {
						if protocol.IsSessionAdapterImplemented(a) {
							adps = append(adps, a)
						}
					}
				}
				adpsJSON, _ := json.Marshal(adps)

				osName := ""
				if p.Machine != nil {
					osName = strings.TrimSpace(p.Machine.OS)
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO registry_machines (machine_id, display_name, transport_mode, role, capabilities_json, tailscale_host, ssh_alias, created_at)
					VALUES (?, ?, 'remote_ws', ?, ?, '', '', ?)
				`, machineID, displayName, string(machineRole), string(capsJSON), now); err != nil {
					return fmt.Errorf("insert registry machine: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO machines (machine_id, transport_mode, status, role, os_name, capabilities_json, adapters_json, resources_json, runner_version, last_seen_at, updated_at)
					VALUES (?, 'remote_ws', 'offline', ?, ?, ?, ?, '{}', ?, ?, ?)
					ON CONFLICT(machine_id) DO UPDATE SET
						transport_mode = 'remote_ws',
						role = excluded.role,
						os_name = CASE WHEN excluded.os_name != '' THEN excluded.os_name ELSE machines.os_name END,
						capabilities_json = excluded.capabilities_json,
						updated_at = excluded.updated_at
				`, machineID, string(machineRole), osName, string(capsJSON), string(adpsJSON), runnerVersionOrNil(p.Machine), now, now); err != nil {
					return fmt.Errorf("upsert paired machine: %w", err)
				}
			}

			runnerSecret, err := randomSecretHex(32)
			if err != nil {
				return err
			}
			out.RunnerToken = "ort_" + runnerSecret
			credID := newID("mcr")
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO machine_credentials (id, machine_id, token_hash, created_at)
				VALUES (?, ?, ?, ?)
			`, credID, machineID, HashSecret(out.RunnerToken), now); err != nil {
				return fmt.Errorf("insert machine credential: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				UPDATE pairing_codes SET claimed_machine_id = ? WHERE id = ?
			`, machineID, id); err != nil {
				return err
			}
			out.MachineID = machineID
			out.DisplayName = displayName
		}

		// 4. Mint client token if requested
		if p.WantClient {
			clientSecret, err := randomSecretHex(32)
			if err != nil {
				return err
			}
			out.ClientToken = "ock_" + clientSecret
			clientName := displayName
			if clientName == "" && p.Machine != nil {
				clientName = strings.TrimSpace(p.Machine.Hostname)
			}
			if clientName == "" {
				clientName = "paired-client"
			}
			ctkID := newID("ctk")
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO client_tokens (id, name, token_hash, scope, machine_id, created_at)
				VALUES (?, ?, ?, 'client', ?, ?)
			`, ctkID, clientName, HashSecret(out.ClientToken), out.MachineID, now); err != nil {
				return fmt.Errorf("insert client token: %w", err)
			}
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorSystem,
			ActorID:    "pairing",
			Kind:       protocol.EventPairingUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"pairingId": id,
				"state":     protocol.PairingCodeClaimed,
				"machineId": out.MachineID,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		out.Events = append(out.Events, ev)
		if out.MachineID != "" {
			mev, err := insertEventTx(ctx, tx, eventInsert{
				StreamID:   "global",
				ActorKind:  protocol.ActorSystem,
				ActorID:    "pairing",
				Kind:       protocol.EventMachineStateChanged,
				Visibility: protocol.VisibilityTimeline,
				Payload: map[string]any{
					"machineId": out.MachineID,
					"paired":    true,
				},
				CreatedAt: now,
			})
			if err != nil {
				return err
			}
			out.Events = append(out.Events, mev)
		}
		return nil
	})
	return out, err
}

func allocateUniqueMachineIDTx(ctx context.Context, tx *sql.Tx, base string, yamlIDs map[string]bool) (string, error) {
	isTaken := func(cand string) (bool, error) {
		if yamlIDs != nil && yamlIDs[cand] {
			return true, nil
		}
		var x string
		err := tx.QueryRowContext(ctx, `SELECT machine_id FROM machines WHERE machine_id = ?`, cand).Scan(&x)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		err = tx.QueryRowContext(ctx, `SELECT machine_id FROM registry_machines WHERE machine_id = ? AND removed_at IS NULL`, cand).Scan(&x)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	taken, err := isTaken(base)
	if err != nil {
		return "", err
	}
	if !taken {
		return base, nil
	}
	for i := 2; i <= 99; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		taken, err := isTaken(cand)
		if err != nil {
			return "", err
		}
		if !taken {
			return cand, nil
		}
	}
	return fmt.Sprintf("%s-%s", base, newID("n")[2:8]), nil
}

func sanitizeMachineID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

func deriveMachineCapabilities(m *protocol.ClaimMachineInfo) []string {
	if m != nil && len(m.Capabilities) > 0 {
		return m.Capabilities
	}
	if m != nil && strings.EqualFold(m.OS, "darwin") {
		return []string{"macos-env", "primary-dev"}
	}
	arch := "amd64"
	if m != nil && strings.TrimSpace(m.Arch) != "" {
		arch = strings.TrimSpace(m.Arch)
	}
	return []string{"primary-dev", "linux-" + arch}
}

func runnerVersionOrNil(m *protocol.ClaimMachineInfo) any {
	if m == nil || strings.TrimSpace(m.RunnerVersion) == "" {
		return nil
	}
	return strings.TrimSpace(m.RunnerVersion)
}

// ListRegistryMachines returns all active (non-removed) dynamically registered machines.
func (s *Store) ListRegistryMachines(ctx context.Context) (map[string]protocol.MachineConfig, error) {
	out := map[string]protocol.MachineConfig{}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT machine_id, display_name, transport_mode, capabilities_json, tailscale_host, ssh_alias
		FROM registry_machines
		WHERE removed_at IS NULL
	`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, displayName, transport, capsJSON, tsHost, sshAlias string
		if err := rows.Scan(&id, &displayName, &transport, &capsJSON, &tsHost, &sshAlias); err != nil {
			return out, err
		}
		var caps []string
		_ = json.Unmarshal([]byte(capsJSON), &caps)
		out[id] = protocol.MachineConfig{
			DisplayName:   displayName,
			Transport:     protocol.TransportMode(transport),
			TailscaleHost: tsHost,
			SSHAlias:      sshAlias,
			Capabilities:  caps,
		}
	}
	return out, rows.Err()
}

// ListRemovedRegistryMachineIDs returns the set of soft-removed dynamic machine IDs.
func (s *Store) ListRemovedRegistryMachineIDs(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := s.ro.QueryContext(ctx, `SELECT machine_id FROM registry_machines WHERE removed_at IS NOT NULL`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// UpdateMachineAlias updates display_alias for a machine and emits machine.state_changed.
func (s *Store) UpdateMachineAlias(ctx context.Context, machineID, alias, actorID string) (protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	alias = strings.TrimSpace(alias)
	var ev protocol.EventRecord
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE machines SET display_alias = ?, updated_at = ? WHERE machine_id = ?
		`, alias, now, machineID)
		if err != nil {
			return err
		}
		if aff, _ := res.RowsAffected(); aff == 0 {
			return fmt.Errorf("%w: machine %s", ErrNotFound, machineID)
		}
		var err2 error
		ev, err2 = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventMachineStateChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"machineId":    machineID,
				"displayAlias": alias,
			},
			CreatedAt: now,
		})
		return err2
	})
	return ev, err
}

// RemovePairedMachine revokes credentials and soft-removes a paired machine (YAML machines must be checked by caller).
func (s *Store) RemovePairedMachine(ctx context.Context, machineID, actorID string) (protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	var ev protocol.EventRecord
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE registry_machines SET removed_at = ?
			WHERE machine_id = ? AND removed_at IS NULL
		`, now, machineID)
		if err != nil {
			return err
		}
		if aff, _ := res.RowsAffected(); aff == 0 {
			return fmt.Errorf("%w: paired machine %s", ErrNotFound, machineID)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes SET state = 'revoked' WHERE bind_machine_id = ? AND state = 'pending'
		`, machineID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE machine_credentials SET revoked_at = ? WHERE machine_id = ? AND revoked_at IS NULL
		`, now, machineID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE client_tokens SET revoked_at = ? WHERE machine_id = ? AND revoked_at IS NULL
		`, now, machineID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE machines SET status = 'offline', updated_at = ? WHERE machine_id = ?
		`, now, machineID); err != nil {
			return err
		}
		var err2 error
		ev, err2 = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventMachineStateChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"machineId": machineID,
				"removed":   true,
			},
			CreatedAt: now,
		})
		return err2
	})
	return ev, err
}

// SaveMachineAgentProbes stores the latest AgentProbes reported by a Runner and updates ready adapters.
func (s *Store) SaveMachineAgentProbes(ctx context.Context, machineID string, probes []protocol.AgentProbe, runnerCaps []string) (*protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	if probes == nil {
		probes = []protocol.AgentProbe{}
	}
	probesJSON, err := json.Marshal(probes)
	if err != nil {
		return nil, err
	}
	readyAdapters := deriveReadyAdaptersFromProbes(probes)
	adaptersJSON, err := json.Marshal(readyAdapters)
	if err != nil {
		return nil, err
	}
	var capsVal any
	if len(runnerCaps) > 0 {
		if b, err := json.Marshal(runnerCaps); err == nil {
			capsVal = string(b)
		}
	}

	var emitted *protocol.EventRecord
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE machines
			SET agents_json = ?,
			    adapters_json = ?,
			    runner_caps_json = COALESCE(?, runner_caps_json),
			    updated_at = ?
			WHERE machine_id = ?
		`, string(probesJSON), string(adaptersJSON), capsVal, now, machineID)
		if err != nil {
			return err
		}
		if aff, _ := res.RowsAffected(); aff == 0 {
			return fmt.Errorf("%w: machine %s", ErrNotFound, machineID)
		}
		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorRunner,
			ActorID:    "runner:" + machineID,
			Kind:       protocol.EventMachineAgentsUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"machineId": machineID,
				"ready":     readyAdapters,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev
		return nil
	})
	return emitted, err
}

func deriveReadyAdaptersFromProbes(probes []protocol.AgentProbe) []protocol.Adapter {
	out := []protocol.Adapter{}
	seen := map[protocol.Adapter]bool{}
	for _, p := range probes {
		if p.State == protocol.AgentStateReady && p.Enabled && protocol.IsSessionAdapterImplemented(p.Adapter) && !seen[p.Adapter] {
			seen[p.Adapter] = true
			out = append(out, p.Adapter)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// GetMachineAgentProbes returns the persisted AgentProbe slice for one machine.
func (s *Store) GetMachineAgentProbes(ctx context.Context, machineID string) ([]protocol.AgentProbe, error) {
	var raw string
	err := s.ro.QueryRowContext(ctx, `SELECT agents_json FROM machines WHERE machine_id = ?`, machineID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var probes []protocol.AgentProbe
	_ = json.Unmarshal([]byte(raw), &probes)
	if probes == nil {
		probes = []protocol.AgentProbe{}
	}
	return probes, nil
}

// GetMachineAgentSettings loads Hub-managed sparse agent overrides for one machine.
func (s *Store) GetMachineAgentSettings(ctx context.Context, machineID string) (map[string]protocol.RunnerAgentConfig, int64, error) {
	out := map[string]protocol.RunnerAgentConfig{}
	var rev int64
	rows, err := s.ro.QueryContext(ctx, `
		SELECT adapter, enabled, bin, args_json, default_model, max_live_workers, updated_at
		FROM machine_agent_settings
		WHERE machine_id = ?
	`, machineID)
	if err != nil {
		return out, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var adapter string
		var enabled sql.NullInt64
		var bin, argsJSON, defModel sql.NullString
		var maxWorkers sql.NullInt64
		var updatedAt int64
		if err := rows.Scan(&adapter, &enabled, &bin, &argsJSON, &defModel, &maxWorkers, &updatedAt); err != nil {
			return out, 0, err
		}
		var cfg protocol.RunnerAgentConfig
		if enabled.Valid {
			v := enabled.Int64 != 0
			cfg.Enabled = &v
		}
		if bin.Valid {
			cfg.Bin = bin.String
		}
		if argsJSON.Valid && argsJSON.String != "" {
			_ = json.Unmarshal([]byte(argsJSON.String), &cfg.Args)
		}
		if defModel.Valid {
			cfg.DefaultModel = defModel.String
		}
		if maxWorkers.Valid {
			cfg.MaxLiveWorkers = int(maxWorkers.Int64)
		}
		if updatedAt > rev {
			rev = updatedAt
		}
		out[adapter] = cfg
	}
	return out, rev, rows.Err()
}

// UpsertMachineAgentSetting patches one adapter's sparse override row for machineID.
func (s *Store) UpsertMachineAgentSetting(
	ctx context.Context,
	machineID string,
	adapter protocol.Adapter,
	req protocol.UpdateMachineAgentRequest,
) (int64, error) {
	if !protocol.SessionAdapter(adapter).IsValid() {
		return 0, fmt.Errorf("%w: unsupported session adapter %q", ErrInvalidInput, adapter)
	}
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT machine_id FROM machines WHERE machine_id = ?`, machineID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: machine %s", ErrNotFound, machineID)
		} else if err != nil {
			return err
		}

		var curEnabled sql.NullInt64
		var curBin, curArgs, curModel sql.NullString
		var curMax sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT enabled, bin, args_json, default_model, max_live_workers
			FROM machine_agent_settings
			WHERE machine_id = ? AND adapter = ?
		`, machineID, string(adapter)).Scan(&curEnabled, &curBin, &curArgs, &curModel, &curMax)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if req.Enabled != nil {
			v := int64(0)
			if *req.Enabled {
				v = 1
			}
			curEnabled = sql.NullInt64{Int64: v, Valid: true}
		}
		if req.Bin != nil {
			b := strings.TrimSpace(*req.Bin)
			if b == "" {
				curBin = sql.NullString{Valid: false}
			} else {
				curBin = sql.NullString{String: b, Valid: true}
			}
		}
		if req.Args != nil {
			rawArgs, _ := json.Marshal(req.Args)
			curArgs = sql.NullString{String: string(rawArgs), Valid: true}
		}
		if req.DefaultModel != nil {
			m := strings.TrimSpace(*req.DefaultModel)
			if m == "" {
				curModel = sql.NullString{Valid: false}
			} else {
				curModel = sql.NullString{String: m, Valid: true}
			}
		}
		if req.MaxLiveWorkers != nil {
			if *req.MaxLiveWorkers <= 0 {
				curMax = sql.NullInt64{Valid: false}
			} else {
				curMax = sql.NullInt64{Int64: int64(*req.MaxLiveWorkers), Valid: true}
			}
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO machine_agent_settings (machine_id, adapter, enabled, bin, args_json, default_model, max_live_workers, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(machine_id, adapter) DO UPDATE SET
				enabled = excluded.enabled,
				bin = excluded.bin,
				args_json = excluded.args_json,
				default_model = excluded.default_model,
				max_live_workers = excluded.max_live_workers,
				updated_at = excluded.updated_at
		`, machineID, string(adapter), curEnabled, curBin, curArgs, curModel, curMax, now)
		return err
	})
	return now, err
}

// ListDynamicProjects returns all non-archived UI-created projects from SQLite.
func (s *Store) ListDynamicProjects(ctx context.Context) (map[string]protocol.ProjectConfig, error) {
	out := map[string]protocol.ProjectConfig{}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT project_id, config_json FROM projects
		WHERE source = 'ui' AND archived = 0 AND project_id != ?
	`, protocol.ScratchProjectID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, cfgJSON string
		if err := rows.Scan(&id, &cfgJSON); err != nil {
			return out, err
		}
		var cfg protocol.ProjectConfig
		if err := json.Unmarshal([]byte(cfgJSON), &cfg); err == nil {
			out[id] = cfg
		}
	}
	return out, rows.Err()
}

func sameLocalProjectAnchor(existingCfgJSON string, incoming protocol.ProjectConfig) bool {
	var oldCfg protocol.ProjectConfig
	if json.Unmarshal([]byte(existingCfgJSON), &oldCfg) != nil || len(oldCfg.Anchors) == 0 || len(incoming.Anchors) == 0 {
		return false
	}
	for _, inA := range incoming.Anchors {
		inMach := strings.TrimSpace(inA.Machine)
		inRoot := filepath.Clean(strings.TrimSpace(inA.RepoPath))
		if inMach == "" || inRoot == "" || inRoot == "." {
			continue
		}
		for _, oldA := range oldCfg.Anchors {
			if strings.TrimSpace(oldA.Machine) == inMach && filepath.Clean(strings.TrimSpace(oldA.RepoPath)) == inRoot {
				return true
			}
		}
	}
	return false
}

// CreateDynamicProject persists a UI/API-registered project in SQLite (never touching projects.yaml).
// Optional prepareMirror runs inside the transaction before commit so mirror init failures roll back atomically.
func (s *Store) CreateDynamicProject(
	ctx context.Context,
	projectID string,
	cfg protocol.ProjectConfig,
	mirrorPath string,
	actorID string,
	prepareMirror ...func(finalMirrorPath string) error,
) (protocol.ProjectView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	if strings.TrimSpace(projectID) == "" || projectID == protocol.ScratchProjectID {
		return pv, ev, fmt.Errorf("%w: invalid projectId", ErrInvalidInput)
	}
	if cfg.DefaultBranch == "" {
		cfg.DefaultBranch = "main"
	}
	if cfg.IDEScheme == "" {
		cfg.IDEScheme = protocol.IDECursor
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return pv, ev, err
	}

	finalProjectID := projectID
	finalMirrorPath := mirrorPath
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var existingSource, existingRepoIdentity, existingCfgJSON string
		var archived int
		err := tx.QueryRowContext(ctx, `SELECT source, repo_identity, config_json, archived FROM projects WHERE project_id = ?`, finalProjectID).Scan(&existingSource, &existingRepoIdentity, &existingCfgJSON, &archived)
		if err == nil {
			if archived == 0 {
				return fmt.Errorf("%w: project %s already exists", ErrConflict, finalProjectID)
			}
			// Archived ID: only allow restoring when the exact same UI-managed repo matches;
			// for local-only repos (no remote origin, "local:..."), require matching (machineId, repoPath).
			trimmedOldID := strings.TrimSpace(existingRepoIdentity)
			trimmedNewID := strings.TrimSpace(cfg.RepoIdentity)
			sameRepo := existingSource == string(protocol.ProjectSourceUI) &&
				trimmedOldID != "" &&
				strings.EqualFold(trimmedOldID, trimmedNewID)
			if sameRepo && strings.HasPrefix(strings.ToLower(trimmedOldID), "local:") {
				sameRepo = sameLocalProjectAnchor(existingCfgJSON, cfg)
			}
			if !sameRepo {
				allocated := ""
				for suffix := 2; suffix < 1000; suffix++ {
					cand := fmt.Sprintf("%s-%d", projectID, suffix)
					var dummy int
					cErr := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE project_id = ?`, cand).Scan(&dummy)
					if errors.Is(cErr, sql.ErrNoRows) {
						allocated = cand
						break
					}
					if cErr != nil {
						return cErr
					}
				}
				if allocated == "" {
					return fmt.Errorf("%w: unable to allocate unique projectId for %s", ErrConflict, projectID)
				}
				finalProjectID = allocated
				if finalMirrorPath != "" {
					finalMirrorPath = filepath.Join(filepath.Dir(finalMirrorPath), finalProjectID+".git")
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if len(prepareMirror) > 0 && prepareMirror[0] != nil {
			if err := prepareMirror[0](finalMirrorPath); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects (project_id, display_name, repo_identity, default_branch, mirror_bare_path, config_json, archived, source, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 0, 'ui', ?)
			ON CONFLICT(project_id) DO UPDATE SET
				display_name = excluded.display_name,
				repo_identity = excluded.repo_identity,
				default_branch = excluded.default_branch,
				mirror_bare_path = excluded.mirror_bare_path,
				config_json = excluded.config_json,
				archived = 0,
				source = 'ui',
				updated_at = excluded.updated_at
		`, finalProjectID, cfg.DisplayName, cfg.RepoIdentity, cfg.DefaultBranch, finalMirrorPath, string(cfgJSON), now); err != nil {
			return err
		}
		if err := ensureDefaultPartAndLocationsTx(ctx, tx, finalProjectID, cfg.DisplayName, cfg.RepoIdentity, string(cfgJSON), now); err != nil {
			return fmt.Errorf("init project parts: %w", err)
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  finalProjectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId": finalProjectID,
				"action":    "created",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, ev, err
	}
	pv, err = s.GetProject(ctx, finalProjectID)
	return pv, ev, err
}

// CreateClientTokenChecked creates a new browser client token inside a single write transaction
// after re-verifying that the optional source client token hash is not revoked and the optional
// machineId is not soft-removed.
func (s *Store) CreateClientTokenChecked(ctx context.Context, name, scope, machineID, sourceTokenHash string) (string, string, error) {
	if strings.TrimSpace(scope) == "" {
		scope = "browser"
	}
	if strings.TrimSpace(name) == "" {
		name = "orbit-client"
	}
	secret, err := randomSecretHex(32)
	if err != nil {
		return "", "", err
	}
	rawToken := "ock_" + secret
	tokenHash := HashSecret(rawToken)
	tokenID := newID("ctk")
	now := time.Now().UnixMilli()
	trimmedMachineID := strings.TrimSpace(machineID)

	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if strings.TrimSpace(sourceTokenHash) != "" {
			var revoked sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM client_tokens WHERE token_hash = ?`, sourceTokenHash).Scan(&revoked)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid) {
				return ErrForbidden
			}
			if err != nil {
				return err
			}
		}
		if trimmedMachineID != "" {
			var removed sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT removed_at FROM registry_machines WHERE machine_id = ?`, trimmedMachineID).Scan(&removed)
			if err == nil && removed.Valid {
				return ErrForbidden
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO client_tokens (id, name, token_hash, scope, machine_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, tokenID, name, tokenHash, scope, trimmedMachineID, now)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return rawToken, tokenID, nil
}

// DeleteDynamicProject archives a UI-registered project; returns ErrForbidden for YAML-managed projects.
func (s *Store) DeleteDynamicProject(ctx context.Context, projectID, actorID string) (protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	var ev protocol.EventRecord
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var source string
		var archived int
		err := tx.QueryRowContext(ctx, `SELECT source, archived FROM projects WHERE project_id = ?`, projectID).Scan(&source, &archived)
		if errors.Is(err, sql.ErrNoRows) || archived != 0 {
			return fmt.Errorf("%w: project %s", ErrNotFound, projectID)
		}
		if err != nil {
			return err
		}
		if source != string(protocol.ProjectSourceUI) {
			return fmt.Errorf("%w: project %s is managed in projects.yaml", ErrForbidden, projectID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET archived = 1, updated_at = ? WHERE project_id = ?`, now, projectID); err != nil {
			return err
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId": projectID,
				"action":    "removed",
			},
			CreatedAt: now,
		})
		return err
	})
	return ev, err
}
