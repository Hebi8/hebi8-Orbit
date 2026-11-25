package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func runService(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, `Usage:
  orbit service install [hub|runner|all] [--config-dir <dir>] [--machine <id>] [--force]
  orbit service uninstall [hub|runner|all]
  orbit service status [hub|runner|all]`)
		return nil
	}
	sub := args[0]
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configDirFlag = fs.String("config-dir", "", "orbit config directory")
		dataDirFlag   = fs.String("data-dir", "", "orbit data directory")
		machineFlag   = fs.String("machine", "", "machineId override for runner")
		force         = fs.Bool("force", false, "overwrite existing service unit")
	)
	if err := parseFlagsPermuted(fs, args[1:]); err != nil {
		return err
	}
	target := "all"
	if fs.NArg() > 0 {
		target = strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	}
	if target != "hub" && target != "runner" && target != "all" {
		return fmt.Errorf("invalid service target %q (expected hub, runner, or all)", target)
	}

	cfgDir := resolveConfigDir(*configDirFlag)
	dataDir := resolveDataDir(*dataDirFlag, cfgDir)
	mid := defaultMachineID(*machineFlag)

	targets := []string{target}
	if target == "all" {
		targets = []string{"hub", "runner"}
	}

	for _, kind := range targets {
		switch sub {
		case "install":
			if err := installServiceUnit(ctx, kind, cfgDir, dataDir, mid, *force, stdout); err != nil {
				return err
			}
		case "uninstall", "remove":
			if err := uninstallServiceUnit(ctx, kind, stdout); err != nil {
				return err
			}
		case "status":
			printServiceStatus(ctx, kind, stdout)
		default:
			return fmt.Errorf("unknown service subcommand %q (try: install, uninstall, status)", sub)
		}
	}
	return nil
}

func installAndStartService(ctx context.Context, kind, cfgDir, dataDir, machineID string, w io.Writer) error {
	return installServiceUnit(ctx, kind, cfgDir, dataDir, machineID, false, w)
}

func installServiceUnit(ctx context.Context, kind, cfgDir, dataDir, machineID string, force bool, w io.Writer) error {
	switch runtime.GOOS {
	case "linux":
		return installSystemdUserUnit(ctx, kind, cfgDir, machineID, force, w)
	case "darwin":
		return installLaunchAgent(ctx, kind, cfgDir, dataDir, machineID, force, w)
	default:
		return fmt.Errorf("background service management is supported on Linux (systemd user) and macOS (LaunchAgent)")
	}
}

