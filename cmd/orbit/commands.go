package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	baseURL     string
	devToken    string
	clientToken string
	http        *http.Client
}

func newHubClient(configDir, dataDir, hubURLFlag, tokenFlag string) *hubClient {
	hubURL, _, devToken, clientToken := resolveHubAuth(configDir, dataDir, hubURLFlag, tokenFlag)
	return &hubClient{
		baseURL:     hubURL,
		devToken:    devToken,
		clientToken: clientToken,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				if !sameOriginURL(req.URL.Scheme+"://"+req.URL.Host, hubURL) {
					return fmt.Errorf("refusing cross-origin redirect to %s", req.URL.Host)
				}
				return nil
			},
		},
	}
}

type httpStatusError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s failed (%d): %s", e.Method, e.Path, e.StatusCode, e.Body)
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
	if c.clientToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.clientToken)
	} else if c.devToken != "" {
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
		return nil, &httpStatusError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(raw)),
		}
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
  orbit tasks new (--project <id> | --chat) --goal "..." [--title "..."] [--parent <taskId>] [--role "..."] [--machine <id>] [--worktree[=true|false]] [--adapter agy|claude|codex] [--model <m>] [--effort <lvl>] [--mode <mode>] [--capability <cap>] [--dirty] [--json]
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

type optionalBoolFlag struct {
	set bool
	val bool
}

func (f *optionalBoolFlag) IsBoolFlag() bool { return true }
func (f *optionalBoolFlag) String() string {
	if !f.set {
		return ""
	}
	if f.val {
		return "true"
	}
	return "false"
}
func (f *optionalBoolFlag) Set(s string) error {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "1", "true", "yes":
		f.set = true
		f.val = true
		return nil
	case "0", "false", "no":
		f.set = true
		f.val = false
		return nil
	default:
		return fmt.Errorf("invalid boolean value %q", s)
	}
}
func (f *optionalBoolFlag) Ptr() *bool {
	if !f.set {
		return nil
	}
	v := f.val
	return &v
}

func runTasksNew(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		projectID  = fs.String("project", "", "target project ID")
		chatMode   = fs.Bool("chat", false, "start a free chat session (_scratch)")
		goal       = fs.String("goal", "", "task goal / initial prompt (or pass as positional args)")
		title      = fs.String("title", "", "optional task title")
		parentID   = fs.String("parent", "", "optional parent task ID (creates a delegated child session)")
		machineID  = fs.String("machine", "", "optional target machine ID")
		role       = fs.String("role", "", "optional delegated role label")
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
		worktree   optionalBoolFlag
	)
	fs.Var(&worktree, "worktree", "override Git worktree isolation (true|false)")
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
		ParentTaskID:         strings.TrimSpace(*parentID),
		DelegatedRole:        strings.TrimSpace(*role),
		MachineID:            strings.TrimSpace(*machineID),
		UseWorktree:          worktree.Ptr(),
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
	if err := parseFlagsPermuted(fs, args); err != nil {
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
	if err := parseFlagsPermuted(fs, args); err != nil {
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
	if err := parseFlagsPermuted(fs, args); err != nil {
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
	if err := parseFlagsPermuted(fs, args); err != nil {
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
	if err := parseFlagsPermuted(fs, args); err != nil {
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

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedStringFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*f = append(*f, trimmed)
		}
	}
	return nil
}

func isTerminalAttemptState(st protocol.AttemptState) bool {
	switch st {
	case protocol.AttemptSucceeded,
		protocol.AttemptNeedsVerification,
		protocol.AttemptFailed,
		protocol.AttemptExpired,
		protocol.AttemptCancelled:
		return true
	default:
		return false
	}
}

func runHebi8(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "fold":
			return runHebi8Fold(ctx, args[1:], stdout, stderr)
		case "expand":
			return runHebi8Expand(ctx, args[1:], stdout, stderr)
		case "pin":
			return runHebi8Pin(ctx, args[1:], stdout, stderr)
		case "line", "tree", "list", "ls":
			return runHebi8Line(ctx, args[1:], stdout, stderr)
		case "-h", "--help", "help":
			fmt.Fprintln(stdout, `Usage:
  orbit hebi8 [--ref <taskId>] [--adapter <adp>] [--model <m>] [--no-wait] [--json] "<message>"
  orbit hebi8 fold [--title "..."] [--summary "..."] [--tags "a,b"] [--json]
  orbit hebi8 line [--json]
  orbit hebi8 expand <nodeId> [--offset 0] [--limit 20] [--json]
  orbit hebi8 pin <nodeId> [--unpin] [--json]
  orbit hebi8 --brief [--json]`)
			return nil
		}
	}
	return runPA(ctx, args, stdout, stderr)
}

