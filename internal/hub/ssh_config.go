package hub

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/sshcfg"
)

// SetSSHConfigOptions overrides ~/.ssh/config matching options (used by tests).
func (s *Server) SetSSHConfigOptions(opts sshcfg.MatchOptions) {
	s.mu.Lock()
	s.sshCfgOpts = opts
	s.mu.Unlock()
}

func (s *Server) sshMatchOptions() sshcfg.MatchOptions {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sshCfgOpts
}

func (s *Server) handleGetMachineSSHCandidates(w http.ResponseWriter, r *http.Request) {
	machineID := strings.TrimSpace(chi.URLParam(r, "machineId"))
	if machineID == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_MACHINE", "machineId is required")
		return
	}

	s.mu.RLock()
	mCfg, inMem := s.machines.Machines[machineID]
	s.mu.RUnlock()
	yamlMachines := s.effectiveYAMLMachines()

	var target sshcfg.Target
	found := inMem
	if inMem {
		target = sshcfg.Target{
			MachineID:     machineID,
			DisplayName:   mCfg.DisplayName,
			TailscaleHost: mCfg.TailscaleHost,
			SSHAlias:      mCfg.SSHAlias,
		}
	}

	if boot, err := s.store.Bootstrap(r.Context(), yamlMachines); err == nil {
		for _, mv := range boot.Machines {
			if mv.MachineID == machineID {
				found = true
				target.MachineID = mv.MachineID
				if target.DisplayName == "" {
					if mv.DisplayAlias != "" {
						target.DisplayName = mv.DisplayAlias
					} else {
						target.DisplayName = mv.DisplayName
					}
				}
				if target.TailscaleHost == "" {
					target.TailscaleHost = mv.TailscaleHost
				}
				target.SSHAlias = mv.SSHAlias
				break
			}
		}
	}

	if !found {
		writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", "machine not found")
		return
	}

	// Only inspect Hub's local ~/.ssh/config when the request genuinely originates from
	// local loopback without reverse-proxy / Tailscale Serve headers; remote browsers must
	// never mistake the Hub host's ~/.ssh/config for their own client machine's config.
	if !isLoopbackAddr(r.RemoteAddr) || hasReverseProxyHeaders(r) || !isLoopbackHost(r.Host) {
		writeJSON(w, http.StatusOK, protocol.SSHCandidatesResponse{
			LocalConfigAvailable: false,
			Candidates:           []protocol.SSHCandidate{},
			AllHosts:             []string{},
		})
		return
	}

	match := sshcfg.MatchMachine(r.Context(), target, s.sshMatchOptions())
	cands := make([]protocol.SSHCandidate, 0, len(match.Candidates))
	for _, c := range match.Candidates {
		cands = append(cands, protocol.SSHCandidate{
			Alias:       c.Alias,
			HostName:    c.HostName,
			User:        c.User,
			Port:        c.Port,
			ProxyJump:   c.ProxyJump,
			Score:       c.Score,
			MatchReason: c.MatchReason,
			Reachable:   c.Reachable,
			LatencyMs:   c.LatencyMs,
		})
	}
	allHosts := match.AllHosts
	if allHosts == nil {
		allHosts = []string{}
	}
	writeJSON(w, http.StatusOK, protocol.SSHCandidatesResponse{
		LocalConfigAvailable: match.LocalConfigAvailable,
		Recommended:          match.Recommended,
		Candidates:           cands,
		AllHosts:             allHosts,
	})
}
