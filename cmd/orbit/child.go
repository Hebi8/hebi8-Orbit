package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/mcpserver"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func useDelegateSocket(hubURLFlag string) (sockPath, delegateToken string, ok bool) {
	if strings.TrimSpace(hubURLFlag) != "" {
		return "", "", false
	}
	sockPath = strings.TrimSpace(os.Getenv("ORBIT_RUNNER_SOCK"))
	delegateToken = strings.TrimSpace(os.Getenv("ORBIT_DELEGATE_TOKEN"))
	if sockPath != "" && delegateToken != "" {
		return sockPath, delegateToken, true
	}
	return "", "", false
}

func runChild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runChildList(ctx, args, stdout, stderr)
	}
	switch args[0] {
	case "spawn", "new", "create":
		return runChildSpawn(ctx, args[1:], stdout, stderr)
	case "ls", "list":
		return runChildList(ctx, args[1:], stdout, stderr)
	case "show", "read", "get":
		return runChildShow(ctx, args[1:], stdout, stderr)
	case "send", "followup":
		return runChildSend(ctx, args[1:], stdout, stderr)
	case "cancel", "stop":
		return runChildCancel(ctx, args[1:], stdout, stderr)
	case "board":
		return runChildBoard(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, `Usage:
  orbit child spawn --goal "..." [--title "..."] [--role "..."] [--project <id>] [--machine <id>] [--adapter agy|claude|codex] [--model <m>] [--effort <lvl>] [--mode <mode>] [--worktree[=true|false]] [--parent <taskId>] [--no-notify] [--json]
  orbit child ls [--parent <taskId>] [--archived] [--targets] [--json]
  orbit child show <childTaskId> [--limit 10] [--json]
  orbit child send <childTaskId> --message "..." [--steer] [--mode queue|steer] [--json]
  orbit child cancel <childTaskId> [--reason "..."] [--json]
  orbit child board [read] [--parent <taskId>] [--limit 50] [--before-seq N] [--json]
  orbit child board post --body "..." [--target "@..."] [--parent <taskId>] [--json]`)
		return nil
	default:
		return fmt.Errorf("unknown child subcommand %q (try: spawn, ls, show, send, cancel, board)", args[0])
	}
}

