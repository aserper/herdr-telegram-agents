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
	Usage      struct {
		Output int `json:"output"`
	} `json:"usage"`
}

type piContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
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
