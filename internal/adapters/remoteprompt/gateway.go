// Package remoteprompt decorates the Herdr gateway so a prompt aimed at a
// local pane that mirrors a remote terminal (the poorplebs.remote-panes
// plugin's attach mirrors) is typed into the agent on the machine that owns
// the terminal, over SSH.
//
// The mirror is needed because Herdr refuses to prompt an agent that is not
// the foreground process of its pane, and a mirror pane's foreground process
// is the mirror client, not the agent: the agent lives in the remote
// terminal. The decorator therefore reverses the remote-panes snapshot
// (local pane -> host and remote terminal), asks the remote herdr which
// agent holds that terminal, and prompts it there. Prompts to panes that
// are no mirror go to the local Herdr unchanged, and so does every other
// gateway call.
package remoteprompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/permgps/herdr-telegram-agents/internal/domain"
)

// Gateway decorates a domain.HerdrGateway. Only Prompt is intercepted;
// every other method, and raw key presses in particular, deliberately go to
// the decorated gateway: keys are typed blind, and the mirror pane's local
// process is the right place for whatever the application sends today.
type Gateway struct {
	domain.HerdrGateway
	resolver Resolver
	runner   Runner
	log      *slog.Logger
}

var _ domain.HerdrGateway = (*Gateway)(nil)

// NewGateway wraps inner with the remote prompt routing.
func NewGateway(inner domain.HerdrGateway, resolver Resolver, runner Runner, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Gateway{HerdrGateway: inner, resolver: resolver, runner: runner, log: log}
}

// Prompt routes the prompt to the remote agent when target is a local pane
// mirroring a remote terminal, and to the local Herdr otherwise.
func (g *Gateway) Prompt(ctx context.Context, target, text string) error {
	t, ok, err := g.resolver.Mirror(target)
	if err != nil {
		return fmt.Errorf("prompt %s: %w", target, err)
	}
	if !ok {
		return g.HerdrGateway.Prompt(ctx, target, text)
	}
	return g.promptRemote(ctx, target, t, text)
}

// promptRemote runs the two remote calls: find the agent holding the
// mirrored terminal, then prompt it there.
func (g *Gateway) promptRemote(ctx context.Context, pane string, t Target, text string) error {
	if !validToken(t.Host) {
		return fmt.Errorf("prompt %s: unusable ssh target %q", pane, t.Host)
	}
	if !validToken(t.Terminal) {
		return fmt.Errorf("prompt %s: unusable terminal id %q", pane, t.Terminal)
	}
	if t.Bin == "" || hasControl(t.Bin) {
		return fmt.Errorf("prompt %s: unusable herdr binary %q", pane, t.Bin)
	}
	if text == "" {
		return errors.New("prompt: text is empty")
	}
	c := remoteClient{runner: g.runner, target: t.Host, session: t.Session, bin: t.Bin, log: g.log}
	remotePane, err := c.agentPane(ctx, t.Terminal)
	if err != nil {
		return err
	}
	if err := c.prompt(ctx, remotePane, text); err != nil {
		return err
	}
	g.log.Info("prompt routed to remote agent",
		slog.String("pane", pane), slog.String("host", t.Host),
		slog.String("remote_terminal", t.Terminal), slog.String("remote_pane", remotePane))
	return nil
}

// hasControl reports whether s contains characters a shell would treat as
// control input. The binary path is still shell-quoted; this keeps a
// hand-edited config from carrying something with a carriage return in it
// at all.
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// remoteClient runs herdr CLI commands for one remote host over ssh.
type remoteClient struct {
	runner  Runner
	target  string
	session string
	bin     string
	log     *slog.Logger
}

// run executes one remote herdr command and unwraps its JSON envelope. A
// refusal the remote herdr printed travels with its error code rather than
// as a bare exit status.
func (c *remoteClient) run(ctx context.Context, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteCommandTimeout)
	defer cancel()
	command := remoteCommand(c.bin, args, c.session)
	argv := sshArgv(c.target, controlPath(c.target, c.session), command)
	c.log.Debug("remote herdr call", slog.String("host", c.target), slog.String("args", fmt.Sprint(args)))
	out, _, err := c.runner.Run(ctx, argv)
	if err != nil {
		// The envelope, when there is one, says more than the exit status.
		if _, decErr := decodeEnvelope(out, args); decErr != nil {
			var apiErr *remoteAPIError
			if errors.As(decErr, &apiErr) {
				if apiErr.notFound() {
					return nil, fmt.Errorf("%s: %w: %w", c.target, apiErr, domain.ErrAgentGone)
				}
				return nil, fmt.Errorf("%s: %w", c.target, apiErr)
			}
		}
		return nil, fmt.Errorf("%s: %w", c.target, err)
	}
	res, decErr := decodeEnvelope(out, args)
	if decErr != nil {
		return nil, fmt.Errorf("%s: %w", c.target, decErr)
	}
	return res, nil
}

// agentListResult is the part of `herdr agent list` this package reads.
type agentListResult struct {
	Agents []struct {
		Agent      string `json:"agent"`
		PaneID     string `json:"pane_id"`
		TerminalID string `json:"terminal_id"`
	} `json:"agents"`
}

// agentPane returns the pane id of the remote agent whose terminal the
// mirrored pane shows. A terminal no agent holds is ErrAgentGone: there is
// nothing to prompt.
func (c *remoteClient) agentPane(ctx context.Context, terminal string) (string, error) {
	res, err := c.run(ctx, "agent", "list")
	if err != nil {
		return "", fmt.Errorf("agent list: %w", err)
	}
	if len(res) == 0 {
		return "", errors.New("agent list: empty response")
	}
	var list agentListResult
	if err := json.Unmarshal(res, &list); err != nil {
		return "", fmt.Errorf("agent list: %w", err)
	}
	for _, a := range list.Agents {
		if a.TerminalID != terminal {
			continue
		}
		if !validToken(a.PaneID) {
			return "", fmt.Errorf("agent list: unusable pane id %q for terminal %s", a.PaneID, terminal)
		}
		return a.PaneID, nil
	}
	return "", fmt.Errorf("agent list: %w: no agent on terminal %s", domain.ErrAgentGone, terminal)
}

// prompt submits the text to the remote agent in its pane.
func (c *remoteClient) prompt(ctx context.Context, pane, text string) error {
	if text == "" {
		return errors.New("agent prompt: text is empty")
	}
	if _, err := c.run(ctx, "agent", "prompt", pane, text); err != nil {
		return fmt.Errorf("agent prompt: %w", err)
	}
	return nil
}