func runChildSpawn(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("child spawn", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		parentID   = fs.String("parent", "", "parent task ID (defaults to ORBIT_TASK_ID)")
		projectID  = fs.String("project", "", "optional target project ID")
		machineID  = fs.String("machine", "", "optional target machine ID")
		title      = fs.String("title", "", "optional child title")
		role       = fs.String("role", "", "optional delegated role label")
		goal       = fs.String("goal", "", "child goal / prompt (or pass as positional args)")
		adapter    = fs.String("adapter", "", "agent adapter: agy | claude | codex")
		model      = fs.String("model", "", "optional model override")
		effort     = fs.String("effort", "", "optional thought_level override")
		mode       = fs.String("mode", "", "optional permission mode override")
		capability = fs.String("capability", "", "optional required capability")
		noNotify   = fs.Bool("no-notify", false, "disable automatic parent notification on finish")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token / client token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
		worktree   optionalBoolFlag
	)
	fs.Var(&worktree, "worktree", "override Git worktree isolation (true|false)")
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}

	goalText := strings.TrimSpace(*goal)
	if goalText == "" && fs.NArg() > 0 {
		goalText = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if goalText == "" {
		return fmt.Errorf("--goal is required")
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

	var notifyPtr *bool
	if *noNotify {
		f := false
		notifyPtr = &f
	}

	var raw []byte
	var err error
	if sock, tok, ok := useDelegateSocket(*hubURLFlag); ok && strings.TrimSpace(*parentID) == "" {
		payload := map[string]any{
			"goal": goalText,
		}
		if v := strings.TrimSpace(*title); v != "" {
			payload["title"] = v
		}
		if v := strings.TrimSpace(*role); v != "" {
			payload["role"] = v
		}
		if v := strings.TrimSpace(*projectID); v != "" {
			payload["projectId"] = v
		}
		if v := strings.TrimSpace(*machineID); v != "" {
			payload["machineId"] = v
		}
		if v := strings.TrimSpace(*capability); v != "" {
			payload["requiredCapability"] = v
		}
		if v := strings.TrimSpace(*adapter); v != "" {
			payload["adapter"] = v
		}
		if len(cfg) > 0 {
			payload["config"] = cfg
		}
		if wt := worktree.Ptr(); wt != nil {
			payload["useWorktree"] = *wt
		}
		if notifyPtr != nil {
			payload["notifyParent"] = *notifyPtr
		}
		raw, err = mcpserver.NewSocketBackend(sock, tok).CallDelegate(ctx, "spawn", payload)
	} else {
		pid := strings.TrimSpace(*parentID)
		if pid == "" {
			pid = strings.TrimSpace(os.Getenv("ORBIT_TASK_ID"))
		}
		if pid == "" {
			return fmt.Errorf("--parent <taskId> is required outside an Orbit session")
		}
		req := protocol.CreateChildTaskRequest{
			Title:              strings.TrimSpace(*title),
			Role:               strings.TrimSpace(*role),
			Goal:               goalText,
			ProjectID:          strings.TrimSpace(*projectID),
			MachineID:          strings.TrimSpace(*machineID),
			UseWorktree:        worktree.Ptr(),
			RequiredCapability: strings.TrimSpace(*capability),
			Config:             cfg,
			NotifyParent:       notifyPtr,
		}
		if adp := strings.TrimSpace(*adapter); adp != "" {
			req.Adapter = protocol.Adapter(adp)
		}
		c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
		raw, err = c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(pid)+"/children", req)
	}
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
	fmt.Fprintf(stdout, "Spawned child %s (parent=%s project=%s state=%s adapter=%s)\n",
		resp.Task.TaskID, resp.Task.ParentTaskID, resp.Task.ProjectID, resp.Task.State, resp.Task.PreferredAdapter)
	if resp.Attempt != nil && resp.Attempt.AttemptID != "" {
		fmt.Fprintf(stdout, "  Attempt: %s (machine=%s)\n", resp.Attempt.AttemptID, resp.Attempt.MachineID)
	}
	return nil
}

func runChildList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("child ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		parentID   = fs.String("parent", "", "parent task ID (defaults to ORBIT_TASK_ID)")
		archived   = fs.Bool("archived", false, "include archived child sessions")
		targets    = fs.Bool("targets", false, "include available machines/projects (delegate socket mode)")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token / client token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}

	var raw []byte
	var err error
	if sock, tok, ok := useDelegateSocket(*hubURLFlag); ok && strings.TrimSpace(*parentID) == "" {
		raw, err = mcpserver.NewSocketBackend(sock, tok).CallDelegate(ctx, "list", map[string]any{
			"includeArchived": *archived,
			"includeTargets":  *targets,
		})
	} else {
		pid := strings.TrimSpace(*parentID)
		if pid == "" {
			pid = strings.TrimSpace(os.Getenv("ORBIT_TASK_ID"))
		}
		if pid == "" {
			return fmt.Errorf("--parent <taskId> is required outside an Orbit session")
		}
		path := "/api/tasks/" + url.PathEscape(pid) + "/children"
		if *archived {
			path += "?includeArchived=1"
		}
		c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
		raw, err = c.doJSON(ctx, http.MethodGet, path, nil)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.ListChildrenResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Children) == 0 {
		fmt.Fprintln(stdout, "(no child sessions)")
		return nil
	}
	for _, ch := range resp.Children {
		role := ch.DelegatedRole
		if role == "" {
			role = "-"
		}
		fmt.Fprintf(stdout, "%-30s  %-16s  %-12s  %-10s  %s\n", ch.TaskID, ch.State, role, ch.MachineID, ch.Title)
	}
	return nil
}

func runChildShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("child show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		limit      = fs.Int("limit", 10, "max recent turns to display")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token / client token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit child show <childTaskId>")
	}
	childID := strings.TrimSpace(fs.Arg(0))

	var raw []byte
	var err error
	if sock, tok, ok := useDelegateSocket(*hubURLFlag); ok {
		raw, err = mcpserver.NewSocketBackend(sock, tok).CallDelegate(ctx, "read", map[string]any{
			"childTaskId": childID,
			"limit":       *limit,
		})
	} else {
		c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
		path := "/api/tasks/" + url.PathEscape(childID) + "/transcript?limit=" + strconv.Itoa(*limit)
		raw, err = c.doJSON(ctx, http.MethodGet, path, nil)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var tr protocol.TaskTranscriptResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Child %s [%s] project=%s (%s)\n", tr.TaskID, tr.State, tr.ProjectID, tr.Title)
	if tr.OpenApprovalCount > 0 {
		fmt.Fprintf(stdout, "  Pending approvals: %d\n", tr.OpenApprovalCount)
	}
	for _, t := range tr.Turns {
		fmt.Fprintf(stdout, "\n--- Turn #%d (%s · %s · tools=%d) ---\n", t.AttemptNo, t.State, t.TriggerKind, t.ToolCount)
		if t.Prompt != "" {
			fmt.Fprintf(stdout, "Prompt: %s\n", t.Prompt)
		}
		if t.Reply != "" {
			fmt.Fprintf(stdout, "Reply:\n%s\n", t.Reply)
		}
		if t.ErrorDetail != "" {
			fmt.Fprintf(stdout, "Error: %s\n", t.ErrorDetail)
		}
	}
	return nil
}

func runChildSend(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("child send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		message    = fs.String("message", "", "follow-up message")
		steer      = fs.Bool("steer", false, "interrupt current turn and steer immediately")
		modeFlag   = fs.String("mode", "", "queue | steer")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token / client token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit child send <childTaskId> --message \"...\" [--steer]")
	}
	childID := strings.TrimSpace(fs.Arg(0))
	msg := strings.TrimSpace(*message)
	if msg == "" && fs.NArg() > 1 {
		msg = strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	}
	if msg == "" {
		return fmt.Errorf("--message is required")
	}
	mode := protocol.FollowupModeQueue
	if strings.TrimSpace(*modeFlag) != "" {
		mode = protocol.FollowupMode(strings.TrimSpace(*modeFlag))
	}
	if *steer {
		mode = protocol.FollowupModeSteer
	}

	var raw []byte
	var err error
	if sock, tok, ok := useDelegateSocket(*hubURLFlag); ok {
		raw, err = mcpserver.NewSocketBackend(sock, tok).CallDelegate(ctx, "send", map[string]any{
			"childTaskId": childID,
			"message":     msg,
			"mode":        string(mode),
		})
	} else {
		c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
		raw, err = c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(childID)+"/followup", protocol.FollowupTaskRequest{
			Message: msg,
			Mode:    mode,
		})
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	fmt.Fprintf(stdout, "Sent follow-up (%s) to child %s\n", mode, childID)
	return nil
}

func runChildCancel(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("child cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		reason     = fs.String("reason", "cancelled via orbit child CLI", "cancellation reason")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token / client token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: orbit child cancel <childTaskId> [--reason \"...\"]")
	}
	childID := strings.TrimSpace(fs.Arg(0))

	var raw []byte
	var err error
	if sock, tok, ok := useDelegateSocket(*hubURLFlag); ok {
		raw, err = mcpserver.NewSocketBackend(sock, tok).CallDelegate(ctx, "cancel", map[string]any{
			"childTaskId": childID,
			"reason":      strings.TrimSpace(*reason),
		})
	} else {
		c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
		raw, err = c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(childID)+"/cancel", protocol.CancelTaskRequest{
			Reason: strings.TrimSpace(*reason),
		})
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	fmt.Fprintf(stdout, "Cancellation requested for child %s\n", childID)
	return nil
}

