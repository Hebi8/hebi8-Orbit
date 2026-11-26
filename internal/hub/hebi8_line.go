package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

const (
	hebi8MaxPins          = 8
	hebi8FoldTriggerCount = 8
	hebi8FoldBatchSize    = 5
	hebi8SuggestFoldTurns = 12
	hebi8SuggestFoldChars = 28000
	hebi8TitleMaxRunes    = 60
)

var (
	hebi8NodeIDRegex      = regexp.MustCompile(`^(seg|fld)_[0-9a-f]{8}$`)
	errHebi8NodeImmutable = errors.New("hebi8-line node already exists and is immutable")
	errHebi8PinLimit      = fmt.Errorf("最多只能钉住 %d 个节点，请先取消钉住其他节点", hebi8MaxPins)
	errHebi8NodeNotFound  = errors.New("hebi8-line node not found")
)

type hebi8PinsFile struct {
	V    int      `json:"v"`
	Pins []string `json:"pins"`
}

// hebi8LineStore manages immutable segment/fold nodes and pins.json under <dataDir>/hebi8-line/.
// Only orbit-hub writes to this directory; node JSON/MD files are write-once immutable.
type hebi8LineStore struct {
	baseDir  string
	nodesDir string
	pinsPath string
}

func newHebi8LineStore(dataDir string) *hebi8LineStore {
	base := filepath.Join(dataDir, "hebi8-line")
	return &hebi8LineStore{
		baseDir:  base,
		nodesDir: filepath.Join(base, "nodes"),
		pinsPath: filepath.Join(base, "pins.json"),
	}
}

func isValidHebi8NodeID(id string) bool {
	return hebi8NodeIDRegex.MatchString(strings.TrimSpace(id))
}

func (l *hebi8LineStore) ensureDirs() error {
	if err := os.MkdirAll(l.nodesDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(l.baseDir, 0o700); err != nil {
		return fmt.Errorf("chmod hebi8-line dir: %w", err)
	}
	if err := os.Chmod(l.nodesDir, 0o700); err != nil {
		return fmt.Errorf("chmod hebi8-line nodes dir: %w", err)
	}
	return nil
}

func (l *hebi8LineStore) RemoveUncommittedNodes(ids []string) {
	for _, id := range ids {
		if !isValidHebi8NodeID(id) {
			continue
		}
		_ = os.Remove(l.nodeJSONPath(id))
		_ = os.Remove(l.nodeMDPath(id))
	}
}

// CleanupOrphans deletes any uncommitted seg_/fld_ files in nodesDir that are NOT part of
// SQLite's authoritative committed state (e.g. files left behind after a kill -9 crash mid-fold).
func (l *hebi8LineStore) CleanupOrphans(state store.Hebi8RolloverState) {
	entries, err := os.ReadDir(l.nodesDir)
	if err != nil {
		return
	}
	// If Generation > 0 without an explicit CommittedNodeIDs manifest (legacy workspace),
	// preserve existing files until the next fold populates CommittedNodeIDs.
	if state.Generation > 0 && len(state.CommittedNodeIDs) == 0 {
		return
	}
	committed := make(map[string]bool, len(state.CommittedNodeIDs))
	for _, id := range state.CommittedNodeIDs {
		committed[strings.TrimSpace(id)] = true
	}
	var orphans []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var id string
		switch {
		case strings.HasSuffix(name, ".json"):
			id = strings.TrimSuffix(name, ".json")
		case strings.HasSuffix(name, ".md"):
			id = strings.TrimSuffix(name, ".md")
		default:
			continue
		}
		if !isValidHebi8NodeID(id) || seen[id] {
			continue
		}
		seen[id] = true
		if state.Generation == 0 || !committed[id] {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) > 0 {
		l.RemoveUncommittedNodes(orphans)
	}
}

// LoadCommitted loads only nodes committed in SQLite hebi8_rollover_state, ignoring any
// uncommitted crash-orphan files on disk.
func (l *hebi8LineStore) LoadCommitted(state store.Hebi8RolloverState) ([]protocol.Hebi8LineNode, []string, error) {
	if state.Generation == 0 {
		return []protocol.Hebi8LineNode{}, []string{}, nil
	}
	allNodes, allPins, err := l.LoadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(state.CommittedNodeIDs) == 0 {
		// Legacy compatibility for workspaces that folded before CommittedNodeIDs was tracked.
		return allNodes, allPins, nil
	}
	committed := make(map[string]bool, len(state.CommittedNodeIDs))
	for _, id := range state.CommittedNodeIDs {
		committed[strings.TrimSpace(id)] = true
	}
	nodes := make([]protocol.Hebi8LineNode, 0, len(allNodes))
	for _, n := range allNodes {
		if committed[n.ID] {
			nodes = append(nodes, n)
		}
	}
	pins := make([]string, 0, len(allPins))
	pinSet := make(map[string]bool, len(allPins))
	for _, id := range allPins {
		if committed[id] {
			pins = append(pins, id)
			pinSet[id] = true
		}
	}
	foldedInto := map[string]string{}
	for _, n := range nodes {
		if n.Kind == protocol.Hebi8NodeFold {
			for _, cid := range n.Covers {
				foldedInto[cid] = n.ID
			}
		}
	}
	for i := range nodes {
		nodes[i].Pinned = pinSet[nodes[i].ID]
		nodes[i].FoldedInto = foldedInto[nodes[i].ID]
	}
	return nodes, pins, nil
}

func (l *hebi8LineStore) nodeJSONPath(id string) string {
	return filepath.Join(l.nodesDir, id+".json")
}

func (l *hebi8LineStore) nodeMDPath(id string) string {
	return filepath.Join(l.nodesDir, id+".md")
}

func (l *hebi8LineStore) NextID(kind protocol.Hebi8NodeKind) (string, error) {
	if err := l.ensureDirs(); err != nil {
		return "", err
	}
	prefix := "seg_"
	if kind == protocol.Hebi8NodeFold {
		prefix = "fld_"
	}
	var buf [4]byte
	for range 32 {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		id := prefix + hex.EncodeToString(buf[:])
		if _, err := os.Stat(l.nodeJSONPath(id)); errors.Is(err, os.ErrNotExist) {
			if _, err2 := os.Stat(l.nodeMDPath(id)); errors.Is(err2, os.ErrNotExist) {
				return id, nil
			}
		}
	}
	return "", errors.New("failed to allocate unique hebi8 node id")
}

func (l *hebi8LineStore) LoadPins() ([]string, error) {
	raw, err := os.ReadFile(l.pinsPath)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var pf hebi8PinsFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pf.Pins))
	seen := map[string]bool{}
	for _, id := range pf.Pins {
		id = strings.TrimSpace(id)
		if isValidHebi8NodeID(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func (l *hebi8LineStore) SavePins(pins []string) error {
	if err := l.ensureDirs(); err != nil {
		return err
	}
	if pins == nil {
		pins = []string{}
	}
	if len(pins) > hebi8MaxPins {
		return errHebi8PinLimit
	}
	data, err := json.MarshalIndent(hebi8PinsFile{V: 1, Pins: pins}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(l.pinsPath, data, 0o600)
}

func (l *hebi8LineStore) LoadAll() ([]protocol.Hebi8LineNode, []string, error) {
	pins, err := l.LoadPins()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(l.nodesDir)
	if errors.Is(err, os.ErrNotExist) {
		return []protocol.Hebi8LineNode{}, pins, nil
	}
	if err != nil {
		return nil, nil, err
	}

	nodes := make([]protocol.Hebi8LineNode, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !isValidHebi8NodeID(id) {
			continue
		}
		raw, rErr := os.ReadFile(filepath.Join(l.nodesDir, name))
		if rErr != nil {
			return nil, nil, rErr
		}
		var n protocol.Hebi8LineNode
		if uErr := json.Unmarshal(raw, &n); uErr != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", name, uErr)
		}
		nodes = append(nodes, n)
	}

	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Seq != nodes[j].Seq {
			return nodes[i].Seq < nodes[j].Seq
		}
		if nodes[i].CreatedAtMs != nodes[j].CreatedAtMs {
			return nodes[i].CreatedAtMs < nodes[j].CreatedAtMs
		}
		return nodes[i].ID < nodes[j].ID
	})

	pinSet := map[string]bool{}
	for _, id := range pins {
		pinSet[id] = true
	}
	foldedInto := map[string]string{}
	for _, n := range nodes {
		if n.Kind == protocol.Hebi8NodeFold {
			for _, cid := range n.Covers {
				foldedInto[cid] = n.ID
			}
		}
	}
	for i := range nodes {
		nodes[i].Pinned = pinSet[nodes[i].ID]
		nodes[i].FoldedInto = foldedInto[nodes[i].ID]
	}
	return nodes, pins, nil
}