func uninstallServiceUnit(ctx context.Context, kind string, w io.Writer) error {
	switch runtime.GOOS {
	case "linux":
		unit := "orbit-" + kind + ".service"
		_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", unit).Run()
		home, _ := os.UserHomeDir()
		unitPath := filepath.Join(home, ".config", "systemd", "user", unit)
		_ = os.Remove(unitPath)
		_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
		fmt.Fprintf(w, "✓ 已卸载 %s\n", unit)
		return nil
	case "darwin":
		label := "io.github.hebi8.orbit-" + kind
		home, _ := os.UserHomeDir()
		plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
		uid := strconv.Itoa(os.Getuid())
		_ = exec.CommandContext(ctx, "launchctl", "bootout", "gui/"+uid+"/"+label).Run()
		_ = os.Remove(plistPath)
		fmt.Fprintf(w, "✓ 已卸载 %s\n", label)
		return nil
	default:
		return fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
}

func printServiceStatus(ctx context.Context, kind string, w io.Writer) {
	switch runtime.GOOS {
	case "linux":
		unit := "orbit-" + kind + ".service"
		out, err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", unit).Output()
		state := strings.TrimSpace(string(out))
		if err != nil && state == "" {
			state = "inactive"
		}
		fmt.Fprintf(w, "  %-22s %s\n", unit, state)
	case "darwin":
		label := "io.github.hebi8.orbit-" + kind
		uid := strconv.Itoa(os.Getuid())
		err := exec.CommandContext(ctx, "launchctl", "print", "gui/"+uid+"/"+label).Run()
		state := "active"
		if err != nil {
			state = "inactive"
		}
		fmt.Fprintf(w, "  %-30s %s\n", label, state)
	default:
		fmt.Fprintf(w, "  orbit-%s: unsupported on %s\n", kind, runtime.GOOS)
	}
}

func installSystemdUserUnit(ctx context.Context, kind, cfgDir, machineID string, force bool, w io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	unitName := "orbit-" + kind + ".service"
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	unitPath := filepath.Join(unitDir, unitName)

	// Respect pre-existing customized systemd unit unless --force
	if _, err := os.Stat(unitPath); err == nil && !force {
		if out, err := exec.CommandContext(ctx, "systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
			return fmt.Errorf("enable existing %s: %v (%s)", unitName, err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(w, "✓ 已启用现有服务 %s\n", unitName)
		return nil
	}

	binName := "orbit-" + kind
	binPath, err := locateSiblingOrPathBin(binName)
	if err != nil {
		return err
	}
	execStart := fmt.Sprintf("%s --config-dir %s", binPath, cfgDir)
	after := "network-online.target"
	if kind == "runner" {
		after = "network-online.target orbit-hub.service"
		if machineID != "" {
			execStart = fmt.Sprintf("%s --config-dir %s --orbit-bin-dir %s", binPath, cfgDir, filepath.Dir(binPath))
		}
	}
	pathEnv := os.Getenv("PATH")
	if !strings.Contains(pathEnv, filepath.Dir(binPath)) {
		pathEnv = filepath.Dir(binPath) + ":" + pathEnv
	}

	content := fmt.Sprintf(`[Unit]
Description=hebi8-Orbit %s
After=%s

[Service]
Type=simple
ExecStart=%s
Environment="PATH=%s"
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`, strings.ToUpper(kind), after, execStart, pathEnv)

	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return err
	}
	_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.CommandContext(ctx, "systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("start %s: %v (%s)", unitName, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "✓ 已安装并启动 %s\n", unitName)
	return nil
}

func installLaunchAgent(ctx context.Context, kind, cfgDir, dataDir, _ string, force bool, w io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	label := "io.github.hebi8.orbit-" + kind
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	plistPath := filepath.Join(agentsDir, label+".plist")
	uid := strconv.Itoa(os.Getuid())

	if _, err := os.Stat(plistPath); err != nil || force {
		binPath, err := locateSiblingOrPathBin("orbit-" + kind)
		if err != nil {
			return err
		}
		logDir := filepath.Join(dataDir, "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			return err
		}
		argsXML := fmt.Sprintf("    <string>%s</string>\n    <string>--config-dir</string>\n    <string>%s</string>\n", binPath, cfgDir)
		if kind == "runner" {
			argsXML += fmt.Sprintf("    <string>--orbit-bin-dir</string>\n    <string>%s</string>\n", filepath.Dir(binPath))
		}
		pathParts := []string{
			filepath.Dir(binPath),
			filepath.Join(home, ".local", "bin"),
			"/opt/homebrew/bin",
			"/usr/local/bin",
			"/usr/bin",
			"/bin",
		}
		if curPath := strings.TrimSpace(os.Getenv("PATH")); curPath != "" {
			pathParts = append(pathParts, strings.Split(curPath, ":")...)
		}
		seenPath := map[string]struct{}{}
		dedupPath := make([]string, 0, len(pathParts))
		for _, p := range pathParts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seenPath[p]; ok {
				continue
			}
			seenPath[p] = struct{}{}
			dedupPath = append(dedupPath, p)
		}
		pathEnv := strings.Join(dedupPath, ":")
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, label, argsXML, pathEnv, filepath.Join(logDir, kind+".log"), filepath.Join(logDir, kind+".log"))
		if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
			return err
		}
	}

	_ = exec.CommandContext(ctx, "launchctl", "bootout", "gui/"+uid+"/"+label).Run()
	if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %v (%s)", label, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "✓ 已安装并启动 LaunchAgent %s\n", label)
	return nil
}
