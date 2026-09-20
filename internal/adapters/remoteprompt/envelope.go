package remoteprompt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// cliEnvelope is the JSON envelope every herdr CLI command prints around
// its answer or its refusal.
type cliEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *cliErrorBody   `json:"error"`
}

// cliErrorBody is the error Herdr puts in that envelope.
type cliErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// remoteAPIError is a refusal the remote herdr printed in its envelope.
type remoteAPIError struct {
	command string
	code    string
	message string
}

func (e *remoteAPIError) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", e.command, e.code, e.message)
}

// notFound reports whether the refusal says the thing asked about does not
// exist, which maps to the domain's ErrAgentGone.
func (e *remoteAPIError) notFound() bool {
	return strings.HasSuffix(e.code, "_not_found")
}

// decodeEnvelope unwraps the JSON envelope a herdr CLI command printed. The
// response is found line by line, because herdr prints the occasional
// notice around its JSON; the last envelope wins. Empty output is no result
// and no error, which callers report as an empty response.
func decodeEnvelope(out []byte, args []string) (json.RawMessage, error) {
	command := strings.Join(args, " ")
	var (
		env   cliEnvelope
		found bool
	)
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var candidate cliEnvelope
		if err := json.Unmarshal(line, &candidate); err != nil {
			continue
		}
		if candidate.Result == nil && candidate.Error == nil {
			continue
		}
		env, found = candidate, true
	}
	if !found {
		trimmed := bytes.TrimSpace(out)
		if len(trimmed) == 0 {
			return nil, nil
		}
		// Nothing on a line of its own parsed, so try the whole output: a
		// response printed across several lines is still a response.
		var whole cliEnvelope
		if err := json.Unmarshal(trimmed, &whole); err == nil && (whole.Result != nil || whole.Error != nil) {
			env, found = whole, true
		}
	}
	if !found {
		return nil, fmt.Errorf("herdr %s: unreadable response: %s", command, truncateForError(out))
	}
	if env.Error != nil {
		return nil, &remoteAPIError{command: command, code: env.Error.Code, message: env.Error.Message}
	}
	return env.Result, nil
}

// truncateForError keeps an unreadable response to a fragment for the error
// message. It counts characters, not bytes, so a cut never splits a rune.
func truncateForError(out []byte) string {
	const max = 200
	runes := []rune(string(out))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "…"
}