func (l *hebi8LineStore) ReadSummary(nodeID string) (string, error) {
	if !isValidHebi8NodeID(nodeID) {
		return "", fmt.Errorf("%w: invalid nodeId %q", errHebi8NodeNotFound, nodeID)
	}
	raw, err := os.ReadFile(l.nodeMDPath(nodeID))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", errHebi8NodeNotFound, nodeID)
	}
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// WriteImmutableNode atomically writes nodes/<id>.md and nodes/<id>.json, refusing to overwrite existing files.
func (l *hebi8LineStore) WriteImmutableNode(node protocol.Hebi8LineNode, summaryMD string) error {
	if !isValidHebi8NodeID(node.ID) {
		return fmt.Errorf("invalid hebi8 node id %q", node.ID)
	}
	if node.Kind == protocol.Hebi8NodeSegment && !strings.HasPrefix(node.ID, "seg_") {
		return fmt.Errorf("segment node id must start with seg_: %q", node.ID)
	}
	if node.Kind == protocol.Hebi8NodeFold && !strings.HasPrefix(node.ID, "fld_") {
		return fmt.Errorf("fold node id must start with fld_: %q", node.ID)
	}
	if err := l.ensureDirs(); err != nil {
		return err
	}

	jsonPath := l.nodeJSONPath(node.ID)
	mdPath := l.nodeMDPath(node.ID)
	if _, err := os.Stat(jsonPath); err == nil {
		return fmt.Errorf("%w: %s", errHebi8NodeImmutable, node.ID)
	}
	if _, err := os.Stat(mdPath); err == nil {
		return fmt.Errorf("%w: %s", errHebi8NodeImmutable, node.ID)
	}

	// Strip computed view-only fields before persisting immutable disk record.
	persisted := node
	persisted.V = 1
	persisted.Title = clampHebi8Title(persisted.Title)
	persisted.Pinned = false
	persisted.FoldedInto = ""
	persisted.Summary = ""

	jsonBytes, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	jsonBytes = append(jsonBytes, '\n')

	mdText := strings.TrimSpace(summaryMD) + "\n"
	if err := writeFileAtomicExclusive(mdPath, []byte(mdText), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomicExclusive(jsonPath, jsonBytes, 0o600); err != nil {
		_ = os.Remove(mdPath)
		return err
	}
	return nil
}

// SetPin pins or unpins nodeID in pins.json (enforcing max 8 pinned nodes).
func (l *hebi8LineStore) SetPin(nodeID string, pinned bool) ([]string, error) {
	if !isValidHebi8NodeID(nodeID) {
		return nil, fmt.Errorf("%w: %s", errHebi8NodeNotFound, nodeID)
	}
	if _, err := os.Stat(l.nodeJSONPath(nodeID)); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", errHebi8NodeNotFound, nodeID)
	}
	pins, err := l.LoadPins()
	if err != nil {
		return nil, err
	}
	idx := slices.Index(pins, nodeID)
	if pinned {
		if idx >= 0 {
			return pins, nil
		}
		if len(pins) >= hebi8MaxPins {
			return nil, errHebi8PinLimit
		}
		pins = append(pins, nodeID)
	} else {
		if idx < 0 {
			return pins, nil
		}
		pins = append(pins[:idx], pins[idx+1:]...)
	}
	if err := l.SavePins(pins); err != nil {
		return nil, err
	}
	return pins, nil
}