func runChildBoard(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "post", "send", "write":
			return runChildBoardPost(ctx, args[1:], stdout, stderr)
		case "read", "ls", "list", "show":
			return runChildBoardRead(ctx, args[1:], stdout, stderr)
		}
	}
	return runChildBoardRead(ctx, args, stdout, stderr)
}

func runChildBoardRead(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("child board read", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		parentID   = fs.String("parent", "", "parent task ID (defaults to ORBIT_TASK_ID)")
		beforeSeq  = fs.Int64("before-seq", 0, "return messages before this seq")
		limit      = fs.Int("limit", 50, "max board messages to return")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token / client token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}

	var raw []byte
	var err error
	if sock, tok, ok := useDelegateSocket(*hubURLFlag); ok {
		payload := map[string]any{"limit": *limit}
		if *beforeSeq > 0 {
			payload["beforeSeq"] = *beforeSeq
		}
		if pid := strings.TrimSpace(*parentID); pid != "" {
			payload["boardTaskId"] = pid
		}
		raw, err = mcpserver.NewSocketBackend(sock, tok).CallDelegate(ctx, "board-read", payload)
	} else {
		pid := strings.TrimSpace(*parentID)
		if pid == "" {
			pid = strings.TrimSpace(os.Getenv("ORBIT_TASK_ID"))
		}
		if pid == "" {
			return fmt.Errorf("--parent <taskId> is required outside an Orbit session")
		}
		q := url.Values{}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		if *beforeSeq > 0 {
			q.Set("beforeSeq", strconv.FormatInt(*beforeSeq, 10))
		}
		path := "/api/tasks/" + url.PathEscape(pid) + "/board"
		if qs := q.Encode(); qs != "" {
			path += "?" + qs
		}
		c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
		raw, err = c.doJSON(ctx, http.MethodGet, path, nil)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var board protocol.TaskBoardResponse
	if err := json.Unmarshal(raw, &board); err != nil {
		return err
	}
	if len(board.Messages) == 0 {
		fmt.Fprintln(stdout, "(no board messages)")
		return nil
	}
	for _, m := range board.Messages {
		mention := ""
		if m.TargetMention != "" {
			mention = " @" + strings.TrimPrefix(m.TargetMention, "@")
		}
		fmt.Fprintf(stdout, "#%d [%s]%s: %s\n", m.Seq, m.AuthorName, mention, m.Body)
	}
	return nil
}

func runChildBoardPost(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("child board post", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		parentID   = fs.String("parent", "", "parent task ID (defaults to ORBIT_TASK_ID)")
		bodyFlag   = fs.String("body", "", "board message body")
		targetFlag = fs.String("target", "", "optional @mention target (sibling title/taskId or @parent)")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional X-Orbit-Dev-Token / client token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	body := strings.TrimSpace(*bodyFlag)
	if body == "" && fs.NArg() > 0 {
		body = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if body == "" {
		return fmt.Errorf("--body is required")
	}

	var raw []byte
	var err error
	if sock, tok, ok := useDelegateSocket(*hubURLFlag); ok {
		payload := map[string]any{"body": body}
		if v := strings.TrimSpace(*targetFlag); v != "" {
			payload["targetMention"] = v
		}
		if pid := strings.TrimSpace(*parentID); pid != "" {
			payload["boardTaskId"] = pid
		}
		raw, err = mcpserver.NewSocketBackend(sock, tok).CallDelegate(ctx, "board-post", payload)
	} else {
		pid := strings.TrimSpace(*parentID)
		if pid == "" {
			pid = strings.TrimSpace(os.Getenv("ORBIT_TASK_ID"))
		}
		if pid == "" {
			return fmt.Errorf("--parent <taskId> is required outside an Orbit session")
		}
		c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
		raw, err = c.doJSON(ctx, http.MethodPost, "/api/tasks/"+url.PathEscape(pid)+"/board/messages", protocol.PostBoardMessageRequest{
			TargetMention: strings.TrimSpace(*targetFlag),
			Body:          body,
		})
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var msg protocol.BoardMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Posted board message #%d (%s)\n", msg.Seq, msg.MessageID)
	return nil
}
