package transcript

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

func TestPiProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/home/amit":                        "--home-amit--",
		"/home/amit/projects/onrecord-site": "--home-amit-projects-onrecord-site--",
		"/tmp/pi_work":                      "--tmp-pi_work--",
		`C:\Users\amit\work`:                "--C--Users-amit-work--",
	}
	for cwd, want := range cases {
		if got := piProjectSlug(cwd); got != want {
			t.Errorf("piProjectSlug(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestNewestPiTranscriptFiltersCollidingCwd(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	wanted := filepath.Join(dir, "wanted.jsonl")
	writePiSession(t, wanted, "/work/a-b", base, "wanted")
	if err := os.Chtimes(wanted, base, base); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.jsonl")
	writePiSession(t, other, "/work/a/b", base.Add(time.Minute), "private other project")
	if err := os.Chtimes(other, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	child := filepath.Join(dir, "child.jsonl")
	childData := joinLines([]string{
		piChildHeader("/work/a-b", wanted),
		piUserLine("cu", nil, base.Add(2*time.Minute), "internal task"),
		piAssistantLine("ca", ptr("cu"), base.Add(3*time.Minute), "stop", "subagent reply", "model", 1),
	})
	if err := os.WriteFile(child, []byte(childData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(child, base.Add(2*time.Hour), base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	path, mod, count, err := newestPiTranscript(dir, "/work/a-b")
	if err != nil || path != wanted || !mod.Equal(base) || count != 1 {
		t.Fatalf("path=%q mod=%v count=%d err=%v", path, mod, count, err)
	}
}

func TestNewestPiTranscriptNormalizesRelativeCwd(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "relative.jsonl")
	writePiSession(t, path, want, time.Now(), "reply")
	got, _, _, err := newestPiTranscript(dir, ".")
	if err != nil || got != path {
		t.Fatalf("path=%q err=%v", got, err)
	}
}

func TestLastPiReply(t *testing.T) {
	home := t.TempDir()
	cwd := "/home/amit/projects/demo"
	dir := filepath.Join(home, ".pi", "agent", "sessions", piProjectSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	older := filepath.Join(dir, "older.jsonl")
	writePiSession(t, older, cwd, base, "old reply")
	if err := os.Chtimes(older, base, base); err != nil {
		t.Fatal(err)
	}

	newer := filepath.Join(dir, "newer.jsonl")
	lines := []string{
		piHeader(cwd),
		piUserLine("u", nil, base, "do it"),
		piAssistantLine("a1", ptr("u"), base.Add(time.Second), "toolUse", "working", "gpt-test", 9),
		piToolResultLine("r1", "a1", base.Add(2*time.Second)),
		piAssistantLine("a2", ptr("r1"), base.Add(time.Minute), "stop", "new reply", "gpt-test", 42),
		piInfoLine("i", "a2"),
		`{not json}`,
	}
	if err := os.WriteFile(newer, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	now := base.Add(time.Hour + 3*time.Second)
	r := newReader(func() (string, error) { return home, nil }, func() time.Time { return now }, slog.New(slog.DiscardHandler))
	reply, err := r.LastReply(context.Background(), domain.Agent{Key: domain.Key{PaneID: "p1"}, Kind: kindPi, Cwd: cwd, SessionPath: newer})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "new reply" || reply.Source != newer || reply.Age != 3*time.Second || !reply.Written.Equal(base.Add(time.Hour)) {
		t.Errorf("reply = %+v", reply)
	}
	if reply.Meta.Model != "gpt-test" || reply.Meta.OutputTokens != 51 || !reply.Meta.Started.Equal(base) || !reply.Meta.Ended.Equal(base.Add(time.Minute)) {
		t.Errorf("meta = %+v", reply.Meta)
	}
}

func TestPendingPiQuestion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	data := joinLines([]string{piHeader("/work"), piUserLine("u", nil, base, "ask"), piAskUserLine("ask", "u", base.Add(time.Second), false, false)})
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := pendingPiQuestionIn(path, defaultMaxScan)
	want := domain.Question{ID: "call", Prompt: "Choose a deployment", Context: "The build passed.", Options: []domain.QuestionOption{{Number: 1, Title: "Deploy now"}, {Number: 2, Title: "Wait"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("question=%+v err=%v, want %+v", got, err, want)
	}
	if err := os.WriteFile(path, []byte(data+piAskResultLine("result", "ask", base.Add(2*time.Second))), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = pendingPiQuestionIn(path, defaultMaxScan)
	if !errors.Is(err, domain.ErrNoQuestion) {
		t.Fatalf("resolved question err=%v", err)
	}
	if err := os.WriteFile(path, []byte(joinLines([]string{piHeader("/work"), piUserLine("u", nil, base, "ask"), piAskUserLine("ask", "u", base.Add(time.Second), true, false)})), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = pendingPiQuestionIn(path, defaultMaxScan)
	if !errors.Is(err, domain.ErrNoQuestion) {
		t.Fatalf("multi-select question err=%v", err)
	}
	serialized := strings.ReplaceAll(strings.ReplaceAll(data, `,"allowFreeform":false`, ""), `,"allowComment":false`, "")
	if err := os.WriteFile(path, []byte(serialized), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err = pendingPiQuestionIn(path, defaultMaxScan)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("serialized false question=%+v err=%v, want %+v", got, err, want)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(data, `"allowFreeform":false`, `"allowFreeform":true`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = pendingPiQuestionIn(path, defaultMaxScan)
	if !errors.Is(err, domain.ErrNoQuestion) {
		t.Fatalf("explicit freeform question err=%v", err)
	}
}

func TestLastPiReplyIncludesLatestPersistentTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	lines := []string{
		piHeader("/work"),
		piUserLine("u1", nil, base, "first"),
		piTaskLine("tasks", "u1", base.Add(time.Second)),
		piAssistantLine("a1", ptr("tasks"), base.Add(2*time.Second), "stop", "first reply", "m", 4),
		piUserLine("u2", ptr("a1"), base.Add(3*time.Second), "second"),
		piAssistantLine("a2", ptr("u2"), base.Add(4*time.Second), "stop", "latest reply", "m", 5),
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	text, meta, _, err := lastPiReplyIn(path, defaultMaxScan)
	if err != nil || text != "latest reply" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	want := []domain.ReplyTask{{Title: "Finished thing", Status: "completed"}, {Title: "Current thing", Status: "in-progress"}, {Title: "Later thing", Status: "not-started"}}
	if !reflect.DeepEqual(meta.Tasks, want) {
		t.Fatalf("tasks = %+v, want %+v", meta.Tasks, want)
	}
}

func TestLastPiReplyRefusesAbortedLatestTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	lines := []string{
		piHeader("/work"),
		piUserLine("u1", nil, base, "first"),
		piAssistantLine("a1", ptr("u1"), base.Add(time.Second), "stop", "old completed reply", "m", 4),
		piUserLine("u2", ptr("a1"), base.Add(2*time.Second), "second"),
		piAssistantLine("a2", ptr("u2"), base.Add(3*time.Second), "aborted", "partial", "m", 2),
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := lastPiReplyIn(path, defaultMaxScan)
	if !errors.Is(err, domain.ErrNoReply) {
		t.Fatalf("err = %v", err)
	}
}

func TestLastPiReplyRefusesPartialFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	data := joinLines([]string{
		piHeader("/work"),
		piUserLine("u", nil, base, "ask"),
		piAssistantLine("a", ptr("u"), base.Add(time.Second), "stop", "completed", "m", 4),
	}) + `{"type":"message","id":"partial"`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := lastPiReplyIn(path, defaultMaxScan)
	if !errors.Is(err, domain.ErrNoReply) {
		t.Fatalf("err = %v", err)
	}
}

func TestLastPiReplyAcceptsLengthAndJoinsText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	line := fmt.Sprintf(`{"type":"message","id":"a","parentId":"u","timestamp":%q,"message":{"role":"assistant","model":"m","stopReason":"length","usage":{"output":7},"content":[{"type":"text","text":"first "},{"type":"thinking","thinking":"hidden"},{"type":"text","text":"second"}]}}`, base.Add(time.Second).Format(time.RFC3339Nano))
	data := joinLines([]string{piHeader("/work"), piUserLine("u", nil, base, "ask"), line})
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	text, meta, stats, err := lastPiReplyIn(path, defaultMaxScan)
	if err != nil || text != "first second" || meta.OutputTokens != 7 || stats.lines != 3 {
		t.Fatalf("text=%q meta=%+v stats=%+v err=%v", text, meta, stats, err)
	}
}

func TestReadTailKeepsRecordAtExactBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	first := "discard me\n"
	second := "keep me\n"
	if err := os.WriteFile(path, []byte(first+second), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _, err := readTail(path, int64(len(second)))
	if err != nil || string(data) != second {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func writePiSession(t *testing.T, path, cwd string, stamp time.Time, text string) {
	t.Helper()
	data := joinLines([]string{
		piHeader(cwd),
		piUserLine("u", nil, stamp, "ask"),
		piAssistantLine("a", ptr("u"), stamp.Add(time.Second), "stop", text, "model", 1),
	})
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func piHeader(cwd string) string {
	return fmt.Sprintf(`{"type":"session","version":3,"id":"session","timestamp":"2026-09-19T20:00:00Z","cwd":%q}`, cwd)
}

func piChildHeader(cwd, parent string) string {
	return fmt.Sprintf(`{"type":"session","version":3,"id":"session","timestamp":"2026-09-19T20:00:00Z","cwd":%q,"parentSession":%q}`, cwd, parent)
}

func piUserLine(id string, parent *string, stamp time.Time, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":%q}]}}`, id, jsonPointer(parent), stamp.Format(time.RFC3339Nano), text)
}

func piAssistantLine(id string, parent *string, stamp time.Time, stop, text, model string, output int) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":%q,"message":{"role":"assistant","model":%q,"stopReason":%q,"usage":{"output":%d},"content":[{"type":"text","text":%q}]}}`, id, jsonPointer(parent), stamp.Format(time.RFC3339Nano), model, stop, output, text)
}

func piToolResultLine(id, parent string, stamp time.Time) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%q,"timestamp":%q,"message":{"role":"toolResult","content":[{"type":"text","text":"ok"}]}}`, id, parent, stamp.Format(time.RFC3339Nano))
}

func piAskUserLine(id, parent string, stamp time.Time, multiple, freeform bool) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%q,"timestamp":%q,"message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"call","name":"ask_user","arguments":{"question":"Choose a deployment","context":"The build passed.","options":[{"title":"Deploy now"},{"title":"Wait"}],"allowMultiple":%t,"allowFreeform":%t,"allowComment":false}}]}}`, id, parent, stamp.Format(time.RFC3339Nano), multiple, freeform)
}

func piAskResultLine(id, parent string, stamp time.Time) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%q,"timestamp":%q,"message":{"role":"toolResult","toolName":"ask_user","content":[{"type":"text","text":"answered"}]}}`, id, parent, stamp.Format(time.RFC3339Nano))
}

func piTaskLine(id, parent string, stamp time.Time) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%q,"timestamp":%q,"message":{"role":"toolResult","toolName":"manage_todo_list","content":[{"type":"text","text":"updated"}],"details":{"operation":"write","todos":[{"id":1,"title":"Finished thing","status":"completed"},{"id":2,"title":"Current thing","status":"in-progress"},{"id":3,"title":"Later thing","status":"not-started"}]}}}`, id, parent, stamp.Format(time.RFC3339Nano))
}

func piInfoLine(id, parent string) string {
	return fmt.Sprintf(`{"type":"session_info","id":%q,"parentId":%q,"timestamp":"2026-09-19T20:02:00Z","name":"main"}`, id, parent)
}

func ptr(value string) *string { return &value }

func jsonPointer(value *string) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%q", *value)
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestPendingPiActivityTracksActiveAndCompletedTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	// The two pending calls in the newest message are running right now.
	// The read shows its path relative to the agent's cwd; the grep key
	// is an argument a card must never show.
	live := fmt.Sprintf(`{"type":"message","id":"a3","parentId":"r2","timestamp":%q,"message":{"role":"assistant","stopReason":"toolUse","content":[`+
		`{"type":"toolCall","id":"c3","name":"read","arguments":{"path":"/home/amit/secrets.txt"}},`+
		`{"type":"toolCall","id":"c4","name":"grep","arguments":{"pattern":"password"}}]}}`, base.Add(5*time.Second).Format(time.RFC3339Nano))
	lines := []string{
		piHeader("/work"),
		piUserLine("u", nil, base, "run the checks"),
		piToolCallLine("a1", ptr("u"), base.Add(time.Second), "c1", "bash"),
		piResultLine("r1", ptr("a1"), base.Add(2*time.Second), "bash", "c1", `{}`),
		piToolCallLine("a2", ptr("r1"), base.Add(3*time.Second), "c2", "edit"),
		piResultLine("r2", ptr("a2"), base.Add(4*time.Second), "edit", "c2", `{}`),
		live,
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := pendingPiActivityIn(path, "/work", defaultMaxScan)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ActivitySnapshot{ActiveTool: "📖 Reading /home/amit/secrets.txt", RecentTools: []string{"✍️ Editing a file", "🛠 Running command"}}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
}

func TestPendingPiActivityRendersPathsRelativeToCwd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	call := func(id, parent, callID, name, path string, offset time.Duration) string {
		return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%q,"timestamp":%q,"message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":%q,"name":%q,"arguments":{"path":%q}}]}}`,
			id, parent, base.Add(offset).Format(time.RFC3339Nano), callID, name, path)
	}
	long := "/work/" + strings.Repeat("deep/", 12) + "leaf.go"
	// The relative path keeps its newest 62 runes behind the ellipsis, so
	// the tail names the file and the directories closest to it.
	longTail := strings.Repeat("deep/", 11) + "leaf.go"
	lines := []string{
		piHeader("/work"),
		piUserLine("u", nil, base, "edit some files"),
		// A completed edit: the path comes from the indexed call, since the
		// result itself carries no arguments.
		call("a1", "u", "c1", "edit", "/work/internal/app/bridge.go", time.Second),
		piResultLine("r1", ptr("a1"), base.Add(2*time.Second), "edit", "c1", `{}`),
		// A completed read outside the cwd keeps its absolute path.
		call("a2", "r1", "c2", "read", "/etc/hosts", 3*time.Second),
		piResultLine("r2", ptr("a2"), base.Add(4*time.Second), "read", "c2", `{}`),
		// An over-long path keeps its most specific trailing segments.
		call("a3", "r2", "c3", "write", long, 5*time.Second),
		piResultLine("r3", ptr("a3"), base.Add(6*time.Second), "write", "c3", `{}`),
		// A file call without a path falls back to the generic label.
		fmt.Sprintf(`{"type":"message","id":"a4","parentId":"r3","timestamp":%q,"message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c4","name":"read","arguments":{}}]}}`, base.Add(7*time.Second).Format(time.RFC3339Nano)),
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := pendingPiActivityIn(path, "/work", defaultMaxScan)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ActivitySnapshot{
		ActiveTool: "📖 Reading a file",
		RecentTools: []string{
			"✍️ Writing …/" + longTail,
			"📖 Reading /etc/hosts",
			"✍️ Editing internal/app/bridge.go",
		},
	}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
}

func TestPendingPiActivityCapsToolsAndStopsAtThePrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	stamp := base
	lines := []string{piHeader("/work")}
	last := ""
	addPrompt := func(id, text string) {
		lines = append(lines, piUserLine(id, ptr(last), stamp, text))
		last, stamp = id, stamp.Add(time.Second)
	}
	addPair := func(name, callID string) {
		lines = append(lines, piToolCallLine("a-"+callID, ptr(last), stamp, callID, name))
		last, stamp = "a-"+callID, stamp.Add(time.Second)
		lines = append(lines, piResultLine("r-"+callID, ptr(last), stamp, name, callID, `{}`))
		last, stamp = "r-"+callID, stamp.Add(time.Second)
	}
	addPrompt("u1", "first")
	addPair("oldone", "o1")
	addPair("oldtwo", "o2")
	lines = append(lines, piAssistantLine("done1", ptr(last), stamp, "stop", "first reply", "m", 1))
	last, stamp = "done1", stamp.Add(time.Second)
	addPrompt("u2", "second")
	for index := 1; index <= 6; index++ {
		addPair(fmt.Sprintf("tool%d", index), fmt.Sprintf("t%d", index))
	}
	lines = append(lines, piToolCallLine("a-live", ptr(last), stamp, "live", "bash"))
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := pendingPiActivityIn(path, "/work", defaultMaxScan)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ActivitySnapshot{
		ActiveTool:  "🛠 Running command",
		RecentTools: []string{"⚙️ tool6", "⚙️ tool5", "⚙️ tool4", "⚙️ tool3", "⚙️ tool2"},
	}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
}

func TestPendingPiActivitySubagentRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	lines := []string{
		piHeader("/work"),
		piUserLine("u", nil, base, "fan out"),
		piToolCallLine("a1", ptr("u"), base.Add(time.Second), "s1", "bash"),
		piResultLine("r1", ptr("a1"), base.Add(2*time.Second), "bash", "s1", `{}`),
		// A modern background run: the structured details are the row.
		piToolCallLine("a2", ptr("r1"), base.Add(3*time.Second), "s2", "Agent"),
		piResultLine("r2", ptr("a2"), base.Add(4*time.Second), "Agent", "s2",
			`{"description":"Trace  NZBGet\n send\tflow","subagentType":"Explore","status":"background"}`),
		// A modern result without structured fields: no row, no guess.
		piToolCallLine("a3", ptr("r2"), base.Add(5*time.Second), "s3", "Agent"),
		piResultLine("r3", ptr("a3"), base.Add(6*time.Second), "Agent", "s3", `{}`),
		// A legacy parallel run: kind and exit code per entry.
		piToolCallLine("a4", ptr("r3"), base.Add(7*time.Second), "s4", "subagent"),
		piResultLine("r4", ptr("a4"), base.Add(8*time.Second), "subagent", "s4",
			`{"mode":"parallel","results":[{"agent":"worker","exitCode":0},{"agent":"reviewer","exitCode":1}]}`),
		// A long description is capped with an ellipsis.
		piToolCallLine("a5", ptr("r4"), base.Add(9*time.Second), "s5", "Agent"),
		piResultLine("r5", ptr("a5"), base.Add(10*time.Second), "Agent", "s5",
			`{"description":"`+strings.Repeat("x", 120)+`","subagentType":"general-purpose","status":"completed"}`),
		// The live foreground spawn stays a pending call, not a row.
		piToolCallLine("a6", ptr("r5"), base.Add(11*time.Second), "s6", "Agent"),
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := pendingPiActivityIn(path, "/work", defaultMaxScan)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ActivitySnapshot{
		ActiveTool:  "🤖 Starting subagent",
		RecentTools: []string{"🛠 Running command"},
		Subagents: []domain.SubagentActivity{
			{Description: strings.Repeat("x", 96) + "…", Type: "general-purpose", Status: "completed"},
			{Type: "worker", Status: "completed"},
			{Type: "reviewer", Status: "error"},
			{Description: "Trace NZBGet send flow", Type: "Explore", Status: "background"},
		},
	}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
}

func TestPendingPiActivityCapsSubagentRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	stamp := base
	lines := []string{piHeader("/work"), piUserLine("u", nil, stamp, "fan out")}
	last := "u"
	stamp = stamp.Add(time.Second)
	for index := 1; index <= 6; index++ {
		callID := fmt.Sprintf("s%d", index)
		lines = append(lines, piToolCallLine("a-"+callID, ptr(last), stamp, callID, "Agent"))
		last, stamp = "a-"+callID, stamp.Add(time.Second)
		lines = append(lines, piResultLine("r-"+callID, ptr(last), stamp, "Agent", callID,
			fmt.Sprintf(`{"description":"run%d","subagentType":"worker","status":"completed"}`, index)))
		last, stamp = "r-"+callID, stamp.Add(time.Second)
	}
	lines = append(lines, piToolCallLine("a-live", ptr(last), stamp, "live", "bash"))
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := pendingPiActivityIn(path, "/work", defaultMaxScan)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveTool != "🛠 Running command" || len(snapshot.Subagents) != maxActivitySubagents {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Subagents[0].Description != "run6" || snapshot.Subagents[4].Description != "run2" {
		t.Fatalf("rows = %+v, want run6..run2 newest first", snapshot.Subagents)
	}
}

func TestPendingPiActivityRefusesIdleAbortedAndEmpty(t *testing.T) {
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	completed := joinLines([]string{
		piHeader("/work"),
		piUserLine("u", nil, base, "do it"),
		piToolCallLine("a1", ptr("u"), base.Add(time.Second), "c1", "bash"),
		piResultLine("r1", ptr("a1"), base.Add(2*time.Second), "bash", "c1", `{}`),
		piAssistantLine("a2", ptr("r1"), base.Add(3*time.Second), "stop", "all done", "m", 3),
	})
	aborted := joinLines([]string{
		piHeader("/work"),
		piUserLine("u", nil, base, "do it"),
		fmt.Sprintf(`{"type":"message","id":"a1","parentId":"u","timestamp":%q,"message":{"role":"assistant","stopReason":"aborted","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"rm -rf /"}}]}}`, base.Add(time.Second).Format(time.RFC3339Nano)),
	})
	promptOnly := joinLines([]string{piHeader("/work"), piUserLine("u", nil, base, "do it")})
	partial := joinLines([]string{
		piHeader("/work"),
		piUserLine("u", nil, base, "do it"),
		piToolCallLine("a1", ptr("u"), base.Add(time.Second), "c1", "bash"),
	}) + `{"type":"message","id":"partial"`
	cases := map[string]string{
		"completed turn": completed,
		"aborted turn":   aborted,
		"prompt only":    promptOnly,
		"empty file":     "",
		"partial record": partial,
	}
	for name, data := range cases {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := pendingPiActivityIn(path, "/work", defaultMaxScan); !errors.Is(err, domain.ErrNoActivity) {
			t.Fatalf("%s: err = %v, want ErrNoActivity", name, err)
		}
	}
}