// FoldExcessLevels checks every level starting at 0; whenever >= 8 unfolded, unpinned nodes
// exist at the same level, the oldest 5 are merged from their FULL nodes/<id>.md originals
// into one immutable higher-level fold node.
func (l *hebi8LineStore) FoldExcessLevels(now time.Time) ([]protocol.Hebi8LineNode, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var created []protocol.Hebi8LineNode

	for pass := 0; pass < 16; pass++ {
		nodes, pins, err := l.LoadAll()
		if err != nil {
			return created, err
		}
		pinSet := map[string]bool{}
		for _, id := range pins {
			pinSet[id] = true
		}
		covered := map[string]bool{}
		maxSeq := 0
		maxLevel := 0
		for _, n := range nodes {
			if n.Seq > maxSeq {
				maxSeq = n.Seq
			}
			if n.Level > maxLevel {
				maxLevel = n.Level
			}
			if n.Kind == protocol.Hebi8NodeFold {
				for _, cid := range n.Covers {
					covered[cid] = true
				}
			}
		}

		foldedAnyInPass := false
		for level := 0; level <= maxLevel; level++ {
			var eligible []protocol.Hebi8LineNode
			for _, n := range nodes {
				if n.Level == level && !covered[n.ID] && !pinSet[n.ID] {
					eligible = append(eligible, n)
				}
			}
			if len(eligible) < hebi8FoldTriggerCount {
				continue
			}

			batch := eligible[:hebi8FoldBatchSize]
			childMDs := make([]string, len(batch))
			for i, child := range batch {
				md, rErr := l.ReadSummary(child.ID)
				if rErr != nil {
					return created, fmt.Errorf("read full child summary %s for fold: %w", child.ID, rErr)
				}
				childMDs[i] = strings.TrimSpace(md)
			}

			foldID, err := l.NextID(protocol.Hebi8NodeFold)
			if err != nil {
				return created, err
			}

			var fromMs, toMs int64
			var fromRFC, toRFC string
			var covers []string
			var tags []string
			tagSeen := map[string]bool{}
			highestChildLevel := level
			for _, child := range batch {
				covers = append(covers, child.ID)
				if child.Level > highestChildLevel {
					highestChildLevel = child.Level
				}
				cf := parseNodeTime(child.CoversFrom, child.CoversFromMs)
				ct := parseNodeTime(child.CoversTo, child.CoversToMs)
				if !cf.IsZero() && (fromMs == 0 || cf.UnixMilli() < fromMs) {
					fromMs = cf.UnixMilli()
					fromRFC = cf.UTC().Format(time.RFC3339)
				}
				if !ct.IsZero() && ct.UnixMilli() > toMs {
					toMs = ct.UnixMilli()
					toRFC = ct.UTC().Format(time.RFC3339)
				}
				for _, tg := range child.Tags {
					tg = strings.TrimSpace(tg)
					if tg != "" && !tagSeen[tg] && len(tags) < 12 {
						tagSeen[tg] = true
						tags = append(tags, tg)
					}
				}
			}
			if fromRFC == "" {
				fromMs = now.UnixMilli()
				fromRFC = now.UTC().Format(time.RFC3339)
			}
			if toRFC == "" {
				toMs = now.UnixMilli()
				toRFC = now.UTC().Format(time.RFC3339)
			}

			foldTitle, foldSummary := synthesizeFoldSummary(batch, childMDs, highestChildLevel+1)
			maxSeq++
			foldNode := protocol.Hebi8LineNode{
				V:              1,
				ID:             foldID,
				Kind:           protocol.Hebi8NodeFold,
				Seq:            maxSeq,
				CoversFrom:     fromRFC,
				CoversTo:       toRFC,
				CoversFromMs:   fromMs,
				CoversToMs:     toMs,
				Covers:         covers,
				Level:          highestChildLevel + 1,
				Title:          clampHebi8Title(foldTitle),
				Tags:           tags,
				SummaryQuality: protocol.Hebi8SummaryFallback,
				CreatedAt:      now.UTC().Format(time.RFC3339),
				CreatedAtMs:    now.UnixMilli(),
			}
			if err := l.WriteImmutableNode(foldNode, foldSummary); err != nil {
				return created, err
			}
			created = append(created, foldNode)
			foldedAnyInPass = true
			break // re-load and check again cleanly from level 0
		}
		if !foldedAnyInPass {
			break
		}
	}

	return created, nil
}

func summarizeLineState(nodes []protocol.Hebi8LineNode, pins []string, pendingTurns, pendingChars int) protocol.Hebi8LineSummary {
	sum := protocol.Hebi8LineSummary{
		TotalNodes:  len(nodes),
		PinnedCount: len(pins),
	}
	for _, n := range nodes {
		if n.Kind == protocol.Hebi8NodeSegment {
			sum.SegmentCount++
			if n.AttemptRange != nil && n.AttemptRange.EndAttemptNo > sum.LastFoldedAttemptNo {
				sum.LastFoldedAttemptNo = n.AttemptRange.EndAttemptNo
			}
		} else if n.Kind == protocol.Hebi8NodeFold {
			sum.FoldCount++
		}
		if n.Level > sum.MaxLevel {
			sum.MaxLevel = n.Level
		}
		if n.CreatedAt != "" {
			sum.LastFoldedAt = n.CreatedAt
		}
	}
	if pendingTurns >= hebi8SuggestFoldTurns || pendingChars >= hebi8SuggestFoldChars {
		sum.ShouldSuggestFold = true
	}
	return sum
}

func (s *Server) hebi8Line() *hebi8LineStore {
	cfg := s.hubConfigSnapshot()
	return newHebi8LineStore(cfg.DataDir)
}

func (s *Server) pickDefaultHebi8Adapter(ctx context.Context) protocol.Adapter {
	s.mu.RLock()
	machs := make([]string, 0, len(s.machines.Machines))
	for id, m := range s.machines.Machines {
		if m.Transport != protocol.TransportSSHOutbound {
			machs = append(machs, id)
		}
	}
	s.mu.RUnlock()
	sort.Strings(machs)
	for _, id := range machs {
		if reported := s.runners.ReportedAdapters(id); len(reported) > 0 {
			return reported[0]
		}
	}
	return protocol.AdapterAgy
}

