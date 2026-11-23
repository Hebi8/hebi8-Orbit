package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
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
	case "deliver":
		return runDeliver(ctx, args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  orbit deliver --project <id> [--repo <dir>] [--base <sha>] [--task <id>] [--title "..."] [--summary "..."] [--attach <file>...]
  orbit deliver --resume <deliveryId> [--hub <url>] [--mirror <path>]
  orbit status [--hub <url>] [--json]

Environment variables:
  ORBIT_HUB_URL     Hub HTTP base URL (default from hub.yaml or http://127.0.0.1:4310)
  ORBIT_CONFIG_DIR  Config directory (default ~/.config/hebi8-orbit)
  ORBIT_DATA_DIR    Data directory (default from hub.yaml or ~/.local/share/hebi8-orbit)
  ORBIT_DEV_TOKEN   Optional loopback dev token header`)
}

func resolveHubDefaults(configDirFlag, dataDirFlag, hubURLFlag, tokenFlag string) (hubURL, dataDir, devToken string) {
	cfgDir := configDirFlag
	if cfgDir == "" {
		cfgDir = os.Getenv("ORBIT_CONFIG_DIR")
	}
	if cfgDir == "" {
		cfgDir = "~/.config/hebi8-orbit"
	}
	cfgDir = protocol.ExpandPath(cfgDir)

	hubCfg, _ := protocol.LoadHubConfig(filepath.Join(cfgDir, "hub.yaml"))

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
	if hubURL == "" {
		addr := hubCfg.ListenAddr
		if addr == "" {
			addr = "127.0.0.1:4310"
		}
		if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
			hubURL = addr
		} else {
			hubURL = "http://" + addr
		}
	}
	hubURL = strings.TrimRight(hubURL, "/")

	devToken = tokenFlag
	if devToken == "" {
		devToken = os.Getenv("ORBIT_DEV_TOKEN")
	}
	if devToken == "" {
		devToken = hubCfg.DevToken
	}
	return hubURL, dataDir, devToken
}

func pendingRecordPath(dataDir, deliveryID string) string {
	return filepath.Join(dataDir, "cli-pending", deliveryID+".json")
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
		jsonOut    = fs.Bool("json", false, "output JSON response")
		attaches   stringListFlag
	)
	fs.Var(&attaches, "attach", "attachment file path (repeatable; optional 'local:target/path' form)")

	if err := fs.Parse(args); err != nil {
		return err
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

		var attachInputs []gitops.AttachmentInput
		for _, raw := range attaches {
			localPath := raw
			targetPath := ""
			if idx := strings.Index(raw, ":"); idx > 0 {
				candidateLocal := raw[:idx]
				candidateTarget := raw[idx+1:]
				if _, err := os.Stat(candidateLocal); err == nil {
					localPath = candidateLocal
					targetPath = candidateTarget
				}
			}
			attachInputs = append(attachInputs, gitops.AttachmentInput{
				LocalPath:  localPath,
				TargetPath: targetPath,
			})
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
			Blobs:          pkg.Blobs,
			RequestPayload: reqPayload,
			CreatedAt:      time.Now().UnixMilli(),
		}

		// 2. Persist immutable pending record BEFORE push/upload/import so retries never re-commit
		pendingPath = pendingRecordPath(dataDir, dlvID)
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

	// 6. All steps succeeded -> clean up pending delivery record
	_ = os.Remove(pendingPath)

	if *jsonOut {
		_, err := stdout.Write(append(bytes.TrimSpace(respBytes), '\n'))
		return err
	}

	var result protocol.ManualImportResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return fmt.Errorf("decode manual-import response: %w", err)
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

	hubURL, _, devToken := resolveHubDefaults(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	client := &http.Client{Timeout: 15 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/api/bootstrap", nil)
	if err != nil {
		return err
	}
	if devToken != "" {
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
