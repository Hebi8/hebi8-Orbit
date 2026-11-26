package sshcfg

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMatchMachine_IncludeWildcardsAndScoring(t *testing.T) {
	dir := t.TempDir()
	confD := filepath.Join(dir, "conf.d")
	if err := os.MkdirAll(confD, 0o755); err != nil {
		t.Fatal(err)
	}

	// Included sub-config with concrete hosts, wildcard inheritance, and Match block
	subCfg := `
Host workstation-lan
    HostName 192.0.2.10
    User dev

Host workstation-ts
    HostName workstation.example.ts.net
    User dev
    Port 2222

Host *.example.ts.net
    User fallbackuser

Match host *.internal
    User ignoreduser
`
	if err := os.WriteFile(filepath.Join(confD, "nodes.conf"), []byte(subCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Self-referencing include to verify cycle protection
	cycleCfg := `
Include conf.d/cycle.conf
Host jump-box
    HostName jump.example.com
    ProxyJump bastion.example.com
`
	if err := os.WriteFile(filepath.Join(confD, "cycle.conf"), []byte(cycleCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	rootCfg := `
# Root SSH config
Include conf.d/*.conf

Host dev-node !dev-node-skip *.ignored
    HostName dev-node.example.ts.net
    User alice

Host *
    Port 22
`
	rootPath := filepath.Join(dir, "config")
	if err := os.WriteFile(rootPath, []byte(rootCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Match by tailscaleHost against included host
	res := MatchMachine(context.Background(), Target{
		MachineID:     "workstation",
		DisplayName:   "Workstation",
		TailscaleHost: "workstation.example.ts.net",
	}, MatchOptions{
		ConfigPath:     rootPath,
		SSHDir:         dir,
		DisableSSHExec: true,
		DisableProbe:   true,
		DisableDNS:     true,
	})

	if !res.LocalConfigAvailable {
		t.Fatalf("expected LocalConfigAvailable=true")
	}
	if res.Recommended != "workstation-ts" {
		t.Fatalf("expected recommended workstation-ts, got %q (candidates=%+v)", res.Recommended, res.Candidates)
	}
	if len(res.Candidates) == 0 || res.Candidates[0].Port != 2222 || res.Candidates[0].User != "dev" {
		t.Fatalf("unexpected top candidate: %+v", res.Candidates)
	}

	// Verify wildcards & negated patterns are excluded from AllHosts
	for _, h := range res.AllHosts {
		if h == "*" || h == "*.example.ts.net" || h == "*.ignored" || h == "!dev-node-skip" {
			t.Fatalf("wildcard or negated pattern leaked into AllHosts: %q", h)
		}
	}

	// 2. Match by short hostname label & exact alias on root config
	res2 := MatchMachine(context.Background(), Target{
		MachineID:     "dev-node",
		TailscaleHost: "dev-node.example.ts.net",
	}, MatchOptions{
		ConfigPath:     rootPath,
		SSHDir:         dir,
		DisableSSHExec: true,
		DisableProbe:   true,
		DisableDNS:     true,
	})
	if res2.Recommended != "dev-node" {
		t.Fatalf("expected recommended dev-node, got %q", res2.Recommended)
	}
}

func TestMatchMachine_ConcurrentTCPProbePrefersReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	openPort := ln.Addr().(*net.TCPAddr).Port

	// Grab a closed port reliably by listening and closing immediately.
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closedLn.Addr().(*net.TCPAddr).Port
	_ = closedLn.Close()

	dir := t.TempDir()
	cfg := fmt.Sprintf(`
Host node-down
    HostName 127.0.0.1
    Port %d
    User dev

Host node-up
    HostName 127.0.0.1
    Port %d
    User dev
`, closedPort, openPort)
	cfgPath := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	res := MatchMachine(context.Background(), Target{
		MachineID:     "node-down", // slightly higher static score for node-down, but unreachable!
		TailscaleHost: "127.0.0.1",
	}, MatchOptions{
		ConfigPath:     cfgPath,
		SSHDir:         dir,
		DisableSSHExec: true,
		DisableDNS:     true,
		ProbeTimeout:   500 * time.Millisecond,
	})

	if res.Recommended != "node-up" {
		t.Fatalf("expected reachable node-up to win over unreachable node-down, got %q (cands=%+v)", res.Recommended, res.Candidates)
	}
}

func TestMatchMachine_IncludeInsideHostBlockPreservesContext(t *testing.T) {
	dir := t.TempDir()
	confD := filepath.Join(dir, "conf.d")
	if err := os.MkdirAll(confD, 0o700); err != nil {
		t.Fatal(err)
	}
	// Included file has NO Host header — its directives belong to the enclosing Host block!
	if err := os.WriteFile(filepath.Join(confD, "devbox_opts.conf"), []byte("HostName 198.51.100.66\nPort 2222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainCfg := `
Host my-devbox
    Include conf.d/devbox_opts.conf
    User alice
`
	cfgPath := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgPath, []byte(mainCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	res := MatchMachine(context.Background(), Target{
		MachineID:     "other-id",
		TailscaleHost: "198.51.100.66",
	}, MatchOptions{
		ConfigPath:     cfgPath,
		SSHDir:         dir,
		DisableSSHExec: true,
		DisableProbe:   true,
		DisableDNS:     true,
	})
	if res.Recommended != "my-devbox" {
		t.Fatalf("P2-15 REGRESSION: expected my-devbox matched via Include inside Host block, got %q (cands=%+v)", res.Recommended, res.Candidates)
	}
	if len(res.Candidates) == 0 || res.Candidates[0].HostName != "198.51.100.66" || res.Candidates[0].Port != 2222 || res.Candidates[0].User != "alice" {
		t.Fatalf("expected inherited HostName/Port + parent User=alice, got %+v", res.Candidates)
	}
}