func (s *Server) ensureHebi8MainTask(ctx context.Context) (protocol.TaskSummary, error) {
	task, ev, err := s.store.EnsureHebi8MainTask(ctx, s.pickDefaultHebi8Adapter(ctx))
	if err != nil {
		return task, err
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	return task, nil
}

func (s *Server) collectTaskRawTurns(ctx context.Context, taskID string, minAttemptNo, maxAttemptNo int) ([]protocol.Hebi8RawTurn, error) {
	rows, err := s.store.ListTaskAttemptsFullPrompts(ctx, taskID, minAttemptNo, maxAttemptNo)
	if err != nil {
		return nil, err
	}
	turns := make([]protocol.Hebi8RawTurn, 0, len(rows))
	for _, att := range rows {
		if !isTerminalAttemptStateHub(att.State) {
			continue
		}
		reply, rErr := s.collectAttemptAssistantText(ctx, att.AttemptID)
		if rErr != nil {
			return nil, fmt.Errorf("read assistant messages for attempt %s: %w", att.AttemptID, rErr)
		}
		turns = append(turns, protocol.Hebi8RawTurn{
			AttemptID:      att.AttemptID,
			AttemptNo:      att.AttemptNo,
			TriggerKind:    att.TriggerKind,
			State:          att.State,
			UserPrompt:     strings.TrimSpace(att.UserPrompt),
			AssistantReply: reply,
			StartedAt:      att.StartedAt,
			FinishedAt:     att.FinishedAt,
		})
	}
	return turns, nil
}

func (s *Server) collectAttemptAssistantText(ctx context.Context, attemptID string) (string, error) {
	const rawPageSize = 500
	var sb strings.Builder
	var afterSeq int64
	for {
		page, err := s.store.ListAttemptRawEventsByKinds(ctx, attemptID, []protocol.EventKind{protocol.EventACPMessageBatch}, afterSeq, rawPageSize)
		if err != nil {
			return "", err
		}
		if len(page) == 0 {
			break
		}
		progressed := false
		for _, ev := range page {
			if ev.Seq > afterSeq {
				afterSeq = ev.Seq
				progressed = true
			}
			if txt, ok := ev.Payload["text"].(string); ok && txt != "" {
				sb.WriteString(txt)
			}
		}
		if len(page) < rawPageSize || !progressed {
			break
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

func isTerminalAttemptStateHub(st protocol.AttemptState) bool {
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

// resolveLegacyCommittedNodeIDs reconstructs committed node IDs for an upgraded workspace
// where SQLite has Generation > 0 without an explicit CommittedNodeIDs manifest yet.
// Prefers committed hebi8.folded events (recursively expanding Covers chains), falling back
// to valid disk nodes whose AttemptRange.EndAttemptNo <= state.LastFoldedAttemptNo.
func (s *Server) resolveLegacyCommittedNodeIDs(
	ctx context.Context,
	taskID string,
	state store.Hebi8RolloverState,
	line *hebi8LineStore,
) ([]string, error) {
	if state.Generation == 0 || len(state.CommittedNodeIDs) > 0 {
		return state.CommittedNodeIDs, nil
	}
	diskNodes, _, err := line.LoadAll()
	if err != nil {
		return nil, err
	}
	if len(diskNodes) == 0 {
		return nil, nil
	}
	nodeMap := make(map[string]protocol.Hebi8LineNode, len(diskNodes))
	for _, n := range diskNodes {
		nodeMap[n.ID] = n
	}

	committedSet := map[string]bool{}
	var expandCovers func(id string)
	expandCovers = func(id string) {
		n, ok := nodeMap[id]
		if !ok || committedSet[id] {
			return
		}
		committedSet[id] = true
		for _, cid := range n.Covers {
			expandCovers(cid)
		}
	}

	// 1. Reconstruct from committed hebi8.folded events in SQLite (and their recursive Covers chains).
	eventIDs, err := s.store.ListHebi8FoldedEventNodeIDs(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for _, id := range eventIDs {
		expandCovers(id)
	}

	// 2. Always union all disk segments whose AttemptRange.EndAttemptNo is within LastFoldedAttemptNo.
	for _, n := range diskNodes {
		if n.Kind == protocol.Hebi8NodeSegment && n.AttemptRange != nil && n.AttemptRange.EndAttemptNo > 0 && n.AttemptRange.EndAttemptNo <= state.LastFoldedAttemptNo {
			committedSet[n.ID] = true
		}
	}

	// 3. Fixed-point closure: always include every disk fold node whose Covers children are all committed
	//    (preserving intermediate L1 folds even if a legacy hebi8.folded event only recorded L2).
	for progress := true; progress; {
		progress = false
		for _, n := range diskNodes {
			if n.Kind != protocol.Hebi8NodeFold || committedSet[n.ID] || len(n.Covers) == 0 {
				continue
			}
			allCovered := true
			for _, cid := range n.Covers {
				if !committedSet[cid] {
					allCovered = false
					break
				}
			}
			if allCovered {
				committedSet[n.ID] = true
				progress = true
			}
		}
	}

	out := make([]string, 0, len(committedSet))
	for _, n := range diskNodes {
		if committedSet[n.ID] {
			out = append(out, n.ID)
		}
	}
	return out, nil
}

func (s *Server) hydrateRolloverStateCommittedIDs(
	ctx context.Context,
	taskID string,
	state store.Hebi8RolloverState,
	line *hebi8LineStore,
) (store.Hebi8RolloverState, error) {
	if state.Generation > 0 && len(state.CommittedNodeIDs) == 0 {
		legacyIDs, err := s.resolveLegacyCommittedNodeIDs(ctx, taskID, state, line)
		if err != nil {
			return state, err
		}
		state.CommittedNodeIDs = legacyIDs
	}
	return state, nil
}

func (s *Server) handleHebi8Main(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.hebi8Mu.Lock()
	task, err := s.ensureHebi8MainTask(ctx)
	if err != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_MAIN", err.Error())
		return
	}
	dbState, stErr := s.store.GetHebi8RolloverState(ctx)
	if stErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_STATE", stErr.Error())
		return
	}
	line := s.hebi8Line()
	dbState, stErr = s.hydrateRolloverStateCommittedIDs(ctx, task.TaskID, dbState, line)
	if stErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", stErr.Error())
		return
	}
	nodes, pins, err := line.LoadCommitted(dbState)
	s.hebi8Mu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", err.Error())
		return
	}

	lastEndNo := dbState.LastFoldedAttemptNo

	pendingTurns, err := s.collectTaskRawTurns(ctx, task.TaskID, lastEndNo+1, 0)
	if err != nil {
		pendingTurns = nil
	}
	pendingChars := 0
	for _, t := range pendingTurns {
		pendingChars += runeLen(t.UserPrompt) + runeLen(t.AssistantReply)
	}
	lineSummary := summarizeLineState(nodes, pins, len(pendingTurns), pendingChars)

	writeJSON(w, http.StatusOK, protocol.Hebi8MainResponse{
		Task:                      task,
		LineSummary:               lineSummary,
		Nodes:                     nodes,
		Pins:                      pins,
		PendingTurnsSinceLastFold: len(pendingTurns),
		PendingCharsSinceLastFold: pendingChars,
	})
}

func (s *Server) handleHebi8Line(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.hebi8Mu.Lock()
	task, err := s.ensureHebi8MainTask(ctx)
	if err != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_MAIN", err.Error())
		return
	}
	dbState, stErr := s.store.GetHebi8RolloverState(ctx)
	if stErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_STATE", stErr.Error())
		return
	}
	line := s.hebi8Line()
	dbState, stErr = s.hydrateRolloverStateCommittedIDs(ctx, task.TaskID, dbState, line)
	if stErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", stErr.Error())
		return
	}
	nodes, pins, err := line.LoadCommitted(dbState)
	s.hebi8Mu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", err.Error())
		return
	}

	lastEndNo := dbState.LastFoldedAttemptNo
	pendingTurns, _ := s.collectTaskRawTurns(ctx, task.TaskID, lastEndNo+1, 0)
	pendingChars := 0
	for _, t := range pendingTurns {
		pendingChars += runeLen(t.UserPrompt) + runeLen(t.AssistantReply)
	}

	writeJSON(w, http.StatusOK, protocol.Hebi8LineResponse{
		TaskID:      task.TaskID,
		Nodes:       nodes,
		Pins:        pins,
		LineSummary: summarizeLineState(nodes, pins, len(pendingTurns), pendingChars),
	})
}

