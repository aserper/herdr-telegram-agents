package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

const kindPi = "pi"

// piProjectSlug mirrors Pi's readable session-directory encoding. The cwd is
// framed with two dashes and path separators (plus Windows's drive colon) are
// replaced with dashes. The session header remains authoritative because this
// encoding is intentionally non-injective.
func piProjectSlug(cwd string) string {
	clean, err := normalizePiCwd(cwd)
	if err != nil {
		clean = filepath.Clean(cwd)
	}
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, `\`) {
		clean = clean[1:]
	}
	clean = strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(clean)
	return "--" + clean + "--"
}

func normalizePiCwd(cwd string) (string, error) {
	clean := filepath.Clean(cwd)
	windowsAbsolute := len(clean) >= 3 && ((clean[0] >= 'A' && clean[0] <= 'Z') || (clean[0] >= 'a' && clean[0] <= 'z')) && clean[1] == ':' && (clean[2] == '\\' || clean[2] == '/')
	if !filepath.IsAbs(clean) && filepath.VolumeName(clean) == "" && !windowsAbsolute {
		absolute, err := filepath.Abs(clean)
		if err != nil {
			return "", err
		}
		clean = absolute
	}
	return clean, nil
}

func (r *Reader) lastPiReply(ctx context.Context, home string, agent domain.Agent) (domain.Reply, error) {
	if err := ctx.Err(); err != nil {
		return domain.Reply{}, err
	}
	normalizedCwd, err := normalizePiCwd(agent.Cwd)
	if err != nil {
		return domain.Reply{}, fmt.Errorf("%w: resolve pi cwd %s: %v", domain.ErrNoReply, agent.Cwd, err)
	}
	agent.Cwd = normalizedCwd
	dir := filepath.Join(home, ".pi", "agent", "sessions", piProjectSlug(normalizedCwd))
	path, modTime, candidates, err := piTranscriptPath(agent, dir)
	if err != nil {
		return domain.Reply{}, err
	}
	age := r.now().Sub(modTime)
	r.log.Debug("pi transcript lookup",
		slog.String("pane", agent.PaneID), slog.String("cwd", agent.Cwd), slog.String("dir", dir),
		slog.Int("candidates", candidates), slog.String("chosen", filepath.Base(path)), slog.Int64("age_ms", age.Milliseconds()))
	text, meta, stats, err := lastPiReplyIn(path, r.maxScan)
	r.log.Debug("pi transcript scanned",
		slog.String("chosen", filepath.Base(path)), slog.Int("lines", stats.lines),
		slog.Int64("bytes", stats.bytes), slog.Int("skipped_json", stats.skipped), slog.Bool("found", err == nil),
		slog.String("model", meta.Model), slog.Int("output_tokens", meta.OutputTokens))
	if err != nil {
		return domain.Reply{}, err
	}
	return domain.Reply{Text: text, Source: path, Age: age, Written: modTime, Meta: meta}, nil
}

// PendingQuestion returns a Pi ask_user request only while it is the active
// unresolved tool call. It deliberately declines multi-select and explicitly
// free-form/comment forms. Pi's JSONL serializer drops explicit false values,
// so absent boolean flags are treated as false; only the listed option actions
// are exposed to Telegram.
func (r *Reader) PendingQuestion(ctx context.Context, agent domain.Agent) (domain.Question, error) {
	if err := ctx.Err(); err != nil {
		return domain.Question{}, err
	}
	if agent.Kind != kindPi {
		return domain.Question{}, fmt.Errorf("%w: unsupported agent %q", domain.ErrNoQuestion, agent.Kind)
	}
	if strings.TrimSpace(agent.Cwd) == "" {
		return domain.Question{}, fmt.Errorf("%w: agent has no working directory", domain.ErrNoQuestion)
	}
	home, err := r.home()
	if err != nil {
		return domain.Question{}, fmt.Errorf("%w: home directory unavailable", domain.ErrNoQuestion)
	}
	normalizedCwd, err := normalizePiCwd(agent.Cwd)
	if err != nil {
		return domain.Question{}, fmt.Errorf("%w: resolve pi cwd", domain.ErrNoQuestion)
	}
	agent.Cwd = normalizedCwd
	dir := filepath.Join(home, ".pi", "agent", "sessions", piProjectSlug(normalizedCwd))
	path, _, candidates, err := piTranscriptPath(agent, dir)
	if err != nil {
		return domain.Question{}, fmt.Errorf("%w: transcript unavailable", domain.ErrNoQuestion)
	}
	question, stats, err := pendingPiQuestionIn(path, r.maxScan)
	r.log.Debug("pi question scanned", slog.String("pane", agent.PaneID), slog.String("chosen", filepath.Base(path)),
		slog.Int("candidates", candidates), slog.Int("lines", stats.lines), slog.Int64("bytes", stats.bytes),
		slog.Bool("found", err == nil))
	return question, err
}

// PendingActivity returns a safe snapshot of what a root Pi agent is doing
// right now: the tool it is waiting on, the tools it finished in the
// current turn and the subagent runs its Agent and subagent tool calls
// produced. It follows the active parent chain of the root session only
// and the labels it returns are generic: tool names plus the structured
// description, kind and status fields of subagent tool results, never
// commands, file paths or tool arguments. Every failure is
// domain.ErrNoActivity wrapped with the reason.
func (r *Reader) PendingActivity(ctx context.Context, agent domain.Agent) (domain.ActivitySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.ActivitySnapshot{}, err
	}
	if agent.Kind != kindPi {
		return domain.ActivitySnapshot{}, fmt.Errorf("%w: unsupported agent %q", domain.ErrNoActivity, agent.Kind)
	}
	if strings.TrimSpace(agent.Cwd) == "" {
		return domain.ActivitySnapshot{}, fmt.Errorf("%w: agent has no working directory", domain.ErrNoActivity)
	}
	home, err := r.home()
	if err != nil {
		return domain.ActivitySnapshot{}, fmt.Errorf("%w: home directory unavailable", domain.ErrNoActivity)
	}
	normalizedCwd, err := normalizePiCwd(agent.Cwd)
	if err != nil {
		return domain.ActivitySnapshot{}, fmt.Errorf("%w: resolve pi cwd", domain.ErrNoActivity)
	}
	agent.Cwd = normalizedCwd
	dir := filepath.Join(home, ".pi", "agent", "sessions", piProjectSlug(normalizedCwd))
	path, _, candidates, err := piTranscriptPath(agent, dir)
	if err != nil {
		return domain.ActivitySnapshot{}, fmt.Errorf("%w: transcript unavailable", domain.ErrNoActivity)
	}
	snapshot, stats, err := pendingPiActivityIn(path, r.maxScan)
	r.log.Debug("pi activity scanned", slog.String("pane", agent.PaneID), slog.String("chosen", filepath.Base(path)),
		slog.Int("candidates", candidates), slog.Int("lines", stats.lines), slog.Int64("bytes", stats.bytes),
		slog.Bool("found", err == nil))
	return snapshot, err
}

func piTranscriptPath(agent domain.Agent, fallbackDir string) (path string, modTime time.Time, candidates int, err error) {
	if agent.SessionPath == "" {
		return newestPiTranscript(fallbackDir, agent.Cwd)
	}
	info, statErr := os.Lstat(agent.SessionPath)
	if statErr != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: stat exact pi transcript %s: %v", domain.ErrNoReply, agent.SessionPath, statErr)
	}
	if !info.Mode().IsRegular() {
		return "", time.Time{}, 0, fmt.Errorf("%w: exact pi transcript is not a regular file: %s", domain.ErrNoReply, agent.SessionPath)
	}
	matched, matchErr := piTranscriptMatchesCwd(agent.SessionPath, filepath.Clean(agent.Cwd))
	if matchErr != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: inspect exact pi transcript %s: %v", domain.ErrNoReply, agent.SessionPath, matchErr)
	}
	if !matched {
		return "", time.Time{}, 0, fmt.Errorf("%w: exact pi transcript cwd does not match %s", domain.ErrNoReply, agent.Cwd)
	}
	return agent.SessionPath, info.ModTime(), 1, nil
}

type piSessionHeader struct {
	Type          string `json:"type"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession"`
}

