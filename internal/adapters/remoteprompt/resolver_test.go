package remoteprompt

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveState is the shape of the remote-panes snapshot this daemon reads.
const liveState = `{
  "hosts": {
    "hermesbox": {
      "mirrors": {
        "term_65beab1fe46b12": "wR:p2",
        "term_65beb4e1030c23": "wR:p3"
      },
      "shells": 1,
      "shell_placements": ["follow"]
    }
  }
}`

// liveConfig is the shape of the remote-panes plugin configuration.
const liveConfig = `{
  "hosts": [
    {"target": "hermesbox", "mode": "attach", "herdr_bin": "/home/amit/.local/bin/herdr"}
  ]
}`

// writeBoth writes a snapshot and a config into a test directory; an empty
// string means the file is not written at all.
func writeBoth(t *testing.T, state, config string) (statePath, configPath string) {
	t.Helper()
	dir := t.TempDir()
	statePath = filepath.Join(dir, "mirrors-default.json")
	configPath = filepath.Join(dir, "config.json")
	for path, content := range map[string]string{statePath: state, configPath: config} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return statePath, configPath
}

func TestMirrorMapsPaneToRemoteTerminal(t *testing.T) {
	statePath, configPath := writeBoth(t, liveState, liveConfig)
	r := NewRemotePanes(statePath, configPath, slog.New(slog.DiscardHandler))
	target, ok, err := r.Mirror("wR:p3")
	if err != nil || !ok {
		t.Fatalf("Mirror(wR:p3) = %+v, %v", target, err)
	}
	want := Target{Host: "hermesbox", Bin: "/home/amit/.local/bin/herdr",
		Terminal: "term_65beb4e1030c23"}
	if target != want {
		t.Fatalf("Mirror(wR:p3) = %+v, want %+v", target, want)
	}
}

func TestMirrorUnknownPaneIsLocal(t *testing.T) {
	statePath, configPath := writeBoth(t, liveState, liveConfig)
	r := NewRemotePanes(statePath, configPath, nil)
	if _, ok, err := r.Mirror("w1:p1"); ok || err != nil {
		t.Fatalf("Mirror(w1:p1) = ok %v, err %v", ok, err)
	}
}

func TestMirrorWithoutSnapshotIsLocal(t *testing.T) {
	statePath, configPath := writeBoth(t, "", liveConfig)
	r := NewRemotePanes(statePath, configPath, nil)
	if _, ok, err := r.Mirror("wR:p2"); ok || err != nil {
		t.Fatalf("Mirror with missing snapshot = ok %v, err %v", ok, err)
	}
}

func TestMirrorCorruptSnapshotIsLocal(t *testing.T) {
	statePath, configPath := writeBoth(t, "{not json", liveConfig)
	r := NewRemotePanes(statePath, configPath, nil)
	if _, ok, err := r.Mirror("wR:p2"); ok || err != nil {
		t.Fatalf("Mirror with corrupt snapshot = ok %v, err %v", ok, err)
	}
}

func TestMirrorSkipsUnsafeIdentifiers(t *testing.T) {
	state := `{"hosts":{
		"bad host":{"mirrors":{"term_ok":"wR:p1"}},
		"hermesbox":{"mirrors":{"term; rm -rf /":"wR:p2","term_ok":"wR:p3"}}
	}}`
	statePath, configPath := writeBoth(t, state, liveConfig)
	r := NewRemotePanes(statePath, configPath, nil)
	for _, pane := range []string{"wR:p1", "wR:p2"} {
		if _, ok, err := r.Mirror(pane); ok || err != nil {
			t.Fatalf("Mirror(%q) routed an unsafe entry: ok %v, err %v", pane, ok, err)
		}
	}
	target, ok, err := r.Mirror("wR:p3")
	if err != nil || !ok || target.Host != "hermesbox" || target.Terminal != "term_ok" {
		t.Fatalf("Mirror(wR:p3) = %+v, ok %v, err %v", target, ok, err)
	}
}