func (s *Server) handleHebi8Fold(w http.ResponseWriter, r *http.Request) {
	var req protocol.Hebi8FoldRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
			return
		}
	}
	ctx := r.Context()

	s.hebi8Mu.Lock()
	defer s.hebi8Mu.Unlock()

	task, err := s.ensureHebi8MainTask(ctx)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_MAIN", err.Error())
		return
	}
	if task.ActiveAttemptID != "" {
		writeAPIError(w, http.StatusConflict, "ERR_HEBI8_BUSY", "当前对话正在回复中，请等待本轮完成后再折叠换代")
		return
	}

	dbState, err := s.store.GetHebi8RolloverState(ctx)
	if err != nil {
		// Never run CleanupOrphans or mutate disk when rollover state cannot be decoded/loaded!
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_STATE", err.Error())
		return
	}

	line := s.hebi8Line()
	// If upgrading a legacy DB with Generation > 0 and empty CommittedNodeIDs, preserve all legacy committed nodes!
	var legacyCommittedIDs []string
	if dbState.Generation > 0 && len(dbState.CommittedNodeIDs) == 0 {
		legacyCommittedIDs, err = s.resolveLegacyCommittedNodeIDs(ctx, task.TaskID, dbState, line)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", err.Error())
			return
		}
		dbState.CommittedNodeIDs = legacyCommittedIDs
	}

	// Clean up any uncommitted crash-orphan files on disk before reading committed nodes or folding levels.
	line.CleanupOrphans(dbState)
	existingNodes, _, err := line.LoadCommitted(dbState)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", err.Error())
		return
	}
	maxSeq := 0
	for _, n := range existingNodes {
		if n.Seq > maxSeq {
			maxSeq = n.Seq
		}
	}
	// Authoritative watermark lives solely in SQLite hebi8_rollover_state.
	lastEndNo := dbState.LastFoldedAttemptNo

	// Step 1: Collect completed attempts since last fold
	turns, err := s.collectTaskRawTurns(ctx, task.TaskID, lastEndNo+1, 0)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_READ_TURNS", err.Error())
		return
	}
	if len(turns) == 0 {
		writeAPIError(w, http.StatusBadRequest, "ERR_NOTHING_TO_FOLD", "自上次折叠以来暂无可折叠的已完成对话轮次")
		return
	}

	tl, _ := s.store.GetTaskTimeline(ctx, task.TaskID, 0, false)
	var segAdapter protocol.Adapter
	var segModel string
	if len(tl.Attempts) > 0 {
		lastAtt := tl.Attempts[len(tl.Attempts)-1]
		segAdapter = lastAtt.Adapter
	}
	if tl.RuntimeConfig != nil {
		segModel = strings.TrimSpace(tl.RuntimeConfig["model"])
	}
	if segAdapter == "" {
		segAdapter = task.PreferredAdapter
	}

	now := time.Now()
	firstTurn := turns[0]
	lastTurn := turns[len(turns)-1]
	coversFromMs := firstTurn.StartedAt
	if coversFromMs <= 0 {
		coversFromMs = now.UnixMilli()
	}
	coversToMs := lastTurn.StartedAt
	if lastTurn.FinishedAt != nil && *lastTurn.FinishedAt > 0 {
		coversToMs = *lastTurn.FinishedAt
	}
	if coversToMs < coversFromMs {
		coversToMs = coversFromMs
	}

	segTitle, segTags, segSummaryMD, quality := synthesizeSegmentSummary(turns, req.Title, req.Summary, req.Tags, segAdapter)
	segID, err := line.NextID(protocol.Hebi8NodeSegment)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_ID", err.Error())
		return
	}

	// Track all files written in this fold call so any DB commit failure cleans them up cleanly.
	writtenIDs := make([]string, 0, 4)

	// Step 2: Persist immutable segment node
	segNode := protocol.Hebi8LineNode{
		V:            1,
		ID:           segID,
		Kind:         protocol.Hebi8NodeSegment,
		Seq:          maxSeq + 1,
		CoversFrom:   time.UnixMilli(coversFromMs).UTC().Format(time.RFC3339),
		CoversTo:     time.UnixMilli(coversToMs).UTC().Format(time.RFC3339),
		CoversFromMs: coversFromMs,
		CoversToMs:   coversToMs,
		AttemptRange: &protocol.Hebi8AttemptRange{
			StartAttemptID: firstTurn.AttemptID,
			EndAttemptID:   lastTurn.AttemptID,
			StartAttemptNo: firstTurn.AttemptNo,
			EndAttemptNo:   lastTurn.AttemptNo,
			TurnCount:      len(turns),
		},
		Level:          0,
		Title:          clampHebi8Title(segTitle),
		Tags:           segTags,
		Adapter:        segAdapter,
		Model:          segModel,
		SummaryQuality: quality,
		CreatedAt:      now.UTC().Format(time.RFC3339),
		CreatedAtMs:    now.UnixMilli(),
	}
	if err := line.WriteImmutableNode(segNode, segSummaryMD); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_WRITE_SEGMENT", err.Error())
		return
	}
	writtenIDs = append(writtenIDs, segNode.ID)

	// Step 3: On-demand hierarchical fold when >= 8 unfolded & unpinned nodes exist at the same level
	createdFolds, err := line.FoldExcessLevels(now)
	for _, cf := range createdFolds {
		writtenIDs = append(writtenIDs, cf.ID)
	}
	if err != nil {
		line.RemoveUncommittedNodes(writtenIDs)
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_FOLD_TREE", err.Error())
		return
	}

	evPayload := map[string]any{
		"segmentId":      segNode.ID,
		"title":          segNode.Title,
		"turnCount":      len(turns),
		"startAttemptId": firstTurn.AttemptID,
		"endAttemptId":   lastTurn.AttemptID,
		"startAttemptNo": firstTurn.AttemptNo,
		"endAttemptNo":   lastTurn.AttemptNo,
		"createdAt":      segNode.CreatedAt,
	}
	var primaryFold *protocol.Hebi8LineNode
	if len(createdFolds) > 0 {
		f := createdFolds[len(createdFolds)-1]
		primaryFold = &f
		evPayload["foldedId"] = f.ID
		evPayload["foldedCount"] = len(f.Covers)
	}

	// Step 4 & 5: Atomically advance rollover generation + committed node list + lock prior agent_sessions + append hebi8.folded
	// inside ONE BEGIN IMMEDIATE SQLite transaction. Any error rolls back both SQLite and disk node files!
	allToCommitIDs := make([]string, 0, len(legacyCommittedIDs)+len(writtenIDs))
	allToCommitIDs = append(allToCommitIDs, legacyCommittedIDs...)
	allToCommitIDs = append(allToCommitIDs, writtenIDs...)
	commitRes, err := s.store.CommitHebi8Fold(ctx, store.CommitHebi8FoldParams{
		TaskID:           task.TaskID,
		SegmentID:        segNode.ID,
		CommittedNodeIDs: allToCommitIDs,
		EndAttemptID:     lastTurn.AttemptID,
		EndAttemptNo:     lastTurn.AttemptNo,
		ExpectedPrevNo:   lastEndNo,
		EventPayload:     evPayload,
		BeforeCommitHook: s.getHebi8BeforeCommitHook(),
	})
	if err != nil {
		line.RemoveUncommittedNodes(writtenIDs)
		if errors.Is(err, store.ErrTaskActive) {
			writeAPIError(w, http.StatusConflict, "ERR_HEBI8_BUSY", "当前对话正在回复中，请等待本轮完成后再折叠换代")
			return
		}
		if errors.Is(err, store.ErrTaskArchived) {
			writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，无法折叠换代")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeAPIError(w, http.StatusConflict, "ERR_CONFLICT", err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_COMMIT", err.Error())
		return
	}

	// Broadcast committed timeline divider and close old live ACP sessions on connected runners.
	s.broker.Broadcast(commitRes.Event)
	for _, cs := range commitRes.ClosedSessions {
		if cs.MachineID != "" && cs.WBSessionID != "" && s.runners.IsConnected(cs.MachineID) {
			_ = s.runners.CloseSession(cs.MachineID, protocol.SessionCloseFrame{
				Type:              protocol.RunnerMsgSessionClose,
				WBSessionID:       cs.WBSessionID,
				ProviderSessionID: cs.ProviderSessionID,
			})
		}
	}

	allNodes, pins, err := line.LoadCommitted(commitRes.State)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_RELOAD", err.Error())
		return
	}

	// Attach summary on returned segment/fold objects for immediate UI convenience
	segResp := segNode
	segResp.Summary = segSummaryMD
	if primaryFold != nil {
		if md, mErr := line.ReadSummary(primaryFold.ID); mErr == nil {
			primaryFold.Summary = md
		}
	}

	writeJSON(w, http.StatusCreated, protocol.Hebi8FoldResponse{
		TaskID:      task.TaskID,
		Segment:     segResp,
		Fold:        primaryFold,
		Folds:       createdFolds,
		Nodes:       allNodes,
		Pins:        pins,
		LineSummary: summarizeLineState(allNodes, pins, 0, 0),
	})
}

