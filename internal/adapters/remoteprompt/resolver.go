package remoteprompt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
)

// Target is one mirrored remote terminal: where it lives and how to prompt
// the agent running in it.
type Target struct {
	// Host is the SSH destination, as the remote-panes config names it.
	Host string
	// Session is the remote HERDR_SESSION; empty is the machine's default
	// session, the one plain `herdr` opens there.
	Session string
	// Bin is the remote herdr binary path the remote-panes config carries.
	Bin string
	// Terminal is the remote terminal id the mirrored pane shows.
	Terminal string
}

// Resolver maps a local pane to the remote terminal it mirrors.
type Resolver interface {
	// Mirror resolves paneID. ok is false when the pane is not a mirror, so
	// the caller prompts locally; err reports a configuration problem that
	// makes the routing decision itself impossible (the pane IS a mirror but
	// the host that owns its terminal cannot be reached safely).
	Mirror(paneID string) (Target, bool, error)
}

// defaultSessionName is how the remote-panes config spells the machine's
// own unnamed default session.
const defaultSessionName = "default"

// RemotePanes reads the poorplebs.remote-panes plugin state: its snapshot of
// which local pane shows which remote terminal, and its host config with the
// SSH target and the remote herdr binary. Both files are read on every
// lookup, so a mirror opened or closed while the daemon runs is seen at
// once; both are small.
type RemotePanes struct {
	// statePath is the snapshot (mirrors-<session>.json): per host a map of
	// remote terminal id to the local pane showing it.
	statePath string
	// configPath is the plugin config: the hosts with their SSH target,
	// remote session and herdr binary.
	configPath string
	log        *slog.Logger
}

// NewRemotePanes returns a resolver for the remote-panes state at statePath
// and config at configPath.
func NewRemotePanes(statePath, configPath string, log *slog.Logger) *RemotePanes {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &RemotePanes{statePath: statePath, configPath: configPath, log: log}
}

// Mirror implements Resolver. A pane the snapshot does not mention is not a
// mirror. A snapshot that cannot be read is treated the same way (with a
// warning), because prompting locally is what happened before this
// decorator existed; a snapshot that maps the pane but a config that cannot
// say how to reach the host is an error, because the local prompt would
// fail on the mirror's foreground guard with a far less useful message.
func (r *RemotePanes) Mirror(paneID string) (Target, bool, error) {
	host, terminal, ok := r.mirrorHost(paneID)
	if !ok {
		return Target{}, false, nil
	}
	cfg, err := readConfig(r.configPath)
	if err != nil {
		return Target{}, false, fmt.Errorf("mirrored pane %s: %w", paneID, err)
	}
	hc, ok := cfg.host(host)
	if !ok {
		return Target{}, false, fmt.Errorf("mirrored pane %s: remote-panes config has no host %q", paneID, host)
	}
	if hc.Disabled {
		return Target{}, false, fmt.Errorf("mirrored pane %s: remote-panes host %q is disabled", paneID, host)
	}
	if hc.HerdrBin == "" {
		return Target{}, false, fmt.Errorf("mirrored pane %s: remote-panes host %q has no herdr_bin", paneID, host)
	}
	return Target{
		Host:     hc.Target,
		Session:  sessionFor(hc.Session, cfg.Session),
		Bin:      hc.HerdrBin,
		Terminal: terminal,
	}, true, nil
}

// mirrorHost reads the snapshot and reverses its map: which host's mirror
// shows paneID, and which remote terminal that is. Hosts and terminals are
// visited in sorted order, so a snapshot that claims one pane twice resolves
// the same way every time instead of by map accident. Entries whose
// identifiers are not safe to put into an ssh argv are skipped, never
// routed.
func (r *RemotePanes) mirrorHost(paneID string) (host, terminal string, ok bool) {
	raw, err := os.ReadFile(r.statePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.log.Warn("remote-panes snapshot unreadable, prompting locally",
				slog.String("path", r.statePath), slog.String("err", err.Error()))
		}
		return "", "", false
	}
	var state remotePanesState
	if err := json.Unmarshal(raw, &state); err != nil {
		r.log.Warn("remote-panes snapshot unreadable, prompting locally",
			slog.String("path", r.statePath), slog.String("err", err.Error()))
		return "", "", false
	}
	hosts := make([]string, 0, len(state.Hosts))
	for name := range state.Hosts {
		hosts = append(hosts, name)
	}
	sort.Strings(hosts)
	for _, name := range hosts {
		terms := make([]string, 0, len(state.Hosts[name].Mirrors))
		for remote := range state.Hosts[name].Mirrors {
			terms = append(terms, remote)
		}
		sort.Strings(terms)
		for _, remote := range terms {
			pane := state.Hosts[name].Mirrors[remote]
			if pane != paneID || !validToken(name) || !validToken(remote) || !validToken(pane) {
				continue
			}
			return name, remote, true
		}
	}
	return "", "", false
}

// remotePanesState is the snapshot the remote-panes daemon persists.
type remotePanesState struct {
	Hosts map[string]remotePanesHostState `json:"hosts"`
}

type remotePanesHostState struct {
	// Mirrors maps a remote terminal id to the local pane showing it.
	Mirrors map[string]string `json:"mirrors"`
}

// remotePanesConfig is the plugin configuration file.
type remotePanesConfig struct {
	Session string                  `json:"session"`
	Hosts   []remotePanesHostConfig `json:"hosts"`
}

type remotePanesHostConfig struct {
	Target   string `json:"target"`
	Session  string `json:"session"`
	HerdrBin string `json:"herdr_bin"`
	Disabled bool   `json:"disabled"`
}

// host returns the config of the host a snapshot entry names.
func (c remotePanesConfig) host(target string) (remotePanesHostConfig, bool) {
	for _, h := range c.Hosts {
		if h.Target == target {
			return h, true
		}
	}
	return remotePanesHostConfig{}, false
}

// readConfig loads the remote-panes config file. A missing file is an empty
// configuration, so a mapped pane then reports the missing host rather than
// a missing file.
func readConfig(path string) (remotePanesConfig, error) {
	var cfg remotePanesConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// sessionFor resolves which remote session a host's mirrors live in, the
// way the remote-panes plugin does: the host's own setting, else the
// top-level one, with the default session spelled as an empty HERDR_SESSION.
func sessionFor(hostSession, topLevel string) string {
	name := hostSession
	if name == "" {
		name = topLevel
	}
	if name == defaultSessionName {
		return ""
	}
	return name
}
