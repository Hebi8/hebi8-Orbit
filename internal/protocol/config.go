package protocol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// HubConfig holds runtime configuration loaded from hub.yaml.
type HubConfig struct {
	SchemaVersion            int      `yaml:"schemaVersion" json:"schemaVersion"`
	WorkspaceName            string   `yaml:"workspaceName,omitempty" json:"workspaceName,omitempty"`
	ListenAddr               string   `yaml:"listenAddr" json:"listenAddr"`
	DataDir                  string   `yaml:"dataDir" json:"dataDir"`
	ConfigDir                string   `yaml:"configDir" json:"configDir"`
	HubOrigin                string   `yaml:"hubOrigin" json:"hubOrigin"`
	AllowedTailscaleLogins   []string `yaml:"allowedTailscaleLogins" json:"allowedTailscaleLogins"`
	RyugiBin                 string   `yaml:"ryugiBin" json:"ryugiBin"`
	RyugiServerHome          string   `yaml:"ryugiServerHome" json:"ryugiServerHome"`
	RyugiHost                string   `yaml:"ryugiHost,omitempty" json:"ryugiHost,omitempty"`
	RyugiURLHost             string   `yaml:"ryugiUrlHost,omitempty" json:"ryugiUrlHost,omitempty"`
	RyugiPort                int      `yaml:"ryugiPort" json:"ryugiPort"`
	RyugiPublicOrigin        string   `yaml:"ryugiPublicOrigin" json:"ryugiPublicOrigin"`
	RyugiPollIntervalSeconds int      `yaml:"ryugiPollIntervalSeconds" json:"ryugiPollIntervalSeconds"`
	DevOpen                  bool     `yaml:"devOpen" json:"devOpen"`
	DevToken                 string   `yaml:"devToken" json:"devToken"`
}

// DefaultHubConfig returns sensible local defaults matching standard XDG paths.
func DefaultHubConfig() HubConfig {
	return HubConfig{
		SchemaVersion:            1,
		ListenAddr:               "127.0.0.1:4310",
		DataDir:                  "~/.local/share/hebi8-orbit",
		ConfigDir:                "~/.config/hebi8-orbit",
		HubOrigin:                "http://127.0.0.1:4310",
		RyugiBin:                 "ryugi",
		RyugiPort:                27319,
		RyugiPollIntervalSeconds: 6,
	}
}

// MachinesConfig holds static machine registry loaded from machines.yaml.
type MachinesConfig struct {
	SchemaVersion int                      `yaml:"schemaVersion" json:"schemaVersion"`
	Machines      map[string]MachineConfig `yaml:"machines" json:"machines"`
}

type MachineConfig struct {
	DisplayName        string                             `yaml:"displayName" json:"displayName"`
	Transport          TransportMode                      `yaml:"transport" json:"transport"`
	TailscaleHost      string                             `yaml:"tailscaleHost,omitempty" json:"tailscaleHost,omitempty"`
	SSHAlias           string                             `yaml:"sshAlias,omitempty" json:"sshAlias,omitempty"`
	TokenHash          string                             `yaml:"tokenHash,omitempty" json:"tokenHash,omitempty"`
	Capabilities       []string                           `yaml:"capabilities" json:"capabilities"`
	Adapters           map[string]AdapterConfig           `yaml:"adapters,omitempty" json:"adapters,omitempty"`
	ExclusiveResources map[string]ExclusiveResourceConfig `yaml:"exclusiveResources,omitempty" json:"exclusiveResources,omitempty"`
}

type AdapterConfig struct {
	Bin              string   `yaml:"bin" json:"bin"`
	DefaultArgs      []string `yaml:"defaultArgs,omitempty" json:"defaultArgs,omitempty"`
	MaxLiveWorkers   int      `yaml:"maxLiveWorkers,omitempty" json:"maxLiveWorkers,omitempty"`
	IdleCloseSeconds int      `yaml:"idleCloseSeconds,omitempty" json:"idleCloseSeconds,omitempty"`
}

type ExclusiveResourceConfig struct {
	Probe       string `yaml:"probe" json:"probe"`
	Description string `yaml:"description" json:"description"`
}

// ProjectsConfig holds static logical project registry loaded from projects.yaml.
type ProjectsConfig struct {
	SchemaVersion int                      `yaml:"schemaVersion" json:"schemaVersion"`
	Projects      map[string]ProjectConfig `yaml:"projects" json:"projects"`
}