func (s *Server) handleHebi8Expand(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(chi.URLParam(r, "nodeId"))
	if !isValidHebi8NodeID(nodeID) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_NODE_ID", "invalid nodeId format")
		return
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 {
			offset = v
		}
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = min(v, 100)
		}
	}

	ctx := r.Context()
	s.hebi8Mu.Lock()
	task, err := s.ensureHebi8MainTask(ctx)
	if err != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_MAIN", err.Error())
		return
	}
	dbState, stErr := s.store.GetHebi8RolloverState(ctx)
	if stErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_STATE", stErr.Error())
		return
	}
	line := s.hebi8Line()
	dbState, stErr = s.hydrateRolloverStateCommittedIDs(ctx, task.TaskID, dbState, line)
	if stErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", stErr.Error())
		return
	}
	nodes, _, err := line.LoadCommitted(dbState)
	s.hebi8Mu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", err.Error())
		return
	}

	nodeMap := make(map[string]protocol.Hebi8LineNode, len(nodes))
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}
	target, ok := nodeMap[nodeID]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "ERR_HEBI8_NODE_NOT_FOUND", "node not found: "+nodeID)
		return
	}
	summaryMD, err := line.ReadSummary(nodeID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_READ_SUMMARY", err.Error())
		return
	}

	resp := protocol.Hebi8ExpandResponse{
		Node:    target,
		Summary: summaryMD,
	}

	if target.Kind == protocol.Hebi8NodeFold {
		children := make([]protocol.Hebi8LineNode, 0, len(target.Covers))
		for _, cid := range target.Covers {
			if cn, found := nodeMap[cid]; found {
				if cmd, cErr := line.ReadSummary(cid); cErr == nil {
					cn.Summary = cmd
				}
				children = append(children, cn)
			}
		}
		resp.Children = children
	} else if target.Kind == protocol.Hebi8NodeSegment && target.AttemptRange != nil {
		turns, tErr := s.collectTaskRawTurns(ctx, task.TaskID, target.AttemptRange.StartAttemptNo, target.AttemptRange.EndAttemptNo)
		if tErr != nil {
			writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_EXPAND_TURNS", tErr.Error())
			return
		}
		resp.TotalTurns = len(turns)
		resp.Offset = offset
		resp.Limit = limit
		if offset < len(turns) {
			end := min(len(turns), offset+limit)
			resp.Turns = turns[offset:end]
		} else {
			resp.Turns = []protocol.Hebi8RawTurn{}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHebi8Pin(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(chi.URLParam(r, "nodeId"))
	if !isValidHebi8NodeID(nodeID) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_NODE_ID", "invalid nodeId format")
		return
	}
	var req protocol.Hebi8PinRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	s.hebi8Mu.Lock()
	dbState, stErr := s.store.GetHebi8RolloverState(r.Context())
	if stErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_STATE", stErr.Error())
		return
	}
	line := s.hebi8Line()
	if mainTask, mErr := s.ensureHebi8MainTask(r.Context()); mErr == nil {
		if hydrated, hErr := s.hydrateRolloverStateCommittedIDs(r.Context(), mainTask.TaskID, dbState, line); hErr == nil {
			dbState = hydrated
		}
	}
	committedNodes, _, lErr := line.LoadCommitted(dbState)
	if lErr != nil {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_LINE_LOAD", lErr.Error())
		return
	}
	foundCommitted := false
	for _, n := range committedNodes {
		if n.ID == nodeID {
			foundCommitted = true
			break
		}
	}
	if !foundCommitted {
		s.hebi8Mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "ERR_HEBI8_NODE_NOT_FOUND", fmt.Sprintf("%v: %s", errHebi8NodeNotFound, nodeID))
		return
	}
	pins, err := line.SetPin(nodeID, req.Pinned)
	s.hebi8Mu.Unlock()
	if errors.Is(err, errHebi8NodeNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_HEBI8_NODE_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, errHebi8PinLimit) {
		writeAPIError(w, http.StatusBadRequest, "ERR_HEBI8_PIN_LIMIT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_HEBI8_PIN", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, protocol.Hebi8PinResponse{
		NodeID: nodeID,
		Pinned: req.Pinned,
		Pins:   pins,
	})
}