func TestPendingPiActivityIgnoresStalePendingCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	// A legacy result without a toolCallId cannot resolve its call, but
	// the completed reply written after it proves the call is long done.
	lines := []string{
		piHeader("/work"),
		piUserLine("u", nil, base, "fan out"),
		piToolCallLine("a1", ptr("u"), base.Add(time.Second), "legacy-call", "subagent"),
		piResultLine("r1", ptr("a1"), base.Add(2*time.Second), "subagent", "", `{"mode":"single","results":[]}`),
		piAssistantLine("a2", ptr("r1"), base.Add(3*time.Second), "stop", "done", "m", 2),
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pendingPiActivityIn(path, "/work", defaultMaxScan); !errors.Is(err, domain.ErrNoActivity) {
		t.Fatalf("err = %v, want ErrNoActivity", err)
	}
}

func TestReaderPendingActivityReadsRootSessionOnly(t *testing.T) {
	home := t.TempDir()
	cwd := "/home/amit/projects/demo"
	dir := filepath.Join(home, ".pi", "agent", "sessions", piProjectSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	root := filepath.Join(dir, "root.jsonl")
	rootData := joinLines([]string{
		piHeader(cwd),
		piUserLine("u", nil, base, "go"),
		piToolCallLine("a1", ptr("u"), base.Add(time.Second), "c1", "bash"),
		piResultLine("r1", ptr("a1"), base.Add(2*time.Second), "bash", "c1", `{}`),
		piToolCallLine("a2", ptr("r1"), base.Add(3*time.Second), "c2", "read"),
	})
	if err := os.WriteFile(root, []byte(rootData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(root, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A child session is newer but belongs to a subagent; it must never
	// be mistaken for the pane's transcript, by discovery or by name.
	child := filepath.Join(dir, "child.jsonl")
	childData := joinLines([]string{
		piChildHeader(cwd, "root-session"),
		piUserLine("cu", nil, base.Add(time.Minute), "internal task"),
		piAssistantLine("ca", ptr("cu"), base.Add(2*time.Minute), "stop", "child reply", "m", 1),
	})
	if err := os.WriteFile(child, []byte(childData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(child, base.Add(2*time.Hour), base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := base.Add(2 * time.Hour)
	r := newReader(func() (string, error) { return home, nil }, func() time.Time { return now }, slog.New(slog.DiscardHandler))
	snapshot, err := r.PendingActivity(context.Background(), domain.Agent{Key: domain.Key{PaneID: "p1"}, Kind: kindPi, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ActivitySnapshot{ActiveTool: "📖 Reading a file", RecentTools: []string{"🛠 Running command"}}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
	agent := domain.Agent{Key: domain.Key{PaneID: "p1"}, Kind: kindPi, Cwd: cwd}
	if _, err := r.PendingActivity(context.Background(), domain.Agent{Key: agent.Key, Kind: kindPi, Cwd: cwd, SessionPath: child}); !errors.Is(err, domain.ErrNoActivity) {
		t.Fatalf("child session path err = %v, want ErrNoActivity", err)
	}
	if _, err := r.PendingActivity(context.Background(), domain.Agent{Key: agent.Key, Kind: "claude", Cwd: cwd}); !errors.Is(err, domain.ErrNoActivity) {
		t.Fatalf("other kind err = %v, want ErrNoActivity", err)
	}
	if _, err := r.PendingActivity(context.Background(), domain.Agent{Key: agent.Key, Kind: kindPi, Cwd: "   "}); !errors.Is(err, domain.ErrNoActivity) {
		t.Fatalf("missing cwd err = %v, want ErrNoActivity", err)
	}
}

// piToolCallLine writes an assistant message with one pending tool call.
// The arguments hold junk on purpose: argument values must never reach an
// activity card.
func piToolCallLine(id string, parent *string, stamp time.Time, callID, name string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":%q,"message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":%q,"name":%q,"arguments":{"note":"argument values never reach a card"}}]}}`,
		id, jsonPointer(parent), stamp.Format(time.RFC3339Nano), callID, name)
}

// piResultLine writes a tool result with its tool name, the call id it
// resolves and raw details JSON ({} when empty).
func piResultLine(id string, parent *string, stamp time.Time, toolName, callID, details string) string {
	if details == "" {
		details = "{}"
	}
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":%q,"message":{"role":"toolResult","toolCallId":%q,"toolName":%q,"content":[{"type":"text","text":"ok"}],"details":%s}}`,
		id, jsonPointer(parent), stamp.Format(time.RFC3339Nano), callID, toolName, details)
}