func runHebi8Fold(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hebi8 fold", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		titleFlag   = fs.String("title", "", "optional segment title (<=60 chars)")
		summaryFlag = fs.String("summary", "", "optional first-person summary body")
		tagsFlag    = fs.String("tags", "", "optional comma-separated tags")
		hubURLFlag  = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag   = fs.String("token", "", "optional auth token")
		configDir   = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg  = fs.String("data-dir", "", "orbit data directory")
		jsonOut     = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	req := protocol.Hebi8FoldRequest{
		Title:   strings.TrimSpace(*titleFlag),
		Summary: strings.TrimSpace(*summaryFlag),
	}
	if rawTags := strings.TrimSpace(*tagsFlag); rawTags != "" {
		for _, p := range strings.Split(rawTags, ",") {
			if t := strings.TrimSpace(p); t != "" {
				req.Tags = append(req.Tags, t)
			}
		}
	}
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/hebi8/fold", req)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.Hebi8FoldResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	turns := 0
	if resp.Segment.AttemptRange != nil {
		turns = resp.Segment.AttemptRange.TurnCount
	}
	fmt.Fprintf(stdout, "Folded segment %s (%d turns) · %s\n", resp.Segment.ID, turns, resp.Segment.Title)
	if resp.Fold != nil {
		fmt.Fprintf(stdout, "  Higher fold: %s (L%d, covers %d nodes) · %s\n", resp.Fold.ID, resp.Fold.Level, len(resp.Fold.Covers), resp.Fold.Title)
	}
	return nil
}

func runHebi8Line(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hebi8 line", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional auth token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/hebi8/line", nil)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.Hebi8LineResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Nodes) == 0 {
		fmt.Fprintln(stdout, "(no hebi8-line nodes yet)")
		return nil
	}
	for _, n := range resp.Nodes {
		pinMark := ""
		if n.Pinned {
			pinMark = " [PINNED]"
		}
		foldedMark := ""
		if n.FoldedInto != "" {
			foldedMark = " -> " + n.FoldedInto
		}
		fmt.Fprintf(stdout, "%-14s L%d  %-7s  %s%s%s\n", n.ID, n.Level, n.Kind, n.Title, pinMark, foldedMark)
	}
	return nil
}

func runHebi8Expand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hebi8 expand", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		offsetFlag = fs.Int("offset", 0, "turn pagination offset for segment nodes")
		limitFlag  = fs.Int("limit", 20, "turn pagination limit for segment nodes")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional auth token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit hebi8 expand <nodeId> [--offset 0] [--limit 20] [--json]")
	}
	nodeID := strings.TrimSpace(fs.Arg(0))
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	path := fmt.Sprintf("/api/hebi8/line/%s/expand?offset=%d&limit=%d", url.PathEscape(nodeID), *offsetFlag, *limitFlag)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.Hebi8ExpandResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s [%s L%d] %s\n", resp.Node.ID, resp.Node.Kind, resp.Node.Level, resp.Node.Title)
	if resp.Node.CoversFrom != "" {
		fmt.Fprintf(stdout, "Range: %s ~ %s\n", resp.Node.CoversFrom, resp.Node.CoversTo)
	}
	if strings.TrimSpace(resp.Summary) != "" {
		fmt.Fprintf(stdout, "\n%s\n", strings.TrimSpace(resp.Summary))
	}
	if len(resp.Children) > 0 {
		fmt.Fprintf(stdout, "\nCovered children (%d):\n", len(resp.Children))
		for _, ch := range resp.Children {
			fmt.Fprintf(stdout, "\n--- %s (L%d) %s ---\n%s\n", ch.ID, ch.Level, ch.Title, strings.TrimSpace(ch.Summary))
		}
	}
	if len(resp.Turns) > 0 {
		fmt.Fprintf(stdout, "\nRaw turns (%d/%d, offset=%d):\n", len(resp.Turns), resp.TotalTurns, resp.Offset)
		for _, t := range resp.Turns {
			fmt.Fprintf(stdout, "\n[Turn #%d %s]\nUser: %s\nHebi8: %s\n", t.AttemptNo, t.AttemptID, strings.TrimSpace(t.UserPrompt), strings.TrimSpace(t.AssistantReply))
		}
	}
	return nil
}

