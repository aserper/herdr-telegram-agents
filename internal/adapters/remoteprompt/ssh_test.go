package remoteprompt

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellQuoteLeavesSafeWordsAlone(t *testing.T) {
	for _, s := range []string{"agent", "list", "prompt", "wR:p2", "term_65beab1fe46b12",
		"/home/amit/.local/bin/herdr", "HERDR_SESSION=hub"} {
		if got := shellQuote(s); got != s {
			t.Fatalf("shellQuote(%q) = %q, want unchanged", s, got)
		}
	}
}

func TestShellQuoteWrapsUnsafeWords(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"a b", "'a b'"},
		{"it's", "'it'\\''s'"},
		{"don't panic", `'don'\''t panic'`},
		{`say "hi"`, `'say "hi"'`},
		{"$(touch /tmp/pwned)", "'$(touch /tmp/pwned)'"},
		{"`id`", "'`id`'"},
		{"line\nbreak", "'line\nbreak'"},
		{";rm -rf /", "';rm -rf /'"},
		{"a|b&c", "'a|b&c'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Fatalf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidToken(t *testing.T) {
	for _, s := range []string{"hermesbox", "user@host.example", "wR:p2", "term_65beab1fe46b12",
		"w_1:p_2", "a:b:c", "host-1.example.com"} {
		if !validToken(s) {
			t.Fatalf("validToken(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "-oProxyCommand=evil", "a b", "a;b", "a'b", "a\tb", "a\nb",
		"$(x)", "`x`", "a|b", "a&b", "a<b", "a>b", `a\b`, `a"b`, "a#b", "a?b", "a*b", "a[b",
		"a]b", "a{b", "a}b", "a$b", "a~b", "a!b", strings.Repeat("a", 257)} {
		if validToken(s) {
			t.Fatalf("validToken(%q) = true, want false", s)
		}
	}
}

func TestRemoteCommandQuotesEveryPart(t *testing.T) {
	got := remoteCommand("/home/amit/.local/bin/herdr", []string{"agent", "prompt", "wR:p9", "hi there"}, "")
	want := "env -u HERDR_SOCKET_PATH -u HERDR_CLIENT_SOCKET_PATH" +
		" /home/amit/.local/bin/herdr agent prompt wR:p9 'hi there'"
	if got != want {
		t.Fatalf("remoteCommand =\n%q\nwant\n%q", got, want)
	}
	got = remoteCommand("herdr", []string{"agent", "list"}, "hub")
	want = "env -u HERDR_SOCKET_PATH -u HERDR_CLIENT_SOCKET_PATH HERDR_SESSION=hub herdr agent list"
	if got != want {
		t.Fatalf("remoteCommand =\n%q\nwant\n%q", got, want)
	}
}

func TestSSHArgvRunsOneCommandOnTarget(t *testing.T) {
	argv := sshArgv("hermesbox", "/tmp/ctl.sock", "env -u X herdr agent list")
	if argv[0] != "ssh" {
		t.Fatalf("argv[0] = %q, want ssh", argv[0])
	}
	if got := argv[len(argv)-1]; got != "env -u X herdr agent list" {
		t.Fatalf("command = %q", got)
	}
	dash := -1
	for i, a := range argv {
		if a == "--" {
			dash = i
		}
	}
	if dash < 0 || argv[dash+1] != "hermesbox" {
		t.Fatalf("no -- before target: %v", argv)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"BatchMode=yes", "ControlMaster=auto", "ControlPath=/tmp/ctl.sock",
		"ControlPersist=120", "ConnectTimeout=10", "ServerAliveInterval=15"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %s: %v", want, argv)
		}
	}
}

func TestControlPathMatchesRemotePanesDerivation(t *testing.T) {
	a, b := controlPath("hermesbox", ""), controlPath("hermesbox", "")
	if a != b {
		t.Fatalf("controlPath not deterministic: %q vs %q", a, b)
	}
	if c := controlPath("hermesbox", "hub"); c == a {
		t.Fatalf("session does not change the socket: %q", c)
	}
	base := filepath.Base(a)
	if !strings.HasPrefix(base, "hrp-") || !strings.HasSuffix(base, ".sock") {
		t.Fatalf("controlPath = %q, want hrp-<sum>.sock", base)
	}
}

func TestDecodeEnvelopeFindsResultAroundBanners(t *testing.T) {
	out := []byte("herdr 0.9.1\nan update is available\n" +
		`{"id":"cli:agent:list","result":{"agents":[]},"type":"agent_list"}` + "\n")
	res, err := decodeEnvelope(out, []string{"agent", "list"})
	if err != nil || string(res) != `{"agents":[]}` {
		t.Fatalf("decodeEnvelope = %q, %v", res, err)
	}
}

func TestDecodeEnvelopeReadsMultilineOutput(t *testing.T) {
	out := []byte("{\n  \"result\": {\"ok\": true},\n  \"type\": \"agent_prompted\"\n}")
	res, err := decodeEnvelope(out, []string{"agent", "prompt"})
	if err != nil || !strings.Contains(string(res), `"ok": true`) {
		t.Fatalf("decodeEnvelope = %q, %v", res, err)
	}
}

func TestDecodeEnvelopeErrorBody(t *testing.T) {
	out := []byte(`{"error":{"code":"agent_not_found","message":"no agent for wR:p9"}}`)
	_, err := decodeEnvelope(out, []string{"agent", "prompt"})
	var api *remoteAPIError
	if !errors.As(err, &api) || api.code != "agent_not_found" || api.message != "no agent for wR:p9" || !api.notFound() {
		t.Fatalf("decodeEnvelope error = %v", err)
	}
}

func TestDecodeEnvelopeEmptyOutputIsNoResult(t *testing.T) {
	res, err := decodeEnvelope(nil, []string{"agent", "list"})
	if err != nil || res != nil {
		t.Fatalf("decodeEnvelope = %q, %v", res, err)
	}
}

func TestDecodeEnvelopeUnreadable(t *testing.T) {
	_, err := decodeEnvelope([]byte("panic: boom\n"), []string{"agent", "list"})
	if err == nil || !strings.Contains(err.Error(), "unreadable response") {
		t.Fatalf("decodeEnvelope error = %v", err)
	}
}
