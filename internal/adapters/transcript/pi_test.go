package transcript

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
