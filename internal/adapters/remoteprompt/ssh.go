package remoteprompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// remoteCommandTimeout bounds one ssh invocation of the remote prompt flow.
// The commands are short; the bound exists because ssh is slow to give up on
// a machine that drops packets rather than refusing, and a prompt that
// hangs holds the operator's message queue with it.
const remoteCommandTimeout = 30 * time.Second

// Runner runs one local command by argv (the ssh client) and returns both
// streams, so a caller can parse what the remote printed even when the
// command failed. Production is system.ExecRunner; tests substitute a fake.
type Runner interface {
	Run(ctx context.Context, argv []string) (stdout, stderr []byte, err error)
}

// validToken reports whether s is a safe identifier for an ssh destination,
// a remote pane id or a terminal id: one shell word of ordinary characters
// that never reads as an option. Snapshot and config files are edited by
// hand and the remote answers come from another machine, so everything that
// reaches an ssh argv is checked before it is used; anything else is not
// routed and never quoted-and-hoped.
func validToken(s string) bool {
	if s == "" || len(s) > 256 || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == ':' || c == '@':
		default:
			return false
		}
	}
	return true
}

// shellQuote quotes s for one POSIX shell word. Anything outside the safe
// set is wrapped in single quotes with embedded quotes escaped, so a prompt
// text of any shape - newlines, quotes, semicolons - stays one argument and
// is never read as a command.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '=' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCommand renders one herdr invocation as a single shell command
// string, because ssh joins its trailing arguments and hands them to the
// remote login shell. Every part is quoted. The socket overrides are
// cleared first: they beat HERDR_SESSION, and one forwarded by an ssh
// SendEnv rule or left in a remote profile would point the command at the
// wrong session.
func remoteCommand(bin string, args []string, session string) string {
	parts := []string{"env", "-u", "HERDR_SOCKET_PATH", "-u", "HERDR_CLIENT_SOCKET_PATH"}
	if session != "" {
		parts = append(parts, "HERDR_SESSION="+shellQuote(session))
	}
	parts = append(parts, shellQuote(bin))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// sshArgv builds the ssh invocation that runs command on target. It stays
// non-interactive (BatchMode), so an unreachable host fails fast instead of
// waiting for a password that will never be typed. The ControlMaster socket
// is the one the remote-panes plugin derives for the same target and
// session, so a connection it keeps open is reused and a prompt costs one
// round trip; with ControlMaster=auto ssh also opens one when there is
// none. "--" keeps a destination that begins with a dash from being read as
// an option.
func sshArgv(target, controlPath, command string) []string {
	return []string{
		"ssh",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath,
		"-o", "ControlPersist=120",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"--", target, command,
	}
}

// controlPath derives the ControlMaster socket name for a target and
// session exactly as the remote-panes plugin does, so both share one master
// connection per host.
func controlPath(target, session string) string {
	sum := sha256.Sum256([]byte(target + "\x00" + session))
	return filepath.Join(os.TempDir(), fmt.Sprintf("hrp-%s.sock", hex.EncodeToString(sum[:6])))
}
