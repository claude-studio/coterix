package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ridenow/coterix/internal/runner"
)

// FailureKind preserves the runner failure class after CLI-specific
// classification.
type FailureKind string

const (
	FailureExit        FailureKind = "exit"
	FailureTimeout     FailureKind = "timeout"
	FailureResult      FailureKind = "result"
	FailureInterrupted FailureKind = "interrupted"
	FailureSafety      FailureKind = "safety"
	FailureExecution   FailureKind = "execution"
	FailureUnknown     FailureKind = "failed"
)

// AuthFailure is a best-effort classification of a nonzero CLI exit whose
// stderr matches a known provider authentication message.
type AuthFailure struct {
	CLI  string
	Kind FailureKind
	Err  error
}

func (failure *AuthFailure) Error() string {
	return fmt.Sprintf("cli: %s authentication failed: %v", failure.CLI, failure.Err)
}

func (failure *AuthFailure) Unwrap() error {
	return failure.Err
}

// GenericFailure is the fallback for failures that are not known auth errors.
type GenericFailure struct {
	Kind FailureKind
	Err  error
}

func (failure *GenericFailure) Error() string {
	return fmt.Sprintf("cli: command failed (%s): %v", failure.Kind, failure.Err)
}

func (failure *GenericFailure) Unwrap() error {
	return failure.Err
}

// ClassifyFailure distinguishes known CLI authentication exits from generic
// failures. It does not probe credentials or create a separate auth subsystem.
func ClassifyFailure(cliName string, err error, stderr []byte) error {
	if err == nil {
		return nil
	}
	kind := runnerFailureKind(err)
	if kind == FailureExit && knownAuthFailure(cliName, stderr) {
		return &AuthFailure{CLI: cliName, Kind: kind, Err: err}
	}
	return &GenericFailure{Kind: kind, Err: err}
}

func runnerFailureKind(err error) FailureKind {
	var safety *runner.SafetyError
	if errors.As(err, &safety) {
		return FailureSafety
	}
	var interrupted *runner.InterruptedError
	if errors.As(err, &interrupted) {
		return FailureInterrupted
	}
	var timeout *runner.TimeoutError
	if errors.As(err, &timeout) {
		return FailureTimeout
	}
	var result *runner.ResultError
	if errors.As(err, &result) {
		return FailureResult
	}
	var exit *runner.ExitError
	if errors.As(err, &exit) {
		return FailureExit
	}
	var execution *runner.ExecutionError
	if errors.As(err, &execution) {
		return FailureExecution
	}
	return FailureUnknown
}

func knownAuthFailure(cliName string, stderr []byte) bool {
	const maxSignatureBytes = 64 << 10
	if len(stderr) > maxSignatureBytes {
		stderr = stderr[len(stderr)-maxSignatureBytes:]
	}
	cleaned := ansiSequencePattern.ReplaceAllString(string(stderr), "")
	lines := strings.Split(cleaned, "\n")
	for _, line := range lines {
		normalized := strings.ToLower(strings.TrimSpace(line))
		normalized = strings.TrimSpace(strings.TrimPrefix(normalized, "error:"))
		if normalized == "" {
			continue
		}
		switch strings.ToLower(cliName) {
		case "claude":
			if claudeAuthLine(normalized) {
				return true
			}
		case "codex":
			if codexAuthLine(normalized) {
				return true
			}
		}
	}
	return false
}

func claudeAuthLine(line string) bool {
	switch line {
	case "not logged in",
		"please run /login",
		"invalid api key",
		"authentication failed: invalid or missing api key":
		return true
	}
	return strings.HasPrefix(line, "not logged in. run claude auth login to authenticate") ||
		strings.HasPrefix(line, "not logged in · please run /login") ||
		strings.HasPrefix(line, "api error: 401 invalid api key · please run /login") ||
		strings.HasPrefix(line, "invalid api key · please run /login") ||
		strings.HasPrefix(line, "your session has expired. please run /login") ||
		strings.HasPrefix(line, "failed to authenticate: oauth session expired")
}

func codexAuthLine(line string) bool {
	switch line {
	case "not logged in", "auth: not logged in":
		return true
	}
	return strings.HasPrefix(line, "api key auth is missing a key") ||
		strings.HasPrefix(line, "auth data is not available") ||
		strings.HasPrefix(line, "your access token could not be refreshed") ||
		strings.HasPrefix(line, "refresh token has expired") ||
		strings.HasPrefix(line, "the refresh token has expired") ||
		strings.HasPrefix(line, "refresh token has been revoked") ||
		strings.HasPrefix(line, "the refresh token has been revoked")
}

var ansiSequencePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