type ProjectConfig struct {
	DisplayName        string                         `yaml:"displayName" json:"displayName"`
	RepoIdentity       string                         `yaml:"repoIdentity" json:"repoIdentity"`
	DefaultBranch      string                         `yaml:"defaultBranch" json:"defaultBranch"`
	DefaultCapability  string                         `yaml:"defaultCapability" json:"defaultCapability"`
	DefaultAdapter     Adapter                        `yaml:"defaultAdapter" json:"defaultAdapter"`
	DefaultModelConfig ModelConfig                    `yaml:"defaultModelConfig,omitempty" json:"defaultModelConfig,omitempty"`
	IDEScheme          IDEScheme                      `yaml:"ideScheme" json:"ideScheme"`
	Anchors            map[string]ProjectAnchorConfig `yaml:"anchors" json:"anchors"`
	ExtraArtifactGlobs []string                       `yaml:"extraArtifactGlobs,omitempty" json:"extraArtifactGlobs,omitempty"`
}

type ModelConfig struct {
	Model        string `yaml:"model,omitempty" json:"model,omitempty"`
	ThoughtLevel string `yaml:"thought_level,omitempty" json:"thoughtLevel,omitempty"`
	Mode         string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

type ProjectAnchorConfig struct {
	Machine         string                `yaml:"machine" json:"machine"`
	RepoPath        string                `yaml:"repoPath,omitempty" json:"repoPath,omitempty"`
	WorktreeRoot    string                `yaml:"worktreeRoot,omitempty" json:"worktreeRoot,omitempty"`
	RemoteDir       string                `yaml:"remoteDir,omitempty" json:"remoteDir,omitempty"`
	RemoteMirrorDir string                `yaml:"remoteMirrorDir,omitempty" json:"remoteMirrorDir,omitempty"`
	DeployStrategy  *DeployStrategyConfig `yaml:"deployStrategy,omitempty" json:"deployStrategy,omitempty"`
}

type DeployStrategyConfig struct {
	Kind                    string `yaml:"kind" json:"kind"`
	TargetEnv               string `yaml:"targetEnv" json:"targetEnv"`
	VerifyRunningVersionCmd string `yaml:"verifyRunningVersionCmd" json:"verifyRunningVersionCmd"`
	DeployCmdTemplate       string `yaml:"deployCmdTemplate" json:"deployCmdTemplate"`
	HealthCheckURL          string `yaml:"healthCheckUrl" json:"healthCheckUrl"`
	RequireHumanApproval    bool   `yaml:"requireHumanApproval" json:"requireHumanApproval"`
}

// ExpandPath expands leading ~/ against the current user's home directory.
func ExpandPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

func LoadHubConfig(path string) (HubConfig, error) {
	cfg := DefaultHubConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read hub config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse hub config %s: %w", path, err)
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = 1
	}
	if cfg.SchemaVersion != 1 {
		return cfg, fmt.Errorf("unsupported hub config schemaVersion %d", cfg.SchemaVersion)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:4310"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "~/.local/share/hebi8-orbit"
	}
	if cfg.RyugiBin == "" {
		cfg.RyugiBin = "ryugi"
	}
	if cfg.RyugiPollIntervalSeconds <= 0 {
		cfg.RyugiPollIntervalSeconds = 6
	}
	return cfg, nil
}

func LoadMachinesConfig(path string) (MachinesConfig, error) {
	cfg := MachinesConfig{
		SchemaVersion: 1,
		Machines:      map[string]MachineConfig{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read machines config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse machines config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c MachinesConfig) Validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("machines.yaml: unsupported schemaVersion %d", c.SchemaVersion)
	}
	for id, m := range c.Machines {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("machines.yaml: empty machine id")
		}
		if !m.Transport.IsValid() {
			return fmt.Errorf("machines.yaml: machine %q has invalid transport %q", id, m.Transport)
		}
	}
	return nil
}

func LoadProjectsConfig(path string) (ProjectsConfig, error) {
	cfg := ProjectsConfig{
		SchemaVersion: 1,
		Projects:      map[string]ProjectConfig{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read projects config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse projects config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c ProjectsConfig) Validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("projects.yaml: unsupported schemaVersion %d", c.SchemaVersion)
	}
	for id, p := range c.Projects {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("projects.yaml: empty project id")
		}
		if strings.TrimSpace(p.DisplayName) == "" {
			return fmt.Errorf("projects.yaml: project %q missing displayName", id)
		}
		if strings.TrimSpace(p.RepoIdentity) == "" {
			return fmt.Errorf("projects.yaml: project %q missing repoIdentity", id)
		}
		if p.DefaultAdapter != "" && !p.DefaultAdapter.IsValid() {
			return fmt.Errorf("projects.yaml: project %q invalid defaultAdapter %q", id, p.DefaultAdapter)
		}
		if p.IDEScheme != "" && !p.IDEScheme.IsValid() {
			return fmt.Errorf("projects.yaml: project %q invalid ideScheme %q", id, p.IDEScheme)
		}
	}
	return nil
}