// buildHebi8FoldOpeningForAttempt builds the deterministic 6-part Fold-Rollover opening when
// the Hebi8 main task starts its fresh ACP session turn after a fold.
// Returns ("", false, nil) if no fold opening should be injected for this attempt.
// Propagates errors when required committed node summaries or historical turns cannot be read,
// so a mutilated opening is never silently dispatched.
func (s *Server) buildHebi8FoldOpeningForAttempt(ctx context.Context, att store.AttemptRecord) (string, bool, error) {
	if att.ProjectID != protocol.PAProjectID {
		return "", false, nil
	}
	mainTaskID, err := s.store.GetHebi8MainTaskID(ctx)
	if err != nil {
		return "", false, fmt.Errorf("lookup hebi8 main task id: %w", err)
	}
	if mainTaskID == "" || mainTaskID != att.TaskID {
		return "", false, nil
	}

	dbState, err := s.store.GetHebi8RolloverState(ctx)
	if err != nil {
		return "", false, fmt.Errorf("load hebi8 rollover state: %w", err)
	}

	// Prefer the generation boundary atomically stamped into TriggerPayloadJSON by createAttemptInTx,
	// falling back to DB rollover state for legacy attempts.
	var stampedGen int64
	var stampedLastEndNo int
	if strings.TrimSpace(att.TriggerPayloadJSON) != "" {
		var tp map[string]any
		if json.Unmarshal([]byte(att.TriggerPayloadJSON), &tp) == nil {
			if v, ok := tp["hebi8OpeningGeneration"].(float64); ok && v > 0 {
				stampedGen = int64(v)
			}
			if v, ok := tp["hebi8LastFoldedAttemptNo"].(float64); ok && v > 0 {
				stampedLastEndNo = int(v)
			}
		}
	}

	lastEndNo := dbState.LastFoldedAttemptNo
	if stampedLastEndNo > 0 {
		lastEndNo = stampedLastEndNo
	}
	if lastEndNo == 0 || att.AttemptNo <= lastEndNo {
		return "", false, nil
	}

	if stampedGen == 0 && att.AttemptNo > lastEndNo+1 {
		if strings.TrimSpace(att.ProviderSessionID) != "" {
			return "", false, nil
		}
		postFoldDone, tErr := s.collectTaskRawTurns(ctx, att.TaskID, lastEndNo+1, att.AttemptNo-1)
		if tErr != nil {
			return "", false, fmt.Errorf("collect post-fold turns: %w", tErr)
		}
		for _, pt := range postFoldDone {
			if pt.State == protocol.AttemptSucceeded {
				return "", false, nil
			}
		}
	}

	s.hebi8Mu.Lock()
	line := s.hebi8Line()
	dbState, err = s.hydrateRolloverStateCommittedIDs(ctx, att.TaskID, dbState, line)
	if err != nil {
		s.hebi8Mu.Unlock()
		return "", false, fmt.Errorf("hydrate committed hebi8 nodes: %w", err)
	}
	nodes, pins, err := line.LoadCommitted(dbState)
	if err != nil {
		s.hebi8Mu.Unlock()
		return "", false, fmt.Errorf("load committed hebi8 nodes: %w", err)
	}
	if len(nodes) == 0 {
		s.hebi8Mu.Unlock()
		return "", false, fmt.Errorf("hebi8 rollover state references lastFoldedAttemptNo=%d but no committed nodes were found", lastEndNo)
	}
	summaries := make(map[string]string, len(nodes))
	for _, n := range nodes {
		md, mErr := line.ReadSummary(n.ID)
		if mErr != nil {
			s.hebi8Mu.Unlock()
			return "", false, fmt.Errorf("read committed hebi8 node summary %s: %w", n.ID, mErr)
		}
		summaries[n.ID] = md
	}
	s.hebi8Mu.Unlock()

	// Gather raw turns up through lastEndNo from SQLite for Section 5 (anti-distortion verbatim layer)
	histTurns, tErr := s.collectTaskRawTurns(ctx, att.TaskID, 1, lastEndNo)
	if tErr != nil {
		return "", false, fmt.Errorf("collect hebi8 history turns: %w", tErr)
	}
	var recentUserPrompts []string
	for _, t := range histTurns {
		if t.TriggerKind == protocol.TriggerChildResult {
			continue
		}
		if p := strings.TrimSpace(t.UserPrompt); p != "" {
			recentUserPrompts = append(recentUserPrompts, p)
		}
	}
	if len(recentUserPrompts) > 4 {
		recentUserPrompts = recentUserPrompts[len(recentUserPrompts)-4:]
	}
	var tailTurns []protocol.Hebi8RawTurn
	if len(histTurns) > 2 {
		tailTurns = histTurns[len(histTurns)-2:]
	} else {
		tailTurns = histTurns
	}
	// If the last 2 turns are all automatic child_result wakeups, retain the most recent genuine
	// user turn alongside the latest turn so the verbatim tail still preserves real user Q&A.
	if len(tailTurns) > 0 {
		allChildResults := true
		for _, t := range tailTurns {
			if t.TriggerKind != protocol.TriggerChildResult {
				allChildResults = false
				break
			}
		}
		if allChildResults {
			for i := len(histTurns) - 1; i >= 0; i-- {
				if histTurns[i].TriggerKind != protocol.TriggerChildResult {
					latest := histTurns[len(histTurns)-1]
					tailTurns = []protocol.Hebi8RawTurn{histTurns[i], latest}
					break
				}
			}
		}
	}

	opening := BuildHebi8FoldOpening(Hebi8FoldOpeningInput{
		WorkspaceName:     s.effectiveWorkspaceName(),
		Now:               time.Now(),
		Nodes:             nodes,
		Summaries:         summaries,
		Pins:              pins,
		WorkspaceSnapshot: s.buildPAWorkspaceSnapshot(ctx),
		RecentUserPrompts: recentUserPrompts,
		TailTurns:         tailTurns,
		MaxRunes:          DefaultHebi8OpeningMaxRunes,
	})
	return opening, true, nil
}