// newestPiTranscript chooses the newest session whose header names the exact
// cwd. Pi's directory encoding can map different paths to the same directory.
func newestPiTranscript(dir, cwd string) (path string, modTime time.Time, candidates int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", time.Time{}, 0, fmt.Errorf("%w: no transcript directory %s", domain.ErrNoReply, dir)
		}
		return "", time.Time{}, 0, fmt.Errorf("%w: read %s: %v", domain.ErrNoReply, dir, err)
	}
	want, err := normalizePiCwd(cwd)
	if err != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: resolve pi cwd %s: %v", domain.ErrNoReply, cwd, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), transcriptSuffix) {
			continue
		}
		candidate := filepath.Join(dir, entry.Name())
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		matched, headerErr := piTranscriptMatchesCwd(candidate, want)
		if headerErr != nil || !matched {
			continue
		}
		candidates++
		if path == "" || info.ModTime().After(modTime) {
			path, modTime = candidate, info.ModTime()
		}
	}
	if path == "" {
		return "", time.Time{}, candidates, fmt.Errorf("%w: no transcript for cwd %s in %s", domain.ErrNoReply, cwd, dir)
	}
	return path, modTime, candidates, nil
}

func piTranscriptMatchesCwd(path, cwd string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	line, err := bufio.NewReader(io.LimitReader(file, 64<<10)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	var header piSessionHeader
	if err := json.Unmarshal(bytes.TrimSpace(line), &header); err != nil {
		return false, err
	}
	return header.Type == "session" && filepath.Clean(header.Cwd) == cwd && header.ParentSession == "", nil
}

type piScanStats struct {
	lines   int
	bytes   int64
	skipped int
}

type piRecord struct {
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type piMessage struct {
	Role       string           `json:"role"`
	Content    []piContentBlock `json:"content"`
	Model      string           `json:"model"`
	StopReason string           `json:"stopReason"`
	ToolName   string           `json:"toolName"`
	Details    json.RawMessage  `json:"details"`
	ToolCallID string           `json:"toolCallId"`
	IsError    bool             `json:"isError"`
	Usage      struct {
		Output int `json:"output"`
	} `json:"usage"`
}

type piContentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type piTaskDetails struct {
	Todos []domain.ReplyTask `json:"todos"`
}

type piAskUserArgs struct {
	Question      string `json:"question"`
	Context       string `json:"context"`
	AllowMultiple *bool  `json:"allowMultiple"`
	AllowFreeform *bool  `json:"allowFreeform"`
	AllowComment  *bool  `json:"allowComment"`
	Options       []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"options"`
}

// latestPiTasks finds the newest task-list result on the active branch. The
// list may predate the latest prompt because Pi keeps it across turns.
func latestPiTasks(records map[string]piRecord, leaf string) []domain.ReplyTask {
	visited := make(map[string]struct{})
	for id := leaf; id != ""; {
		if _, seen := visited[id]; seen {
			return nil
		}
		visited[id] = struct{}{}
		record, ok := records[id]
		if !ok {
			return nil
		}
		if record.Type == "message" {
			var message piMessage
			if json.Unmarshal(record.Message, &message) == nil && message.Role == "toolResult" && message.ToolName == "manage_todo_list" {
				var details piTaskDetails
				if json.Unmarshal(message.Details, &details) != nil {
					return nil
				}
				tasks := make([]domain.ReplyTask, 0, len(details.Todos))
				for _, task := range details.Todos {
					task.Title = strings.TrimSpace(task.Title)
					if task.Title != "" {
						tasks = append(tasks, task)
					}
				}
				return tasks
			}
		}
		if record.ParentID == nil {
			return nil
		}
		id = *record.ParentID
	}
	return nil
}

func pendingPiQuestionIn(path string, maxScan int64) (domain.Question, piScanStats, error) {
	data, stats, err := readTail(path, maxScan)
	if err != nil {
		return domain.Question{}, stats, fmt.Errorf("%w: read pi transcript", domain.ErrNoQuestion)
	}
	records := make(map[string]piRecord)
	var leaf string
	lines := bytes.Split(data, []byte{'\n'})
	unterminated := len(data) > 0 && data[len(data)-1] != '\n'
	for index, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		stats.lines++
		var record piRecord
		if err := json.Unmarshal(line, &record); err != nil {
			if unterminated && index == len(lines)-1 {
				return domain.Question{}, stats, fmt.Errorf("%w: partial record", domain.ErrNoQuestion)
			}
			stats.skipped++
			continue
		}
		if record.ID != "" {
			records[record.ID] = record
			leaf = record.ID
		}
	}
	visited := make(map[string]struct{})
	for id := leaf; id != ""; {
		if _, seen := visited[id]; seen {
			return domain.Question{}, stats, fmt.Errorf("%w: parent cycle", domain.ErrNoQuestion)
		}
		visited[id] = struct{}{}
		record, ok := records[id]
		if !ok {
			return domain.Question{}, stats, fmt.Errorf("%w: chain begins before scan window", domain.ErrNoQuestion)
		}
		if record.Type == "message" {
			var message piMessage
			if json.Unmarshal(record.Message, &message) == nil {
				// A result after a tool call proves it is no longer pending.
				if message.Role == "toolResult" && message.ToolName == "ask_user" {
					return domain.Question{}, stats, fmt.Errorf("%w: ask_user already resolved", domain.ErrNoQuestion)
				}
				if message.Role == "assistant" {
					for _, content := range message.Content {
						if content.Type != "toolCall" || content.Name != "ask_user" {
							continue
						}
						var args piAskUserArgs
						if json.Unmarshal(content.Arguments, &args) != nil || (args.AllowMultiple != nil && *args.AllowMultiple) || (args.AllowFreeform != nil && *args.AllowFreeform) || (args.AllowComment != nil && *args.AllowComment) || strings.TrimSpace(args.Question) == "" || len(args.Options) < 2 || len(args.Options) > domain.MaxChoiceButtons {
							return domain.Question{}, stats, fmt.Errorf("%w: unsupported ask_user form", domain.ErrNoQuestion)
						}
						if content.ID == "" {
							return domain.Question{}, stats, fmt.Errorf("%w: ask_user call has no id", domain.ErrNoQuestion)
						}
						question := domain.Question{ID: content.ID, Prompt: args.Question, Context: args.Context, Options: make([]domain.QuestionOption, 0, len(args.Options))}
						for index, option := range args.Options {
							title := strings.TrimSpace(option.Title)
							if title == "" {
								return domain.Question{}, stats, fmt.Errorf("%w: empty option", domain.ErrNoQuestion)
							}
							question.Options = append(question.Options, domain.QuestionOption{Number: index + 1, Title: title, Description: option.Description})
						}
						return question, stats, nil
					}
				}
			}
		}
		if record.ParentID == nil {
			break
		}
		id = *record.ParentID
	}
	return domain.Question{}, stats, fmt.Errorf("%w: no active ask_user", domain.ErrNoQuestion)
}

// Bounds for what an activity snapshot shows: both lists keep their newest
// entries, and every label is capped so a card stays short.
const (
	maxActivityTools       = 5
	maxActivitySubagents   = 5
	maxActivityNameLabel   = 32
	maxActivityStatusLabel = 24
	maxActivityDescription = 96
)

// piSubagentTools are the tool names Pi has used to run subagents. Their
// results carry the structured description, kind and status fields an
// activity card shows; a still-running call is visible through the active
// tool label instead of a second row.
var piSubagentTools = map[string]struct{}{"Agent": {}, "subagent": {}}

// pendingPiActivityIn walks the active parent chain of a root Pi session
// from its newest record backwards and collects the current activity. A
// tool call is pending while no result carrying its id has been seen; the
// walk stops at the newest user prompt, at an aborted assistant message or
// at the edge of the scan window, whichever comes first. A turn that
// already wrote its reply, an aborted turn and a walk that collected
// nothing usable are reported as domain.ErrNoActivity: the card should
// only appear while the agent is visibly working.
func pendingPiActivityIn(path string, maxScan int64) (domain.ActivitySnapshot, piScanStats, error) {
	data, stats, err := readTail(path, maxScan)
	if err != nil {
		return domain.ActivitySnapshot{}, stats, fmt.Errorf("%w: read pi transcript", domain.ErrNoActivity)
	}
	records := make(map[string]piRecord)
	var leaf string
	lines := bytes.Split(data, []byte{'\n'})
	unterminated := len(data) > 0 && data[len(data)-1] != '\n'
	for index, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		stats.lines++
		var record piRecord
		if err := json.Unmarshal(line, &record); err != nil {
			if unterminated && index == len(lines)-1 {
				return domain.ActivitySnapshot{}, stats, fmt.Errorf("%w: partial record", domain.ErrNoActivity)
			}
			stats.skipped++
			continue
		}
		if record.ID != "" {
			records[record.ID] = record
			leaf = record.ID
		}
	}

	var (
		active       string
		recentTools  []string
		subagents    []domain.SubagentActivity
		resolved     = make(map[string]struct{})
		turnComplete bool
		aborted      bool
	)
	visited := make(map[string]struct{})
walk:
	for id := leaf; id != ""; {
		if _, seen := visited[id]; seen {
			return domain.ActivitySnapshot{}, stats, fmt.Errorf("%w: parent cycle", domain.ErrNoActivity)
		}
		visited[id] = struct{}{}
		record, ok := records[id]
		if !ok {
			// The chain continues before the scan window - or a record
			// on it failed to parse. Either way the walk cannot tell
			// what happened after that point, so nothing is reported:
			// a skipped result must never leave its call looking
			// pending.
			return domain.ActivitySnapshot{}, stats, fmt.Errorf("%w: pi chain begins before scan window", domain.ErrNoActivity)
		}
		if record.Type == "message" {
			var message piMessage
			if err := json.Unmarshal(record.Message, &message); err != nil {
				stats.skipped++
			} else {
				switch message.Role {
				case "user":
					// The newest prompt starts the current turn; older
					// records belong to earlier turns.
					break walk
				case "toolResult":
					if message.ToolCallID != "" {
						resolved[message.ToolCallID] = struct{}{}
					}
					name := strings.TrimSpace(message.ToolName)
					if _, isSubagent := piSubagentTools[name]; isSubagent {
						// Subagent runs are shown as rows, not as tool
						// labels, so the card does not list them twice.
						subagents = append(subagents, piSubagentRows(message)...)
						break
					}
					if label := piActivityToolLabel(piContentBlock{Name: name}); label != "" {
						recentTools = append(recentTools, label)
					}
				case "assistant":
					if message.StopReason == "aborted" {
						// The turn was cut off; its pending calls never
						// ran and nothing older is current activity.
						aborted = true
						break walk
					}
					if !turnComplete && (message.StopReason == "stop" || message.StopReason == "length") && piHasText(message) {
						turnComplete = true
					}
					// A call older than a completed reply cannot still be
					// running: the reply could only be written after the
					// call's result arrived.
					if !turnComplete {
						for _, block := range message.Content {
							if block.Type != "toolCall" || block.ID == "" {
								continue
							}
							if _, done := resolved[block.ID]; done {
								continue
							}
							if active == "" {
								active = piActivityToolLabel(block)
							}
						}
					}
				}
			}
		}
		if record.ParentID == nil || *record.ParentID == "" {
			break
		}
		id = *record.ParentID
	}

	if aborted {
		return domain.ActivitySnapshot{}, stats, fmt.Errorf("%w: turn was aborted", domain.ErrNoActivity)
	}
	if active == "" && turnComplete {
		return domain.ActivitySnapshot{}, stats, fmt.Errorf("%w: turn already completed", domain.ErrNoActivity)
	}
	snapshot := domain.ActivitySnapshot{
		ActiveTool:  active,
		RecentTools: recentTools,
		Subagents:   subagents,
	}
	if len(snapshot.RecentTools) > maxActivityTools {
		snapshot.RecentTools = snapshot.RecentTools[:maxActivityTools]
	}
	if len(snapshot.Subagents) > maxActivitySubagents {
		snapshot.Subagents = snapshot.Subagents[:maxActivitySubagents]
	}
	if snapshot.ActiveTool == "" && len(snapshot.RecentTools) == 0 && len(snapshot.Subagents) == 0 {
		return domain.ActivitySnapshot{}, stats, fmt.Errorf("%w: no activity records in scan window", domain.ErrNoActivity)
	}
	return snapshot, stats, nil
}

// piActivityToolLabel renders an explicit public-safe activity label. Shell
// commands, paths, arbitrary arguments and tool output are intentionally
// excluded; only a web-search query or an Agent description is useful enough
// to surface after sanitizing.
func piActivityToolLabel(block piContentBlock) string {
	name := strings.TrimSpace(block.Name)
	base := map[string]string{
		"bash":                 "🛠 Running command",
		"read":                 "📖 Reading a file",
		"write":                "✍️ Writing a file",
		"edit":                 "✍️ Editing a file",
		"websearch":            "🔎 Searching the web",
		"web_search":           "🔎 Searching the web",
		"synthetic_web_search": "🔎 Searching the web",
		"Agent":                "🤖 Starting subagent",
		"subagent":             "🤖 Starting subagent",
		"manage_todo_list":     "☑ Updating tasks",
		"ask_user":             "❓ Waiting for input",
	}
	label, ok := base[name]
	if !ok {
		if clean := sanitizePiLabel(name, maxActivityNameLabel); clean != "" {
			return "⚙️ " + clean
		}
		return ""
	}
	var args struct {
		Query       string `json:"query"`
		Description string `json:"description"`
	}
	if json.Unmarshal(block.Arguments, &args) != nil {
		return label
	}
	if (name == "websearch" || name == "web_search" || name == "synthetic_web_search") && strings.TrimSpace(args.Query) != "" {
		return label + ": " + sanitizePiText(args.Query, maxActivityDescription)
	}
	if (name == "Agent" || name == "subagent") && strings.TrimSpace(args.Description) != "" {
		return label + ": " + sanitizePiText(args.Description, maxActivityDescription)
	}
	return label
}

// piAgentDetails is the structured summary Pi records on an Agent tool
// result. It is the only source for a modern subagent row: the result's
// text may quote anything the subagent saw, so it is never read.
type piAgentDetails struct {
	Description  string `json:"description"`
	SubagentType string `json:"subagentType"`
	Status       string `json:"status"`
}

// piLegacyAgentDetails is the older subagent tool's result shape: one
// entry per spawned run with the agent kind and an exit code, no summary
// text. A single legacy run leaves the results list empty, so it produces
// no row.
type piLegacyAgentDetails struct {
	Results []struct {
		Agent    string `json:"agent"`
		ExitCode int    `json:"exitCode"`
	}
}

// piSubagentRows turns one Agent or subagent tool result into the rows the
// card shows, newest first. Only structured fields are read, and a result
// without any is skipped rather than guessed at.
func piSubagentRows(message piMessage) []domain.SubagentActivity {
	var details piAgentDetails
	if json.Unmarshal(message.Details, &details) == nil {
		row := domain.SubagentActivity{
			Description: sanitizePiText(details.Description, maxActivityDescription),
			Type:        sanitizePiLabel(details.SubagentType, maxActivityNameLabel),
			Status:      sanitizePiLabel(details.Status, maxActivityStatusLabel),
		}
		if row.Status == "" && message.IsError {
			row.Status = "error"
		}
		if row.Description != "" || row.Type != "" || row.Status != "" {
			return []domain.SubagentActivity{row}
		}
	}
	var legacy piLegacyAgentDetails
	if json.Unmarshal(message.Details, &legacy) == nil && len(legacy.Results) > 0 {
		rows := make([]domain.SubagentActivity, 0, len(legacy.Results))
		for _, result := range legacy.Results {
			kind := sanitizePiLabel(result.Agent, maxActivityNameLabel)
			if kind == "" {
				continue
			}
			status := "completed"
			if result.ExitCode != 0 {
				status = "error"
			}
			rows = append(rows, domain.SubagentActivity{Type: kind, Status: status})
		}
		return rows
	}
	return nil
}

// sanitizePiLabel reduces a tool, kind or status name to a short generic
// token: letters, digits, dashes, dots and underscores only. Anything else
// becomes a dash, so no fragment of a command or path can reach a card
// even if a transcript carries an unexpected name.
func sanitizePiLabel(value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	clean := b.String()
	for strings.Contains(clean, "--") {
		clean = strings.ReplaceAll(clean, "--", "-")
	}
	clean = strings.Trim(clean, "-.")
	if len(clean) > max {
		clean = clean[:max]
	}
	return clean
}

// sanitizePiText reduces a free-text field to one short single-line label:
// control characters are dropped, whitespace collapses and the result is
// capped with an ellipsis. It is used only on the structured description
// an agent wrote for a subagent run, never on tool output or arguments.
func sanitizePiText(value string, max int) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			// Drop other control characters.
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.Join(strings.Fields(b.String()), " ")
	runes := []rune(clean)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return clean
}

// piHasText reports whether an assistant message carries any non-blank
// text block, the mark of a turn that wrote its reply.
func piHasText(message piMessage) bool {
	for _, block := range message.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return true
		}
	}
	return false
}

// lastPiReplyIn follows the active parent chain from the newest record back to
// its user prompt. This excludes abandoned branches and refuses to reuse a
// completed response from an older or aborted turn.
func lastPiReplyIn(path string, maxScan int64) (string, domain.TurnMeta, piScanStats, error) {
	data, stats, err := readTail(path, maxScan)
	if err != nil {
		return "", domain.TurnMeta{}, stats, fmt.Errorf("%w: read pi transcript: %v", domain.ErrNoReply, err)
	}
	records := make(map[string]piRecord)
	var leaf string
	lines := bytes.Split(data, []byte{'\n'})
	unterminated := len(data) > 0 && data[len(data)-1] != '\n'
	for index, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		stats.lines++
		var record piRecord
		if err := json.Unmarshal(line, &record); err != nil {
			if unterminated && index == len(lines)-1 {
				return "", domain.TurnMeta{}, stats, fmt.Errorf("%w: pi transcript ends with a partial record", domain.ErrNoReply)
			}
			stats.skipped++
			continue
		}
		if record.ID == "" {
			continue
		}
		records[record.ID] = record
		leaf = record.ID
	}
	if leaf == "" {
		return "", domain.TurnMeta{}, stats, fmt.Errorf("%w: pi transcript has no active records", domain.ErrNoReply)
	}
	tasks := latestPiTasks(records, leaf)

	var text string
	var meta domain.TurnMeta
	outputTokens := 0
	visited := make(map[string]struct{})
	for id := leaf; id != ""; {
		if _, seen := visited[id]; seen {
			return "", domain.TurnMeta{}, stats, fmt.Errorf("%w: cycle in pi transcript parent chain", domain.ErrNoReply)
		}
		visited[id] = struct{}{}
		record, ok := records[id]
		if !ok {
			return "", domain.TurnMeta{}, stats, fmt.Errorf("%w: pi turn begins before scan window", domain.ErrNoReply)
		}
		if record.Type == "message" {
			var message piMessage
			if err := json.Unmarshal(record.Message, &message); err != nil {
				stats.skipped++
			} else {
				switch message.Role {
				case "user":
					if text == "" {
						return "", domain.TurnMeta{}, stats, fmt.Errorf("%w: latest pi turn has no completed reply", domain.ErrNoReply)
					}
					meta.OutputTokens = outputTokens
					meta.Tasks = tasks
					if started, err := time.Parse(time.RFC3339Nano, record.Timestamp); err == nil {
						meta.Started = started
					}
					return text, meta, stats, nil
				case "assistant":
					outputTokens += message.Usage.Output
					if text == "" && (message.StopReason == "stop" || message.StopReason == "length") {
						var candidate strings.Builder
						for _, block := range message.Content {
							if block.Type == "text" {
								candidate.WriteString(block.Text)
							}
						}
						if completed := strings.TrimSpace(candidate.String()); completed != "" {
							text = completed
							meta.Model = message.Model
							if ended, err := time.Parse(time.RFC3339Nano, record.Timestamp); err == nil {
								meta.Ended = ended
							}
						}
					}
				}
			}
		}
		if record.ParentID == nil || *record.ParentID == "" {
			break
		}
		id = *record.ParentID
	}
	return "", domain.TurnMeta{}, stats, fmt.Errorf("%w: latest pi turn has no user prompt in scan window", domain.ErrNoReply)
}

func readTail(path string, maxScan int64) ([]byte, piScanStats, error) {
	var stats piScanStats
	file, err := os.Open(path)
	if err != nil {
		return nil, stats, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, stats, err
	}
	start := int64(0)
	if maxScan > 0 && info.Size() > maxScan {
		start = info.Size() - maxScan
	}
	partial := false
	if start > 0 {
		if _, err := file.Seek(start-1, io.SeekStart); err != nil {
			return nil, stats, err
		}
		var previous [1]byte
		if _, err := io.ReadFull(file, previous[:]); err != nil {
			return nil, stats, err
		}
		partial = previous[0] != '\n'
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, stats, err
	}
	data, err := io.ReadAll(io.LimitReader(file, info.Size()-start))
	stats.bytes = int64(len(data))
	if err != nil {
		return nil, stats, err
	}
	if partial {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
	}
	return data, stats, nil
}