func TestMirrorWithoutHostConfigIsError(t *testing.T) {
	statePath, configPath := writeBoth(t, liveState, `{"hosts":[]}`)
	r := NewRemotePanes(statePath, configPath, nil)
	_, ok, err := r.Mirror("wR:p2")
	if ok || err == nil || !strings.Contains(err.Error(), `"hermesbox"`) {
		t.Fatalf("Mirror without host config = ok %v, err %v", ok, err)
	}
}

func TestMirrorRejectsDisabledHost(t *testing.T) {
	statePath, configPath := writeBoth(t, liveState,
		`{"hosts":[{"target":"hermesbox","herdr_bin":"/x/herdr","disabled":true}]}`)
	r := NewRemotePanes(statePath, configPath, nil)
	if _, ok, err := r.Mirror("wR:p2"); ok || err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("Mirror on disabled host = ok %v, err %v", ok, err)
	}
}

func TestMirrorRejectsMissingBin(t *testing.T) {
	statePath, configPath := writeBoth(t, liveState, `{"hosts":[{"target":"hermesbox"}]}`)
	r := NewRemotePanes(statePath, configPath, nil)
	if _, ok, err := r.Mirror("wR:p2"); ok || err == nil || !strings.Contains(err.Error(), "herdr_bin") {
		t.Fatalf("Mirror without herdr_bin = ok %v, err %v", ok, err)
	}
}

func TestMirrorUnreadableConfigIsError(t *testing.T) {
	statePath, _ := writeBoth(t, liveState, "")
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	r := NewRemotePanes(statePath, configPath, nil)
	if _, ok, err := r.Mirror("wR:p2"); ok || err == nil {
		t.Fatalf("Mirror with unreadable config = ok %v, err %v", ok, err)
	}
}

func TestMirrorCorruptConfigIsError(t *testing.T) {
	statePath, configPath := writeBoth(t, liveState, "{oops")
	r := NewRemotePanes(statePath, configPath, nil)
	_, ok, err := r.Mirror("wR:p2")
	if ok || err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("Mirror with corrupt config = ok %v, err %v", ok, err)
	}
}

func TestMirrorSessionResolution(t *testing.T) {
	state := `{"hosts":{"box":{"mirrors":{"term_a":"wR:p1"}}}}`
	cases := []struct {
		config string
		want   string
	}{
		{`{"hosts":[{"target":"box","herdr_bin":"/h"}]}`, ""},
		{`{"hosts":[{"target":"box","herdr_bin":"/h"}],"session":"hub"}`, "hub"},
		{`{"hosts":[{"target":"box","herdr_bin":"/h","session":"own"}],"session":"hub"}`, "own"},
		{`{"hosts":[{"target":"box","herdr_bin":"/h"}],"session":"default"}`, ""},
		{`{"hosts":[{"target":"box","herdr_bin":"/h","session":"default"}],"session":"hub"}`, ""},
	}
	for i, c := range cases {
		statePath, configPath := writeBoth(t, state, c.config)
		r := NewRemotePanes(statePath, configPath, nil)
		target, ok, err := r.Mirror("wR:p1")
		if err != nil || !ok {
			t.Fatalf("case %d: Mirror = ok %v, err %v", i, ok, err)
		}
		if target.Session != c.want {
			t.Fatalf("case %d: session = %q, want %q", i, target.Session, c.want)
		}
	}
}

func TestSessionFor(t *testing.T) {
	if got := sessionFor("", ""); got != "" {
		t.Fatalf("sessionFor(,) = %q", got)
	}
	if got := sessionFor("default", "hub"); got != "" {
		t.Fatalf("sessionFor(default,hub) = %q", got)
	}
	if got := sessionFor("", "default"); got != "" {
		t.Fatalf("sessionFor(,default) = %q", got)
	}
	if got := sessionFor("", "hub"); got != "hub" {
		t.Fatalf("sessionFor(,hub) = %q", got)
	}
}