func synthesizeSegmentSummary(
	turns []protocol.Hebi8RawTurn,
	reqTitle, reqSummary string,
	reqTags []string,
	adapter protocol.Adapter,
) (title string, tags []string, summaryMD string, quality protocol.Hebi8SummaryQuality) {
	title = clampHebi8Title(strings.TrimSpace(reqTitle))
	if title == "" {
		title = deriveTitleFromTurns(turns)
	}
	tags = normalizeTags(reqTags)
	if len(tags) == 0 {
		tags = deriveTagsFromTurns(turns, adapter)
	}

	if trimmed := strings.TrimSpace(reqSummary); trimmed != "" {
		return title, tags, trimmed, protocol.Hebi8SummaryWritten
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("在这一段里，我与你进行了 %d 轮对话（第 #%d～#%d 轮），主要围绕「%s」展开：\n\n",
		len(turns), turns[0].AttemptNo, turns[len(turns)-1].AttemptNo, title))
	for _, t := range turns {
		u := strings.TrimSpace(t.UserPrompt)
		a := strings.TrimSpace(t.AssistantReply)
		if u == "" && a == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("- **第 %d 轮**：", t.AttemptNo))
		if u != "" {
			sb.WriteString("你提到：" + trimMetadataSnippet(collapseSpaces(u), 240))
		}
		if a != "" {
			if u != "" {
				sb.WriteString("；")
			}
			sb.WriteString("我回复：" + trimMetadataSnippet(collapseSpaces(a), 420))
		} else if t.State != protocol.AttemptSucceeded {
			sb.WriteString(fmt.Sprintf("（该轮状态：%s）", t.State))
		}
		sb.WriteString("\n")
	}
	return title, tags, strings.TrimSpace(sb.String()), protocol.Hebi8SummaryFallback
}

func synthesizeFoldSummary(batch []protocol.Hebi8LineNode, childMDs []string, level int) (string, string) {
	firstTitle := strings.TrimSpace(batch[0].Title)
	lastTitle := strings.TrimSpace(batch[len(batch)-1].Title)
	title := fmt.Sprintf("阶段合集（L%d · %d 节点）：%s", level, len(batch), firstTitle)
	if lastTitle != "" && lastTitle != firstTitle {
		title = fmt.Sprintf("L%d 合集：%s → %s", level, trimMetadataSnippet(firstTitle, 20), trimMetadataSnippet(lastTitle, 22))
	}
	title = clampHebi8Title(title)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("我已将以下 %d 个较早的历史节点（%s）合并归档为 L%d 阶段摘要，保留各节点核心脉络与结论：\n\n",
		len(batch),
		formatDateSpan(batch[0].CoversFrom, batch[len(batch)-1].CoversTo, batch[0].CoversFromMs, batch[len(batch)-1].CoversToMs),
		level,
	))
	for i, child := range batch {
		span := formatDateSpan(child.CoversFrom, child.CoversTo, child.CoversFromMs, child.CoversToMs)
		sb.WriteString(fmt.Sprintf("### %s（%s）· %s\n", child.ID, span, strings.TrimSpace(child.Title)))
		if body := strings.TrimSpace(childMDs[i]); body != "" {
			// Preserve full child nodes/<id>.md summary verbatim during higher-level fold synthesis;
			// opening display budgets are enforced separately inside BuildHebi8FoldOpening.
			sb.WriteString(body + "\n\n")
		}
	}
	return title, strings.TrimSpace(sb.String())
}

func deriveTitleFromTurns(turns []protocol.Hebi8RawTurn) string {
	for _, t := range turns {
		p := strings.TrimSpace(t.UserPrompt)
		if p == "" {
			continue
		}
		if nl := strings.IndexByte(p, '\n'); nl > 0 {
			p = strings.TrimSpace(p[:nl])
		}
		p = collapseSpaces(p)
		if p != "" {
			return clampHebi8Title(p)
		}
	}
	return fmt.Sprintf("对话片段 #%d-#%d", turns[0].AttemptNo, turns[len(turns)-1].AttemptNo)
}

func deriveTagsFromTurns(turns []protocol.Hebi8RawTurn, adapter protocol.Adapter) []string {
	var tags []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" || seen[t] || len(tags) >= 6 {
			return
		}
		seen[t] = true
		tags = append(tags, t)
	}
	if adapter != "" {
		add(string(adapter))
	}
	keywords := []struct {
		needle string
		tag    string
	}{
		{"deploy", "deploy"},
		{"部署", "deploy"},
		{"bug", "debug"},
		{"报错", "debug"},
		{"排查", "debug"},
		{"测试", "test"},
		{"test", "test"},
		{"重构", "refactor"},
		{"架构", "arch"},
		{"设计", "design"},
		{"计划", "plan"},
		{"优先级", "planning"},
		{"复盘", "review"},
		{"review", "review"},
		{"worktree", "worktree"},
		{"acp", "acp"},
	}
	var corpus strings.Builder
	for _, t := range turns {
		corpus.WriteString(strings.ToLower(t.UserPrompt))
		corpus.WriteByte('\n')
		corpus.WriteString(strings.ToLower(trimMetadataSnippet(t.AssistantReply, 300)))
		corpus.WriteByte('\n')
	}
	text := corpus.String()
	for _, kw := range keywords {
		if strings.Contains(text, kw.needle) {
			add(kw.tag)
		}
	}
	if len(tags) == 0 {
		add("hebi8")
	}
	return tags
}

func normalizeTags(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			t := strings.TrimSpace(part)
			if t == "" || seen[strings.ToLower(t)] {
				continue
			}
			seen[strings.ToLower(t)] = true
			out = append(out, t)
			if len(out) >= 10 {
				return out
			}
		}
	}
	return out
}

func clampHebi8Title(s string) string {
	s = collapseSpaces(strings.TrimSpace(s))
	if s == "" {
		return "未命名片段"
	}
	if runeLen(s) <= hebi8TitleMaxRunes {
		return s
	}
	return trimMetadataSnippet(s, hebi8TitleMaxRunes)
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// writeFileAtomicExclusive writes data via temp file and links/renames only if destination doesn't exist.
func writeFileAtomicExclusive(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errHebi8NodeImmutable
		}
		// Fallback on filesystems without hardlinks: check existence then rename
		if _, statErr := os.Stat(path); statErr == nil {
			return errHebi8NodeImmutable
		}
		return os.Rename(tmpName, path)
	}
	return nil
}