func runHebi8Pin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hebi8 pin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		unpinFlag  = fs.Bool("unpin", false, "unpin the specified node")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional auth token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit hebi8 pin <nodeId> [--unpin] [--json]")
	}
	nodeID := strings.TrimSpace(fs.Arg(0))
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/hebi8/line/"+url.PathEscape(nodeID)+"/pin", protocol.Hebi8PinRequest{
		Pinned: !*unpinFlag,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.Hebi8PinResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if resp.Pinned {
		fmt.Fprintf(stdout, "Pinned %s (total pins: %d)\n", resp.NodeID, len(resp.Pins))
	} else {
		fmt.Fprintf(stdout, "Unpinned %s (total pins: %d)\n", resp.NodeID, len(resp.Pins))
	}
	return nil
}

func runPA(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "chat" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("hebi8", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		taskIDFlag  = fs.String("task", "", "explicit _pa task ID override (defaults to the single Hebi8 main conversation)")
		threadID    = fs.String("thread", "", "legacy PA thread ID (uses /api/pa/chat)")
		newThread   = fs.Bool("new", false, "ignored for single-line Hebi8 (retained for flag compatibility)")
		briefFlag   = fs.Bool("brief", false, "fetch structured workspace brief (/api/pa/brief)")
		adapterFlag = fs.String("adapter", "", "optional agent adapter override (agy|claude|codex)")
		machineFlag = fs.String("machine", "", "optional target machine ID")
		modelFlag   = fs.String("model", "", "optional model override")
		effortFlag  = fs.String("effort", "", "optional effort/thought_level override")
		modeFlag    = fs.String("mode", "", "optional permission mode override")
		noWait      = fs.Bool("no-wait", false, "dispatch without waiting for assistant reply")
		timeoutSec  = fs.Int("timeout", 180, "max seconds to wait for assistant reply")
		hubURLFlag  = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag   = fs.String("token", "", "optional auth token")
		configDir   = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg  = fs.String("data-dir", "", "orbit data directory")
		jsonOut     = fs.Bool("json", false, "output JSON")
		refFlags    repeatedStringFlag
	)
	fs.Var(&refFlags, "ref", "referenced conversation taskId (repeatable or comma-separated)")
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)

	// Legacy structured brief shortcut preserved for compatibility
	if *briefFlag {
		raw, err := c.doJSON(ctx, http.MethodPost, "/api/pa/brief", map[string]any{})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		var brief protocol.PABriefResponse
		if err := json.Unmarshal(raw, &brief); err != nil {
			return err
		}
		if strings.TrimSpace(brief.Summary) != "" {
			fmt.Fprintln(stdout, brief.Summary)
		} else {
			return writeRawJSON(stdout, raw)
		}
		return nil
	}

	msg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if msg == "" {
		return fmt.Errorf("usage: orbit hebi8 [--ref <taskId>] [--adapter <adp>] [--brief] \"<message>\"")
	}

	// Legacy /api/pa/chat path ONLY if caller explicitly passed --thread
	if tid := strings.TrimSpace(*threadID); tid != "" && strings.TrimSpace(*taskIDFlag) == "" {
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
	_ = newThread

	cfg := map[string]string{}
	if v := strings.TrimSpace(*modelFlag); v != "" {
		cfg["model"] = v
	}
	if v := strings.TrimSpace(*effortFlag); v != "" {
		cfg["thought_level"] = v
	}
	if v := strings.TrimSpace(*modeFlag); v != "" {
		cfg["mode"] = v
	}

	taskID := strings.TrimSpace(*taskIDFlag)
	var attemptID string
	var pendingInputID string

	// Default: attach directly to the single workspace Hebi8 main conversation via GET /api/hebi8/main.
	// Only fall back to legacy POST /api/projects/_pa/tasks when the Hub explicitly returns 404
	// (older Hub version without /api/hebi8/main). Any 5xx/timeout/JSON error is surfaced directly
	// so we never accidentally spawn a second _pa thread.
	legacyFallback := false
	if taskID == "" {
		mainRaw, mainErr := c.doJSON(ctx, http.MethodGet, "/api/hebi8/main", nil)
		if mainErr != nil {
			var statusErr *httpStatusError
			if errors.As(mainErr, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
				legacyFallback = true
			} else {
				return mainErr
			}
		} else {
			var mainResp protocol.Hebi8MainResponse
			if err := json.Unmarshal(mainRaw, &mainResp); err != nil {
				return fmt.Errorf("decode /api/hebi8/main response: %w", err)
			}
			taskID = strings.TrimSpace(mainResp.Task.TaskID)
			if taskID == "" {
				return fmt.Errorf("/api/hebi8/main returned empty taskId")
			}
		}
	}

	if taskID == "" && legacyFallback {
		createReq := protocol.CreateTaskRequest{
			Goal:              msg,
			MachineID:         strings.TrimSpace(*machineFlag),
			ReferencedTaskIDs: []string(refFlags),
		}
		if adp := strings.TrimSpace(*adapterFlag); adp != "" {
			createReq.Adapter = protocol.Adapter(adp)
		}
		if len(cfg) > 0 {
			createReq.Config = cfg
		}
		raw, err := c.doJSON(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(protocol.PAProjectID)+"/tasks", createReq)
		if err != nil {
			return err
		}
		var resp protocol.CreateTaskResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return err
		}
		taskID = resp.Task.TaskID
		if resp.Attempt != nil {
			attemptID = resp.Attempt.AttemptID
		}
		if *noWait {
			if *jsonOut {
				return writeRawJSON(stdout, raw)
			}
			fmt.Fprintf(stdout, "Hebi8 task %s dispatched (attempt=%s)\n", taskID, attemptID)
			return nil
		}
	} else {
		fReq := protocol.FollowupTaskRequest{
			Message:           msg,
			Mode:              protocol.FollowupModeQueue,
			MachineID:         strings.TrimSpace(*machineFlag),
			ReferencedTaskIDs: []string(refFlags),
		}
		if adp := strings.TrimSpace(*adapterFlag); adp != "" {
			fReq.Adapter = protocol.Adapter(adp)
		}
		if len(cfg) > 0 {
			fReq.Config = cfg
		}
		raw, err := c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(taskID)+"/followup", fReq)
		if err != nil {
			return err
		}
		var resp protocol.FollowupTaskResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return err
		}
		if resp.Attempt != nil {
			attemptID = resp.Attempt.AttemptID
		}
		if resp.PendingInput != nil {
			pendingInputID = resp.PendingInput.InputID
		}
		if *noWait {
			if *jsonOut {
				return writeRawJSON(stdout, raw)
			}
			if attemptID != "" {
				fmt.Fprintf(stdout, "Hebi8 turn dispatched on %s (attempt=%s)\n", taskID, attemptID)
			} else {
				fmt.Fprintf(stdout, "Hebi8 turn queued on %s (input=%s)\n", taskID, pendingInputID)
			}
			return nil
		}
	}

	waitDur := time.Duration(*timeoutSec) * time.Second
	if waitDur <= 0 {
		waitDur = 180 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitDur)
	defer cancel()

	// Poll queue input directly by inputId until dispatched, then poll timeline until attempt finishes.
	var finalAtt *protocol.AttemptView
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	for {
		if attemptID == "" && pendingInputID != "" {
			qPath := "/api/tasks/" + url.PathEscape(taskID) + "/queue/" + url.PathEscape(pendingInputID)
			qRaw, qErr := c.doJSON(waitCtx, http.MethodGet, qPath, nil)
			if qErr != nil {
				if waitCtx.Err() != nil {
					return fmt.Errorf("timed out waiting for queued PA input %s: %w", pendingInputID, qErr)
				}
				return fmt.Errorf("queued PA input %s unavailable before dispatch: %w", pendingInputID, qErr)
			}
			var pi protocol.PendingInput
			if err := json.Unmarshal(qRaw, &pi); err != nil {
				return err
			}
			if pi.DispatchedAttemptID != "" {
				attemptID = pi.DispatchedAttemptID
			} else if pi.State != protocol.PendingInputQueued {
				return fmt.Errorf("queued PA input %s ended in state %s before dispatch", pendingInputID, pi.State)
			} else {
				select {
				case <-waitCtx.Done():
					return fmt.Errorf("timed out waiting for queued PA input %s on task %s", pendingInputID, taskID)
				case <-ticker.C:
				}
				continue
			}
		}

		tlRaw, err := c.doJSON(waitCtx, http.MethodGet, "/api/tasks/"+url.PathEscape(taskID)+"/timeline?includeRaw=0", nil)
		if err != nil {
			if waitCtx.Err() != nil {
				return fmt.Errorf("timed out waiting for PA task %s: %w", taskID, err)
			}
			return err
		}
		var tl protocol.TaskTimelineResponse
		if err := json.Unmarshal(tlRaw, &tl); err != nil {
			return err
		}
		if attemptID != "" {
			for i := range tl.Attempts {
				if tl.Attempts[i].AttemptID == attemptID {
					att := tl.Attempts[i]
					if isTerminalAttemptState(att.State) {
						finalAtt = &att
					}
					break
				}
			}
		}
		if finalAtt != nil {
			break
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for PA task %s (attempt=%s)", taskID, attemptID)
		case <-ticker.C:
		}
	}

	// Collect full assistant reply from paginated raw ACP events for finalAtt.AttemptID
	var replySB strings.Builder
	var afterSeq int64
	for page := 0; page < 40; page++ {
		rawURL := fmt.Sprintf("/api/attempts/%s/raw?afterSeq=%d&limit=500", url.PathEscape(finalAtt.AttemptID), afterSeq)
		rawBytes, err := c.doJSON(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		var rawResp protocol.AttemptRawEventsResponse
		if err := json.Unmarshal(rawBytes, &rawResp); err != nil {
			return err
		}
		for _, ev := range rawResp.Events {
			if ev.Seq > afterSeq {
				afterSeq = ev.Seq
			}
			if ev.Kind == protocol.EventACPMessageBatch {
				if txt, ok := ev.Payload["text"].(string); ok && txt != "" {
					replySB.WriteString(txt)
				}
			}
		}
		if len(rawResp.Events) < 500 {
			break
		}
	}
	replyText := strings.TrimSpace(replySB.String())

	if *jsonOut {
		enc, _ := json.MarshalIndent(map[string]any{
			"taskId":      taskID,
			"attemptId":   finalAtt.AttemptID,
			"state":       finalAtt.State,
			"reply":       replyText,
			"errorCode":   finalAtt.ErrorCode,
			"errorDetail": finalAtt.ErrorDetail,
		}, "", "  ")
		return writeRawJSON(stdout, enc)
	}

	if replyText != "" {
		fmt.Fprintln(stdout, replyText)
	}
	if finalAtt.State != protocol.AttemptSucceeded {
		detail := strings.TrimSpace(finalAtt.ErrorDetail)
		if detail == "" {
			detail = string(finalAtt.State)
		}
		return fmt.Errorf("PA attempt %s ended with %s: %s", finalAtt.AttemptID, finalAtt.State, detail)
	}
	if replyText == "" {
		fmt.Fprintf(stdout, "(PA completed task %s with empty text reply)\n", taskID)
	}
	fmt.Fprintf(stderr, "task=%s\n", taskID)
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

func runOpen(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, "")
	targetURL := c.baseURL + "/"
	// Best-effort short-lived auth handoff so opening in system browser signs in seamlessly.
	if !*printOnly && (c.clientToken != "" || c.devToken != "") {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if raw, err := c.doJSON(hctx, http.MethodPost, "/api/auth/handoff", map[string]any{}); err == nil {
			var ho protocol.AuthHandoffResponse
			if json.Unmarshal(raw, &ho) == nil && ho.Code != "" {
				targetURL = c.baseURL + "/#/auth/" + url.PathEscape(ho.Code)
			}
		}
		cancel()
	}
	if *printOnly {
		fmt.Fprintln(stdout, c.baseURL)
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", targetURL)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", targetURL)
	default:
		cmd = exec.Command("xdg-open", targetURL)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stdout, "%s\n", c.baseURL)
		return fmt.Errorf("launch browser: %w", err)
	}
	fmt.Fprintf(stdout, "Opened %s\n", c.baseURL)
	return nil
}
