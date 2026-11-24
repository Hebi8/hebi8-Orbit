package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type hubClient struct {
	baseURL  string
	devToken string
	http     *http.Client
}

func newHubClient(configDir, dataDir, hubURLFlag, tokenFlag string) *hubClient {
	hubURL, _, devToken := resolveHubDefaults(configDir, dataDir, hubURLFlag, tokenFlag)
	return &hubClient{
		baseURL:  hubURL,
		devToken: devToken,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *hubClient) doJSON(ctx context.Context, method, path string, reqBody any) ([]byte, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-Orbit-Request", "1")
	}
	if c.devToken != "" {
		req.Header.Set("X-Orbit-Dev-Token", c.devToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s failed (%d): %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func writeRawJSON(w io.Writer, raw []byte) error {
	_, err := w.Write(append(bytes.TrimSpace(raw), '\n'))
	return err
}

func runTasks(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runTasksList(ctx, args, stdout, stderr)
	}
	switch args[0] {
	case "list", "ls":
		return runTasksList(ctx, args[1:], stdout, stderr)
	case "new", "create":
		return runTasksNew(ctx, args[1:], stdout, stderr)
	case "show", "get":
		return runTasksShow(ctx, args[1:], stdout, stderr)
	case "followup", "send":
		return runTasksFollowup(ctx, args[1:], stdout, stderr)
	case "cancel", "stop":
		return runTasksCancel(ctx, args[1:], stdout, stderr)
	case "retry":
		return runTasksRetry(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, `Usage:
  orbit tasks [list] [--project <id>] [--json]
  orbit tasks new (--project <id> | --chat) --goal "..." [--title "..."] [--adapter agy|claude|codex] [--model <m>] [--effort <lvl>] [--mode <mode>] [--capability <cap>] [--dirty] [--json]
  orbit tasks show <taskId> [--json]
  orbit tasks followup <taskId> --message "..." [--mode queue|steer|fork_new_session] [--adapter <adp>] [--model <m>] [--json]
  orbit tasks cancel <taskId> [--reason "..."]
  orbit tasks retry <taskId> [--verified-site]`)
		return nil
	default:
		return fmt.Errorf("unknown tasks subcommand %q (try: list, new, show, followup, cancel, retry)", args[0])
	}
}

func runTasksList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		projectID  = fs.String("project", "", "filter by project ID (defaults to active tasks across all projects)")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	var tasks []protocol.TaskSummary

	if pid := strings.TrimSpace(*projectID); pid != "" {
		raw, err := c.doJSON(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(pid)+"/tasks", nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		var resp struct {
			Tasks []protocol.TaskSummary `json:"tasks"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return err
		}
		tasks = resp.Tasks
	} else {
		raw, err := c.doJSON(ctx, http.MethodGet, "/api/bootstrap", nil)
		if err != nil {
			return err
		}
		var boot protocol.BootstrapResponse
		if err := json.Unmarshal(raw, &boot); err != nil {
			return err
		}
		if *jsonOut {
			enc, _ := json.MarshalIndent(boot.ActiveTasks, "", "  ")
			return writeRawJSON(stdout, enc)
		}
		tasks = boot.ActiveTasks
	}

	if len(tasks) == 0 {
		fmt.Fprintln(stdout, "(no tasks)")
		return nil
	}
	for _, t := range tasks {
		title := strings.TrimSpace(t.Title)
		if title == "" {
			title = strings.Split(strings.TrimSpace(t.Goal), "\n")[0]
		}
		fmt.Fprintf(stdout, "%-30s  %-16s  %-14s  %-7s  %s\n", t.TaskID, t.State, t.ProjectID, t.PreferredAdapter, title)
	}
	return nil
}

func runTasksNew(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		projectID  = fs.String("project", "", "target project ID")
		chatMode   = fs.Bool("chat", false, "start a free chat session (_scratch)")
		goal       = fs.String("goal", "", "task goal / initial prompt (or pass as positional args)")
		title      = fs.String("title", "", "optional task title")
		adapter    = fs.String("adapter", "", "agent adapter: agy | claude | codex")
		model      = fs.String("model", "", "optional model override")
		effort     = fs.String("effort", "", "optional thought_level / effort override (low|medium|high|xhigh)")
		mode       = fs.String("mode", "", "optional permission mode override (ask|auto|full)")
		capability = fs.String("capability", "", "optional required machine capability")
		dirty      = fs.Bool("dirty", false, "import dirty workspace snapshot from local repo")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	pid := strings.TrimSpace(*projectID)
	if *chatMode && pid == "" {
		pid = protocol.ScratchProjectID
	}
	if pid == "" {
		return fmt.Errorf("--project <id> (or --chat) is required")
	}
	goalText := strings.TrimSpace(*goal)
	if goalText == "" && fs.NArg() > 0 {
		goalText = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if goalText == "" {
		return fmt.Errorf("--goal is required")
	}

	req := protocol.CreateTaskRequest{
		Title:                strings.TrimSpace(*title),
		Goal:                 goalText,
		RequiredCapability:   strings.TrimSpace(*capability),
		ImportDirtyWorkspace: *dirty,
	}
	if adp := strings.TrimSpace(*adapter); adp != "" {
		req.Adapter = protocol.Adapter(adp)
	}
	cfg := map[string]string{}
	if v := strings.TrimSpace(*model); v != "" {
		cfg["model"] = v
	}
	if v := strings.TrimSpace(*effort); v != "" {
		cfg["thought_level"] = v
	}
	if v := strings.TrimSpace(*mode); v != "" {
		cfg["mode"] = v
	}
	if len(cfg) > 0 {
		req.Config = cfg
	}

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(pid)+"/tasks", req)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.CreateTaskResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created task %s (project=%s state=%s adapter=%s)\n",
		resp.Task.TaskID, resp.Task.ProjectID, resp.Task.State, resp.Task.PreferredAdapter)
	if resp.Attempt.AttemptID != "" {
		fmt.Fprintf(stdout, "  Attempt: %s (machine=%s)\n", resp.Attempt.AttemptID, resp.Attempt.MachineID)
	}
	fmt.Fprintf(stdout, "  URL:     %s/#/t/%s\n", c.baseURL, resp.Task.TaskID)
	return nil
}

func runTasksShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit tasks show <taskId>")
	}
	taskID := strings.TrimSpace(fs.Arg(0))

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/tasks/"+url.PathEscape(taskID)+"/timeline", nil)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var tl protocol.TaskTimelineResponse
	if err := json.Unmarshal(raw, &tl); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Task %s [%s] project=%s adapter=%s\n", tl.Task.TaskID, tl.Task.State, tl.Task.ProjectID, tl.Task.PreferredAdapter)
	if tl.Task.Title != "" {
		fmt.Fprintf(stdout, "  Title: %s\n", tl.Task.Title)
	}
	fmt.Fprintf(stdout, "  Goal:  %s\n", tl.Task.Goal)
	fmt.Fprintf(stdout, "  Attempts (%d):\n", len(tl.Attempts))
	for _, a := range tl.Attempts {
		fmt.Fprintf(stdout, "    #%d %s [%s] machine=%s adapter=%s\n", a.AttemptNo, a.AttemptID, a.State, a.MachineID, a.Adapter)
	}
	if len(tl.Deliveries) > 0 {
		fmt.Fprintf(stdout, "  Deliveries (%d):\n", len(tl.Deliveries))
		for _, d := range tl.Deliveries {
			fmt.Fprintf(stdout, "    - %s %s (+%d -%d) %s\n", d.DeliveryID, shortSHA(d.HeadSHA), d.Manifest.Insertions, d.Manifest.Deletions, d.Summary)
		}
	}
	openAnns := 0
	for _, a := range tl.Annotations {
		if a.Status == protocol.AnnotationOpen {
			openAnns++
		}
	}
	if openAnns > 0 {
		fmt.Fprintf(stdout, "  Open annotations: %d\n", openAnns)
	}
	return nil
}

func runTasksFollowup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks followup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		message    = fs.String("message", "", "follow-up prompt text")
		mode       = fs.String("mode", "queue", "queue | steer | fork_new_session")
		adapter    = fs.String("adapter", "", "optional adapter override (agy|claude|codex)")
		model      = fs.String("model", "", "optional model override")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit tasks followup <taskId> --message \"...\"")
	}
	taskID := strings.TrimSpace(fs.Arg(0))
	msg := strings.TrimSpace(*message)
	if msg == "" && fs.NArg() > 1 {
		msg = strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	}
	if msg == "" {
		return fmt.Errorf("--message is required")
	}

	req := protocol.FollowupTaskRequest{
		Message: msg,
		Mode:    protocol.FollowupMode(strings.TrimSpace(*mode)),
	}
	if adp := strings.TrimSpace(*adapter); adp != "" {
		req.Adapter = protocol.Adapter(adp)
	}
	if m := strings.TrimSpace(*model); m != "" {
		req.Config = map[string]string{"model": m}
	}

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(taskID)+"/followup", req)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.FollowupTaskResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if resp.Attempt != nil {
		fmt.Fprintf(stdout, "Dispatched follow-up attempt %s on task %s\n", resp.Attempt.AttemptID, taskID)
	} else if resp.PendingInput != nil {
		fmt.Fprintf(stdout, "Queued follow-up input %s (position=%d) on task %s\n", resp.PendingInput.InputID, resp.PendingInput.Position, taskID)
	} else {
		fmt.Fprintf(stdout, "Follow-up accepted on task %s\n", taskID)
	}
	return nil
}

func runTasksCancel(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		reason     = fs.String("reason", "cancelled via orbit CLI", "cancellation reason")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit tasks cancel <taskId>")
	}
	taskID := strings.TrimSpace(fs.Arg(0))
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(taskID)+"/cancel", protocol.CancelTaskRequest{Reason: *reason})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	fmt.Fprintf(stdout, "Cancellation requested for task %s\n", taskID)
	return nil
}

func runTasksRetry(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks retry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		verified   = fs.Bool("verified-site", false, "confirm workspace was inspected after needs_verification")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit tasks retry <taskId>")
	}
	taskID := strings.TrimSpace(fs.Arg(0))
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(taskID)+"/retry", protocol.RetryTaskRequest{VerifiedSite: *verified})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	fmt.Fprintf(stdout, "Retry initiated for task %s\n", taskID)
	return nil
}

func runApprovals(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || args[0] == "list" || args[0] == "ls" {
		sub := args
		if len(sub) > 0 && (sub[0] == "list" || sub[0] == "ls") {
			sub = sub[1:]
		}
		return runApprovalsList(ctx, sub, stdout, stderr)
	}
	switch args[0] {
	case "allow", "approve":
		return runApprovalDecide(ctx, true, args[1:], stdout, stderr)
	case "reject", "deny":
		return runApprovalDecide(ctx, false, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, `Usage:
  orbit approvals [list] [--json]
  orbit approvals allow <approvalId> [--option <id>] [--note "..."]
  orbit approvals reject <approvalId> [--option <id>] [--note "..."]`)
		return nil
	default:
		return fmt.Errorf("unknown approvals subcommand %q (try: list, allow, reject)", args[0])
	}
}

func runApprovalsList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("approvals list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/approvals", nil)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp struct {
		Approvals []protocol.ApprovalView `json:"approvals"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Approvals) == 0 {
		fmt.Fprintln(stdout, "(no approvals)")
		return nil
	}
	for _, a := range resp.Approvals {
		fmt.Fprintf(stdout, "%-30s  %-10s  %-14s  %s\n", a.ApprovalID, a.State, a.ProjectID, a.ActionName)
	}
	return nil
}

func runApprovalDecide(ctx context.Context, allow bool, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("approvals decide", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		optionID   = fs.String("option", "", "explicit optionId (defaults to matching allow/reject option)")
		note       = fs.String("note", "", "optional decision note")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("approvalId is required")
	}
	approvalID := strings.TrimSpace(fs.Arg(0))
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)

	opt := strings.TrimSpace(*optionID)
	var matchedApproval *protocol.ApprovalView
	// Fetch approvals to resolve expectedAttemptId, expectedPayloadSha256, and default allow/reject optionId
	if rawList, err := c.doJSON(ctx, http.MethodGet, "/api/approvals", nil); err == nil {
		var resp struct {
			Approvals []protocol.ApprovalView `json:"approvals"`
		}
		if json.Unmarshal(rawList, &resp) == nil {
			for i := range resp.Approvals {
				a := &resp.Approvals[i]
				if a.ApprovalID != approvalID {
					continue
				}
				matchedApproval = a
				if opt == "" {
					for _, o := range a.Options {
						if allow && (o.Kind == "allow_once" || o.Kind == "allow_always" || strings.Contains(strings.ToLower(o.ID), "allow")) {
							opt = o.ID
							break
						}
						if !allow && (o.Kind == "reject_once" || o.Kind == "reject_always" || strings.Contains(strings.ToLower(o.ID), "reject")) {
							opt = o.ID
							break
						}
					}
				}
				break
			}
		}
	}
	if opt == "" {
		if allow {
			opt = "allow"
		} else {
			opt = "reject"
		}
	}

	decideReq := protocol.DecideApprovalRequest{
		OptionID: opt,
		Note:     strings.TrimSpace(*note),
	}
	if matchedApproval != nil {
		decideReq.ExpectedAttemptID = matchedApproval.AttemptID
		decideReq.ExpectedPayloadSHA256 = matchedApproval.PayloadSHA256
	}

	raw, err := c.doJSON(ctx, http.MethodPost, "/api/approvals/"+url.PathEscape(approvalID)+"/decide", decideReq)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	verb := "Rejected"
	if allow {
		verb = "Approved"
	}
	fmt.Fprintf(stdout, "%s approval %s (option=%s)\n", verb, approvalID, opt)
	return nil
}

func runPA(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "chat" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("pa", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		threadID   = fs.String("thread", "", "existing PA thread ID")
		newThread  = fs.Bool("new", false, "start a fresh PA thread")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	msg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if msg == "" {
		return fmt.Errorf("usage: orbit pa [--thread <id>] [--new] \"<message>\"")
	}
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)

	tid := strings.TrimSpace(*threadID)
	if *newThread {
		rawTh, err := c.doJSON(ctx, http.MethodPost, "/api/pa/threads", protocol.CreatePAThreadRequest{})
		if err != nil {
			return err
		}
		var th struct {
			Thread protocol.PAThread `json:"thread"`
		}
		if err := json.Unmarshal(rawTh, &th); err != nil {
			return err
		}
		tid = th.Thread.PAThreadID
	}

	raw, err := c.doJSON(ctx, http.MethodPost, "/api/pa/chat", protocol.PAChatRequest{
		ThreadID: tid,
		Message:  msg,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.PAChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	fmt.Fprintln(stdout, resp.AssistantMessage.Content)
	return nil
}

func runArtifacts(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("artifacts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		projectID  = fs.String("project", "", "filter by project ID")
		kindFilter = fs.String("kind", "", "filter by artifact kind (design|markdown|pptx|docx|spreadsheet|file)")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/artifacts", nil)
	if err != nil {
		return err
	}
	var resp protocol.ListArtifactsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	filtered := make([]protocol.GlobalArtifactView, 0, len(resp.Artifacts))
	for _, a := range resp.Artifacts {
		if *projectID != "" && a.ProjectID != *projectID {
			continue
		}
		if *kindFilter != "" && a.Kind != *kindFilter {
			continue
		}
		filtered = append(filtered, a)
	}
	if *jsonOut {
		enc, _ := json.MarshalIndent(filtered, "", "  ")
		return writeRawJSON(stdout, enc)
	}
	if len(filtered) == 0 {
		fmt.Fprintln(stdout, "(no artifacts)")
		return nil
	}
	for _, a := range filtered {
		fmt.Fprintf(stdout, "%-10s  %-16s  %-28s  %s\n", a.Kind, a.ProjectName, a.Title, a.Locator)
	}
	return nil
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	hubURL, dataDir, devToken := resolveHubDefaults(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	checks := []doctorCheck{}

	// 1. Hub /api/health
	hc := &hubClient{baseURL: hubURL, devToken: devToken, http: &http.Client{Timeout: 5 * time.Second}}
	if raw, err := hc.doJSON(ctx, http.MethodGet, "/api/health", nil); err != nil {
		checks = append(checks, doctorCheck{Name: "hub", OK: false, Detail: fmt.Sprintf("%s unreachable: %v", hubURL, err)})
	} else {
		checks = append(checks, doctorCheck{Name: "hub", OK: true, Detail: fmt.Sprintf("%s (%s)", hubURL, strings.TrimSpace(string(raw)))})
	}

	// 2. Local data dir / runner socket
	socks, _ := filepath.Glob(filepath.Join(dataDir, "runner-*.sock"))
	if len(socks) > 0 {
		checks = append(checks, doctorCheck{Name: "runner-socket", OK: true, Detail: strings.Join(socks, ", ")})
	} else {
		checks = append(checks, doctorCheck{Name: "runner-socket", OK: false, Detail: "no runner-*.sock found in " + dataDir})
	}

	// 3. Core tools & major ACP agents
	for _, tool := range []struct{ label, bin string }{
		{"git", "git"},
		{"agent:agy", "agy"},
		{"agent:claude", "claude-agent-acp"},
		{"agent:codex", "codex-acp"},
		{"ryugi", "ryugi"},
	} {
		if p, err := exec.LookPath(tool.bin); err == nil {
			checks = append(checks, doctorCheck{Name: tool.label, OK: true, Detail: p})
		} else {
			checks = append(checks, doctorCheck{Name: tool.label, OK: false, Detail: tool.bin + " not found in PATH"})
		}
	}

	if *jsonOut {
		enc, _ := json.MarshalIndent(checks, "", "  ")
		return writeRawJSON(stdout, enc)
	}
	fmt.Fprintf(stdout, "hebi8-Orbit Doctor (orbit %s)\n", Version)
	for _, ch := range checks {
		mark := "OK  "
		if !ch.OK {
			mark = "MISS"
		}
		fmt.Fprintf(stdout, "  [%s] %-15s %s\n", mark, ch.Name, ch.Detail)
	}
	return nil
}

func runOpen(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		printOnly  = fs.Bool("print", false, "print URL instead of launching browser")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	hubURL, _, _ := resolveHubDefaults(*configDir, *dataDirFlg, *hubURLFlag, "")
	if *printOnly {
		fmt.Fprintln(stdout, hubURL)
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", hubURL)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", hubURL)
	default:
		cmd = exec.Command("xdg-open", hubURL)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stdout, "%s\n", hubURL)
		return fmt.Errorf("launch browser: %w", err)
	}
	fmt.Fprintf(stdout, "Opened %s\n", hubURL)
	return nil
}
