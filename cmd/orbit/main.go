package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"gopkg.in/yaml.v3"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
)

type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(v string) error {
	if strings.TrimSpace(v) != "" {
		*s = append(*s, v)
	}
	return nil
}

// pendingDeliveryRecord persists the immutable checkpoint identity before network/push steps
// so `--resume <deliveryId>` always retries with the exact same deliveryId, headSha, and payload.
type pendingDeliveryRecord struct {
	DeliveryID     string                       `json:"deliveryId"`
	RepoRoot       string                       `json:"repoRoot"`
	ProjectID      string                       `json:"projectId"`
	TaskID         string                       `json:"taskId,omitempty"`
	ReviewBaseSHA  string                       `json:"reviewBaseSha"`
	HeadSHA        string                       `json:"headSha"`
	ReusedHead     bool                         `json:"reusedHead"`
	MirrorPath     string                       `json:"mirrorPath"`
	HubURL         string                       `json:"hubUrl"`
	Manifest       protocol.DeliveryManifest    `json:"manifest"`
	Blobs          []gitops.PackagedBlob        `json:"blobs,omitempty"`
	RequestPayload protocol.ManualImportRequest `json:"requestPayload"`
	CreatedAt      int64                        `json:"createdAt"`
}

// Version metadata injected via -ldflags at release time.
var (
	Version   = "0.2.0-dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "orbit: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage(stdout)
		return nil
	}

	switch args[0] {
	case "up":
		return runUp(ctx, args[1:], stdout, stderr)
	case "pair":
		return runPair(ctx, args[1:], stdout, stderr)
	case "join":
		return runJoin(ctx, args[1:], stdout, stderr)
	case "discover", "scan":
		return runDiscover(ctx, args[1:], stdout, stderr)
	case "agents", "agent":
		return runAgents(ctx, args[1:], stdout, stderr)
	case "machines", "machine":
		return runMachines(ctx, args[1:], stdout, stderr)
	case "projects", "project":
		return runProjects(ctx, args[1:], stdout, stderr)
	case "service":
		return runService(ctx, args[1:], stdout, stderr)
	case "init":
		return runInit(ctx, args[1:], os.Stdin, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "orbit %s (commit=%s built=%s)\n", Version, Commit, BuildDate)
		return nil
	case "deliver":
		return runDeliver(ctx, args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "brief":
		return runBrief(ctx, args[1:], stdout, stderr)
	case "tasks", "task", "ls":
		return runTasks(ctx, args[1:], stdout, stderr)
	case "child", "children":
		return runChild(ctx, args[1:], stdout, stderr)
	case "mcp-serve":
		return runMCPServe(ctx, args[1:], os.Stdin, stdout, stderr)
	case "approvals", "approval":
		return runApprovals(ctx, args[1:], stdout, stderr)
	case "hebi8", "pa":
		return runHebi8(ctx, args[1:], stdout, stderr)
	case "artifacts", "artifact":
		return runArtifacts(ctx, args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(ctx, args[1:], stdout, stderr)
	case "open":
		return runOpen(ctx, args[1:], stdout, stderr)
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  orbit up [--no-runner] [--service] [--json]               Start or attach to local Orbit on this machine
  orbit pair [--runner|--client|--both] [--machine <id>]    Generate a one-time pairing code & wait for node
  orbit join <hub-url> <pair-code> [--no-runner] [--service] Join an existing Orbit network
  orbit discover [--json]                                   Scan Tailscale & local ports for Orbit Hubs
  orbit agents [list|check|test|install|enable|disable] ... Inspect and configure machine Agents
  orbit machines [ls|rename|rm] ...                         Manage connected machines
  orbit projects [ls|add|rm] ...                            Manage registered projects
  orbit service [install|uninstall|status] [hub|runner|all] Manage background user services
  orbit init [--connect <url> | --hub | --runner --hub-url <url> | --project [dir]]
  orbit status [--hub <url>] [--json]
  orbit tasks [list|new|show|followup|cancel|retry] ...
  orbit child [spawn|ls|show|send|cancel] ...               Manage delegated child sessions
  orbit mcp-serve                                           Run Orbit stdio MCP server (5 sub-session tools)
  orbit approvals [list|allow|reject] ...
  orbit hebi8 "<message>"                                   Send a turn to the single Hebi8 main conversation (alias: orbit pa)
  orbit hebi8 [fold|line|expand <nodeId>|pin <nodeId>]      Manage Hebi8 Fold-Rollover context tree
  orbit artifacts [--project <id>] [--kind <type>] [--json]
  orbit brief [--since-hours 24] [--project <id>] [--json]
  orbit deliver --summary "..." [--final] [--attach <file>...]                               (inside runner worktree via ORBIT_RUNNER_SOCK)
  orbit deliver --project <id> [--repo <dir>] [--base <sha>] [--task <id>] [--title "..."] [--summary "..."] [--attach <file>...]
  orbit deliver --resume <deliveryId> [--hub <url>] [--mirror <path>]
  orbit doctor [--hub <url>] [--json]
  orbit open [--hub <url>]
  orbit version

Environment variables:
  ORBIT_RUNNER_SOCK Local runner Unix domain socket (auto-injected for ACP agent deliveries)
  ORBIT_ATTEMPT_ID  Optional active attempt ID override for runner socket delivery
  ORBIT_TASK_ID     Optional active task ID override for runner socket delivery
  ORBIT_HUB_URL     Hub HTTP base URL (default from client.yaml/hub.yaml or http://127.0.0.1:4310)
  ORBIT_CONFIG_DIR  Config directory (default ~/.config/hebi8-orbit)
  ORBIT_DATA_DIR    Data directory (default from hub.yaml or ~/.local/share/hebi8-orbit)
  ORBIT_CLIENT_TOKEN Optional paired client Bearer token (ock_...)
  ORBIT_DEV_TOKEN   Optional loopback dev token header`)
}

func resolveHubAuth(configDirFlag, dataDirFlag, hubURLFlag, tokenFlag string) (hubURL, dataDir, devToken, clientToken string) {
	cfgDir := configDirFlag
	if cfgDir == "" {
		cfgDir = os.Getenv("ORBIT_CONFIG_DIR")
	}
	if cfgDir == "" {
		cfgDir = "~/.config/hebi8-orbit"
	}
	cfgDir = protocol.ExpandPath(cfgDir)

	hubCfg, _ := protocol.LoadHubConfig(filepath.Join(cfgDir, "hub.yaml"))
	var clientCfg cliClientConfig
	if raw, err := os.ReadFile(filepath.Join(cfgDir, "client.yaml")); err == nil {
		_ = yaml.Unmarshal(raw, &clientCfg)
	}

	dataDir = dataDirFlag
	if dataDir == "" {
		dataDir = os.Getenv("ORBIT_DATA_DIR")
	}
	if dataDir == "" {
		dataDir = hubCfg.DataDir
	}
	dataDir = protocol.ExpandPath(dataDir)

	hubURL = hubURLFlag
	if hubURL == "" {
		hubURL = os.Getenv("ORBIT_HUB_URL")
	}
	if hubURL == "" && strings.TrimSpace(clientCfg.HubURL) != "" {
		hubURL = strings.TrimSpace(clientCfg.HubURL)
	}
	localHubURL := ""
	{
		addr := hubCfg.ListenAddr
		if addr == "" {
			addr = "127.0.0.1:4310"
		}
		if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
			localHubURL = strings.TrimRight(addr, "/")
		} else {
			localHubURL = "http://" + strings.TrimRight(addr, "/")
		}
	}
	if hubURL == "" {
		hubURL = localHubURL
	}
	hubURL = strings.TrimRight(hubURL, "/")

	// Bind saved credentials strictly to their normalized origin (checking both top-level and multi-network profiles)
	// so overriding --hub / ORBIT_HUB_URL never leaks tokens from another Orbit Hub.
	clientOriginMatch := strings.TrimSpace(clientCfg.HubURL) != "" && sameOriginURL(hubURL, clientCfg.HubURL)
	var matchedNet *cliNetworkProfile
	for i := range clientCfg.Networks {
		if strings.TrimSpace(clientCfg.Networks[i].HubURL) != "" && sameOriginURL(hubURL, clientCfg.Networks[i].HubURL) {
			matchedNet = &clientCfg.Networks[i]
			break
		}
	}
	hubOriginMatch := sameOriginURL(hubURL, localHubURL) || (strings.TrimSpace(hubCfg.HubOrigin) != "" && sameOriginURL(hubURL, hubCfg.HubOrigin))

	if strings.HasPrefix(strings.TrimSpace(tokenFlag), "ock_") {
		clientToken = strings.TrimSpace(tokenFlag)
	} else {
		devToken = tokenFlag
	}
	if clientToken == "" {
		clientToken = strings.TrimSpace(os.Getenv("ORBIT_CLIENT_TOKEN"))
	}
	if clientToken == "" && matchedNet != nil && strings.TrimSpace(matchedNet.ClientToken) != "" {
		clientToken = strings.TrimSpace(matchedNet.ClientToken)
	}
	if clientToken == "" && clientOriginMatch {
		clientToken = strings.TrimSpace(clientCfg.ClientToken)
	}
	if devToken == "" {
		devToken = os.Getenv("ORBIT_DEV_TOKEN")
	}
	if devToken == "" && matchedNet != nil && strings.TrimSpace(matchedNet.DevToken) != "" {
		devToken = strings.TrimSpace(matchedNet.DevToken)
	}
	if devToken == "" && clientOriginMatch && strings.TrimSpace(clientCfg.DevToken) != "" {
		devToken = strings.TrimSpace(clientCfg.DevToken)
	}
	if devToken == "" && hubOriginMatch {
		devToken = hubCfg.DevToken
	}
	return hubURL, dataDir, devToken, clientToken
}

func sameOriginURL(a, b string) bool {
	na, err1 := normalizeHubBaseURL(a)
	nb, err2 := normalizeHubBaseURL(b)
	if err1 != nil || err2 != nil {
		return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/"))
	}
	return strings.EqualFold(na, nb)
}

func resolveHubDefaults(configDirFlag, dataDirFlag, hubURLFlag, tokenFlag string) (hubURL, dataDir, devToken string) {
	hubURL, dataDir, devToken, _ = resolveHubAuth(configDirFlag, dataDirFlag, hubURLFlag, tokenFlag)
	return hubURL, dataDir, devToken
}

func pendingRecordPath(dataDir, deliveryID string) string {
	return filepath.Join(dataDir, "cli-pending", deliveryID+".json")
}

func pendingBlobPath(dataDir, sha256Hex string) string {
	return filepath.Join(dataDir, "cli-pending", "blobs", sha256Hex)
}

// stagePendingBlob copies attachment bytes into <dataDir>/cli-pending/blobs/<sha256>
// while verifying the digest so `--resume` remains self-contained even if source files change or vanish.
func stagePendingBlob(dataDir string, blob gitops.PackagedBlob) (string, error) {
	dest := pendingBlobPath(dataDir, blob.Item.SHA256)
	if info, err := os.Stat(dest); err == nil && info.Size() == blob.Item.Size {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	src, err := os.Open(blob.LocalPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	tmp := dest + ".tmp-" + ulid.Make().String()
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = dst.Close()
		_ = os.Remove(tmp)
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if err != nil {
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != blob.Item.SHA256 || n != blob.Item.Size {
		return "", fmt.Errorf("attachment %s changed during staging (expected %s/%d, got %s/%d)", blob.LocalPath, blob.Item.SHA256, blob.Item.Size, actual, n)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// cleanupPendingDelivery removes pendingPath and deletes any staged pending blobs
// no longer referenced by other remaining cli-pending/*.json records.
func cleanupPendingDelivery(dataDir, pendingPath string, blobs []gitops.PackagedBlob) {
	_ = os.Remove(pendingPath)
	if len(blobs) == 0 {
		return
	}
	stillReferenced := map[string]struct{}{}
	entries, err := os.ReadDir(filepath.Join(dataDir, "cli-pending"))
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if other, err := loadPendingRecord(filepath.Join(dataDir, "cli-pending", e.Name())); err == nil {
				for _, b := range other.Blobs {
					stillReferenced[b.Item.SHA256] = struct{}{}
				}
			}
		}
	}
	for _, b := range blobs {
		if _, used := stillReferenced[b.Item.SHA256]; !used {
			_ = os.Remove(pendingBlobPath(dataDir, b.Item.SHA256))
		}
	}
}

func savePendingRecord(path string, rec pendingDeliveryRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadPendingRecord(path string) (pendingDeliveryRecord, error) {
	var rec pendingDeliveryRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func runDeliver(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("deliver", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		projectID  = fs.String("project", "", "logical project ID (required unless --resume)")
		resumeID   = fs.String("resume", "", "resume a previously packaged pending deliveryId without re-committing")
		repoDir    = fs.String("repo", "", "git repository directory or subdirectory (default: current directory)")
		baseSHA    = fs.String("base", "", "explicit review base commit SHA (default: HEAD)")
		taskID     = fs.String("task", "", "existing task ID to attach this delivery to")
		title      = fs.String("title", "", "optional task title when creating a new manual task")
		summary    = fs.String("summary", "", "human-readable delivery summary")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		mirrorFlag = fs.String("mirror", "", "explicit local bare mirror path")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		machineID  = fs.String("machine", "", "optional source machine ID")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory (stores cli-pending records)")
		deliveryID = fs.String("delivery-id", "", "optional explicit delivery ID")
		attemptID  = fs.String("attempt", "", "optional attempt ID override when delivering via ORBIT_RUNNER_SOCK")
		finalFlag  = fs.Bool("final", false, "finalize the active runner attempt upon delivery (required to close swarm_monitoring)")
		jsonOut    = fs.Bool("json", false, "output JSON response")
		attaches   stringListFlag
	)
	fs.Var(&attaches, "attach", "attachment file path (repeatable; optional 'local:target/path' form)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Phase 1 Runner fast-path: if ORBIT_RUNNER_SOCK is set and --project is not explicitly specified,
	// route normal deliver AND --resume / --delivery-id retries through the local runner socket (B13).
	if sock := strings.TrimSpace(os.Getenv("ORBIT_RUNNER_SOCK")); sock != "" && strings.TrimSpace(*projectID) == "" {
		dlvOverride := strings.TrimSpace(*deliveryID)
		resumeFlag := false
		if strings.TrimSpace(*resumeID) != "" {
			dlvOverride = strings.TrimSpace(*resumeID)
			resumeFlag = true
		}
		return runDeliverViaRunnerSocket(ctx, sock, *attemptID, *taskID, dlvOverride, resumeFlag, *repoDir, *summary, *finalFlag, attaches, *jsonOut, stdout)
	}

	hubURL, dataDir, devToken := resolveHubDefaults(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)

	var rec pendingDeliveryRecord
	var pendingPath string

	if strings.TrimSpace(*resumeID) != "" {
		id := strings.TrimSpace(*resumeID)
		pendingPath = pendingRecordPath(dataDir, id)
		loaded, err := loadPendingRecord(pendingPath)
		if err != nil {
			return fmt.Errorf("load pending delivery %s (%s): %w", id, pendingPath, err)
		}
		rec = loaded
		// Allow explicit CLI overrides of hub/mirror on resume (e.g. if hub port changed)
		if *hubURLFlag != "" {
			rec.HubURL = strings.TrimRight(*hubURLFlag, "/")
		}
		if *mirrorFlag != "" {
			rec.MirrorPath = protocol.ExpandPath(*mirrorFlag)
		}
	} else {
		if strings.TrimSpace(*projectID) == "" {
			return fmt.Errorf("--project is required (or pass --resume <deliveryId>)")
		}

		repo := *repoDir
		if repo == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			repo = cwd
		}

		mirrorPath := *mirrorFlag
		if mirrorPath == "" {
			mirrorPath = filepath.Join(dataDir, "git", *projectID+".git")
		}
		mirrorPath = protocol.ExpandPath(mirrorPath)

		dlvID := strings.TrimSpace(*deliveryID)
		if dlvID == "" {
			dlvID = "dlv_" + ulid.Make().String()
		}

		// Protect any existing pending delivery record from being overwritten or re-committed
		pendingPath = pendingRecordPath(dataDir, dlvID)
		if _, err := os.Stat(pendingPath); err == nil {
			return fmt.Errorf("pending delivery %s already exists at %s; use `orbit deliver --resume %s` instead of re-packaging", dlvID, pendingPath, dlvID)
		}

		cliCWD, _ := os.Getwd()
		attachInputs, err := parseAttachFlags(attaches, cliCWD)
		if err != nil {
			return err
		}

		// 1. Zero-pollution checkpoint packaging (resolves git toplevel automatically)
		pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
			RepoDir:       repo,
			DeliveryID:    dlvID,
			TaskID:        *taskID,
			ReviewBaseSHA: *baseSHA,
			Attachments:   attachInputs,
		})
		if err != nil {
			return fmt.Errorf("package checkpoint: %w", err)
		}

		// Stage attachment bytes into cli-pending/blobs/<sha256> so --resume is resilient to source file edits/deletions
		stagedBlobs := make([]gitops.PackagedBlob, len(pkg.Blobs))
		for i, b := range pkg.Blobs {
			stagedPath, err := stagePendingBlob(dataDir, b)
			if err != nil {
				return fmt.Errorf("stage attachment %s: %w", b.Item.Path, err)
			}
			b.LocalPath = stagedPath
			stagedBlobs[i] = b
		}

		sumText := strings.TrimSpace(*summary)
		if sumText == "" {
			sumText = fmt.Sprintf("Manual delivery (%d files changed, +%d -%d)",
				pkg.Manifest.FilesChanged, pkg.Manifest.Insertions, pkg.Manifest.Deletions)
		}

		reqPayload := protocol.ManualImportRequest{
			DeliveryID:      dlvID,
			ProjectID:       *projectID,
			TaskID:          *taskID,
			Title:           *title,
			Summary:         sumText,
			SourceMachineID: *machineID,
			BaseSHA:         pkg.ReviewBaseSHA,
			HeadSHA:         pkg.HeadSHA,
			GitRef:          "refs/orbit/deliveries/" + dlvID,
			Blobs:           pkg.Manifest.Blobs,
			RyugiArtifacts:  pkg.Manifest.RyugiArtifacts,
			WorktreePath:    pkg.Manifest.WorktreePath,
		}

		rec = pendingDeliveryRecord{
			DeliveryID:     dlvID,
			RepoRoot:       pkg.RepoRoot,
			ProjectID:      *projectID,
			TaskID:         *taskID,
			ReviewBaseSHA:  pkg.ReviewBaseSHA,
			HeadSHA:        pkg.HeadSHA,
			ReusedHead:     pkg.ReusedHead,
			MirrorPath:     mirrorPath,
			HubURL:         hubURL,
			Manifest:       pkg.Manifest,
			Blobs:          stagedBlobs,
			RequestPayload: reqPayload,
			CreatedAt:      time.Now().UnixMilli(),
		}

		// 2. Persist immutable pending record BEFORE push/upload/import so retries never re-commit
		if err := savePendingRecord(pendingPath, rec); err != nil {
			return fmt.Errorf("persist pending delivery record: %w", err)
		}
	}

	failWithResumeHint := func(step string, err error) error {
		fmt.Fprintf(stderr, "hint: delivery %s saved at %s; retry with: orbit deliver --resume %s\n", rec.DeliveryID, pendingPath, rec.DeliveryID)
		return fmt.Errorf("%s: %w", step, err)
	}

	// 3. Push delivery ref to local bare mirror (protected by pre-receive hook)
	if err := gitops.PushDeliveryRef(ctx, rec.RepoRoot, rec.MirrorPath, rec.DeliveryID, rec.HeadSHA); err != nil {
		return failWithResumeHint("push delivery ref to "+rec.MirrorPath, err)
	}

	client := &http.Client{Timeout: 60 * time.Second}

	// 4. Upload attachment blobs (HEAD dedup -> PUT with streaming sha256 verification)
	for _, blob := range rec.Blobs {
		if err := uploadBlobIfMissing(ctx, client, rec.HubURL, devToken, blob); err != nil {
			return failWithResumeHint("upload blob "+blob.Item.Path, err)
		}
	}

	// 5. Register immutable delivery in orbit-hub via /api/deliveries/manual-import
	payloadBytes, err := json.Marshal(rec.RequestPayload)
	if err != nil {
		return failWithResumeHint("marshal manual-import payload", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.HubURL+"/api/deliveries/manual-import", bytes.NewReader(payloadBytes))
	if err != nil {
		return failWithResumeHint("build manual-import request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Orbit-Request", "1")
	if devToken != "" {
		httpReq.Header.Set("X-Orbit-Dev-Token", devToken)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return failWithResumeHint("call hub manual-import", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return failWithResumeHint("read hub manual-import response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failWithResumeHint(
			fmt.Sprintf("hub manual-import failed (%d)", resp.StatusCode),
			fmt.Errorf("%s", strings.TrimSpace(string(respBytes))),
		)
	}

	var result protocol.ManualImportResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return failWithResumeHint("decode manual-import response", err)
	}
	if result.Delivery.DeliveryID != rec.DeliveryID || result.Delivery.HeadSHA != rec.HeadSHA {
		return failWithResumeHint("verify manual-import response", fmt.Errorf("hub returned mismatched delivery %s/%s", result.Delivery.DeliveryID, result.Delivery.HeadSHA))
	}

	// 6. All steps confirmed matching by Hub -> clean up pending delivery record and unreferenced staged blobs
	cleanupPendingDelivery(dataDir, pendingPath, rec.Blobs)

	if *jsonOut {
		_, err := stdout.Write(append(bytes.TrimSpace(respBytes), '\n'))
		return err
	}

	shortBase := shortSHA(result.Delivery.BaseSHA)
	shortHead := shortSHA(result.Delivery.HeadSHA)
	fmt.Fprintf(stdout, "Delivered %s (project=%s task=%s created=%v)\n", result.Delivery.DeliveryID, result.Delivery.ProjectID, result.Task.TaskID, result.Created)
	fmt.Fprintf(stdout, "  Range:   %s..%s (reusedHead=%v)\n", shortBase, shortHead, rec.ReusedHead)
	fmt.Fprintf(stdout, "  Changes: %d files (+%d -%d), %d blobs\n",
		result.Delivery.Manifest.FilesChanged, result.Delivery.Manifest.Insertions, result.Delivery.Manifest.Deletions, len(result.Delivery.Manifest.Blobs))
	fmt.Fprintf(stdout, "  Ryugi:   %s (%d bindings)\n", result.Delivery.RyugiSyncState, len(result.RyugiBindings))
	for _, b := range result.RyugiBindings {
		fmt.Fprintf(stdout, "    - %s [%s] -> %s\n", b.ArtifactLocator, b.DerivedArtifactID, b.OpenURL)
	}
	if result.RyugiError != "" {
		fmt.Fprintf(stderr, "  Warning: Ryugi registration error (code delivery intact): %s\n", result.RyugiError)
	}
	return nil
}

func uploadBlobIfMissing(ctx context.Context, client *http.Client, hubURL, devToken string, blob gitops.PackagedBlob) error {
	blobURL := hubURL + "/api/blobs/" + blob.Item.SHA256

	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, blobURL, nil)
	if err != nil {
		return err
	}
	if devToken != "" {
		headReq.Header.Set("X-Orbit-Dev-Token", devToken)
	}
	headResp, err := client.Do(headReq)
	if err != nil {
		return err
	}
	headResp.Body.Close()
	if headResp.StatusCode == http.StatusOK {
		return nil
	}

	f, err := os.Open(blob.LocalPath)
	if err != nil {
		return err
	}
	defer f.Close()

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, blobURL, f)
	if err != nil {
		return err
	}
	putReq.ContentLength = blob.Item.Size
	putReq.Header.Set("X-Orbit-Request", "1")
	if devToken != "" {
		putReq.Header.Set("X-Orbit-Dev-Token", devToken)
	}

	putResp, err := client.Do(putReq)
	if err != nil {
		return err
	}
	defer putResp.Body.Close()
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		body, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("PUT blob failed (%d): %s", putResp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON response")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	hubURL, _, devToken, clientToken := resolveHubAuth(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	client := &http.Client{Timeout: 15 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/api/bootstrap", nil)
	if err != nil {
		return err
	}
	if clientToken != "" {
		req.Header.Set("Authorization", "Bearer "+clientToken)
	} else if devToken != "" {
		req.Header.Set("X-Orbit-Dev-Token", devToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch hub status: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub bootstrap failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if *jsonOut {
		_, err := stdout.Write(append(bytes.TrimSpace(raw), '\n'))
		return err
	}

	var boot protocol.BootstrapResponse
	if err := json.Unmarshal(raw, &boot); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "hebi8-Orbit Hub (%s) · snapshotSeq=%d\n", hubURL, boot.SnapshotSeq)
	fmt.Fprintf(stdout, "\nInbox (%d open):\n", len(boot.OpenInboxItems))
	if len(boot.OpenInboxItems) == 0 {
		fmt.Fprintln(stdout, "  (clear)")
	}
	for _, it := range boot.OpenInboxItems {
		fmt.Fprintf(stdout, "  [%s] %s — %s (project=%s)\n", it.Category, it.Title, it.Summary, it.ProjectID)
	}
	fmt.Fprintf(stdout, "\nProjects (%d):\n", len(boot.Projects))
	for _, p := range boot.Projects {
		fmt.Fprintf(stdout, "  - %-20s active=%d blocked=%d repo=%s\n", p.ProjectID, p.ActiveTaskCount, p.BlockedTaskCount, p.RepoIdentity)
	}
	fmt.Fprintf(stdout, "\nMachines (%d):\n", len(boot.Machines))
	for _, m := range boot.Machines {
		fmt.Fprintf(stdout, "  - %-16s %-8s (%s) caps=%v\n", m.MachineID, m.Status, m.TransportMode, m.Capabilities)
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func parseAttachFlags(attaches stringListFlag, cliCWD string) ([]gitops.AttachmentInput, error) {
	if len(attaches) == 0 {
		return nil, nil
	}
	if cliCWD == "" {
		if wd, err := os.Getwd(); err == nil {
			cliCWD = wd
		}
	}
	resolveLocal := func(p string) string {
		p = protocol.ExpandPath(strings.TrimSpace(p))
		if p != "" && !filepath.IsAbs(p) && cliCWD != "" {
			p = filepath.Join(cliCWD, p)
		}
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}

	out := make([]gitops.AttachmentInput, 0, len(attaches))
	for _, raw := range attaches {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		localPath := resolveLocal(raw)
		targetPath := ""
		if idx := strings.Index(raw, ":"); idx > 0 {
			candidateLocal := resolveLocal(raw[:idx])
			candidateTarget := strings.TrimSpace(raw[idx+1:])
			if fi, err := os.Stat(candidateLocal); err == nil && !fi.IsDir() {
				localPath = candidateLocal
				targetPath = filepath.ToSlash(filepath.Clean(candidateTarget))
			}
		}
		fi, err := os.Stat(localPath)
		if err != nil {
			return nil, fmt.Errorf("attachment %q not found at %s: %w", raw, localPath, err)
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("attachment %q (%s) must be a regular file, not a directory", raw, localPath)
		}
		out = append(out, gitops.AttachmentInput{
			LocalPath:  localPath,
			TargetPath: targetPath,
		})
	}
	return out, nil
}

func runDeliverViaRunnerSocket(
	ctx context.Context,
	sockPath, attemptFlag, taskFlag, deliveryIDFlag string,
	resume bool,
	repoFlag, summary string,
	final bool,
	attaches stringListFlag,
	jsonOut bool,
	stdout io.Writer,
) error {
	aid := strings.TrimSpace(attemptFlag)
	if aid == "" {
		aid = strings.TrimSpace(os.Getenv("ORBIT_ATTEMPT_ID"))
	}
	tid := strings.TrimSpace(taskFlag)
	if tid == "" {
		tid = strings.TrimSpace(os.Getenv("ORBIT_TASK_ID"))
	}
	cliCWD, _ := os.Getwd()
	cwd := strings.TrimSpace(repoFlag)
	if cwd == "" {
		cwd = cliCWD
	} else if !filepath.IsAbs(cwd) && cliCWD != "" {
		cwd = filepath.Join(cliCWD, cwd)
	}
	if absCWD, err := filepath.Abs(cwd); err == nil {
		cwd = absCWD
	}

	attachInputs, err := parseAttachFlags(attaches, cliCWD)
	if err != nil {
		return err
	}

	reqBody := runner.LocalDeliverRequest{
		AttemptID:   aid,
		TaskID:      tid,
		DeliveryID:  strings.TrimSpace(deliveryIDFlag),
		Resume:      resume,
		CWD:         cwd,
		Summary:     strings.TrimSpace(summary),
		Final:       final,
		Attachments: attachInputs,
	}
	rawReq, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", sockPath)
			},
		},
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/deliver", bytes.NewReader(rawReq))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("connect runner socket %s: %w", sockPath, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("runner deliver failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	if jsonOut {
		_, err := stdout.Write(append(bytes.TrimSpace(respBytes), '\n'))
		return err
	}

	var result runner.LocalDeliverResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Delivered %s via runner (project=%s task=%s attempt=%s finalized=%v)\n",
		result.DeliveryID, result.ProjectID, result.TaskID, result.AttemptID, result.Finalized)
	fmt.Fprintf(stdout, "  Range:   %s..%s (%d files, +%d -%d)\n",
		shortSHA(result.BaseSHA), shortSHA(result.HeadSHA), result.FilesChanged, result.Insertions, result.Deletions)
	return nil
}

func runBrief(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		sinceHours = fs.Int("since-hours", 24, "lookback window in hours")
		projectID  = fs.String("project", "", "optional project filter")
		jsonOut    = fs.Bool("json", false, "output JSON response")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	hubURL, _, devToken := resolveHubDefaults(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	client := &http.Client{Timeout: 15 * time.Second}

	body, err := json.Marshal(protocol.PABriefRequest{
		SinceHours: *sinceHours,
		ProjectID:  strings.TrimSpace(*projectID),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/pa/brief", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orbit-Request", "1")
	if devToken != "" {
		req.Header.Set("X-Orbit-Dev-Token", devToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch hub brief: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub brief failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if *jsonOut {
		_, err := stdout.Write(append(bytes.TrimSpace(raw), '\n'))
		return err
	}

	var brief protocol.PABriefResponse
	if err := json.Unmarshal(raw, &brief); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Brief Me (%s)\n", brief.Summary)
	fmt.Fprintf(stdout, "\nOpen Inbox (%d):\n", brief.OpenInboxCount)
	if len(brief.OpenInboxItems) == 0 {
		fmt.Fprintln(stdout, "  (clear)")
	}
	for _, it := range brief.OpenInboxItems {
		fmt.Fprintf(stdout, "  [%s] %s — %s (%s)\n", it.Category, it.Title, it.Summary, it.ProjectID)
	}
	fmt.Fprintf(stdout, "\nActive Tasks (%d):\n", len(brief.ActiveTasks))
	if len(brief.ActiveTasks) == 0 {
		fmt.Fprintln(stdout, "  (none)")
	}
	for _, t := range brief.ActiveTasks {
		fmt.Fprintf(stdout, "  - %-14s %-16s %s (%s)\n", t.ProjectID, t.State, t.Title, t.TaskID)
	}
	fmt.Fprintf(stdout, "\nRecent Deliveries (%d in %dh):\n", len(brief.RecentDeliveries), brief.SinceHours)
	for _, d := range brief.RecentDeliveries {
		fmt.Fprintf(stdout, "  - %s (%s %s) %s\n", d.DeliveryID, d.ProjectID, shortSHA(d.HeadSHA), d.Summary)
	}
	return nil
}
