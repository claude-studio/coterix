package cli

import (
	"errors"
	"testing"

	"github.com/ridenow/coterix/internal/runner"
)

func TestClassifyFailureRecognizesKnownAuthSignatures(t *testing.T) {
	exitErr := &runner.ExitError{
		Attempt: 1,
		Result:  runner.RunResult{Exit: 1},
		Err:     errors.New("exit status 1"),
	}
	tests := []struct {
		name   string
		cli    string
		stderr string
	}{
		{
			name:   "claude login",
			cli:    "claude",
			stderr: "\x1b[31mError: Not logged in. Run claude auth login to authenticate.\x1b[0m",
		},
		{
			name:   "claude invalid key",
			cli:    "claude",
			stderr: "Authentication failed: invalid or missing API key",
		},
		{
			name:   "claude API error invalid key",
			cli:    "claude",
			stderr: "API Error: 401 Invalid API key · Please run /login",
		},
		{
			name:   "claude expired session",
			cli:    "claude",
			stderr: "Your session has expired. Please run /login to authenticate.",
		},
		{
			name:   "claude expired OAuth",
			cli:    "claude",
			stderr: "Failed to authenticate: OAuth session expired. Please run /login.",
		},
		{
			name:   "codex missing key",
			cli:    "codex",
			stderr: "API key auth is missing a key.",
		},
		{
			name:   "codex not logged in",
			cli:    "codex",
			stderr: "Auth: Not logged in",
		},
		{
			name:   "codex expired refresh token",
			cli:    "codex",
			stderr: "The refresh token has expired. Please log out and sign in again.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := ClassifyFailure(test.cli, exitErr, []byte(test.stderr))
			var authErr *AuthFailure
			if !errors.As(classified, &authErr) {
				t.Fatalf("ClassifyFailure() = %T %v, want *AuthFailure", classified, classified)
			}
			if authErr.Kind != FailureExit || !errors.Is(classified, exitErr.Err) {
				t.Fatalf("AuthFailure = %#v", authErr)
			}
		})
	}
}

func TestClassifyFailureFallsBackForNonAuthAndNonExitFailures(t *testing.T) {
	tests := []struct {
		name     string
		cli      string
		err      error
		stderr   string
		wantKind FailureKind
	}{
		{
			name: "generic exit",
			cli:  "codex",
			err: &runner.ExitError{
				Result: runner.RunResult{Exit: 2},
				Err:    errors.New("exit status 2"),
			},
			stderr:   "compilation failed",
			wantKind: FailureExit,
		},
		{
			name: "git auth near miss",
			cli:  "claude",
			err: &runner.ExitError{
				Result: runner.RunResult{Exit: 1},
				Err:    errors.New("exit status 1"),
			},
			stderr:   "Git authentication failed",
			wantKind: FailureExit,
		},
		{
			name: "MCP login near miss",
			cli:  "claude",
			err: &runner.ExitError{
				Result: runner.RunResult{Exit: 1},
				Err:    errors.New("exit status 1"),
			},
			stderr:   "MCP server is not logged in",
			wantKind: FailureExit,
		},
		{
			name: "MCP Claude key near miss",
			cli:  "claude",
			err: &runner.ExitError{
				Result: runner.RunResult{Exit: 1},
				Err:    errors.New("exit status 1"),
			},
			stderr:   "MCP server authentication failed: invalid or missing API key",
			wantKind: FailureExit,
		},
		{
			name: "MCP Codex auth near miss",
			cli:  "codex",
			err: &runner.ExitError{
				Result: runner.RunResult{Exit: 1},
				Err:    errors.New("exit status 1"),
			},
			stderr:   "MCP github: auth data is not available",
			wantKind: FailureExit,
		},
		{
			name: "quota is not auth",
			cli:  "codex",
			err: &runner.ExitError{
				Result: runner.RunResult{Exit: 1},
				Err:    errors.New("exit status 1"),
			},
			stderr:   "usage limit exceeded; add credits",
			wantKind: FailureExit,
		},
		{
			name: "timeout keeps typed meaning",
			cli:  "claude",
			err: &runner.TimeoutError{
				Result: runner.RunResult{TimedOut: true},
				Err:    errors.New("killed"),
			},
			stderr:   "Not logged in",
			wantKind: FailureTimeout,
		},
		{
			name: "result error keeps typed meaning",
			cli:  "codex",
			err: &runner.ResultError{
				Err: errors.New("malformed JSON"),
			},
			stderr:   "API key auth is missing a key.",
			wantKind: FailureResult,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := ClassifyFailure(test.cli, test.err, []byte(test.stderr))
			var authErr *AuthFailure
			if errors.As(classified, &authErr) {
				t.Fatalf("ClassifyFailure() returned auth error: %v", classified)
			}
			var genericErr *GenericFailure
			if !errors.As(classified, &genericErr) {
				t.Fatalf("ClassifyFailure() = %T %v, want *GenericFailure", classified, classified)
			}
			if genericErr.Kind != test.wantKind {
				t.Fatalf("GenericFailure.Kind = %q, want %q", genericErr.Kind, test.wantKind)
			}
		})
	}
}

func TestClassifyFailureIgnoresAuthTextOnSuccess(t *testing.T) {
	if got := ClassifyFailure("claude", nil, []byte("Not logged in")); got != nil {
		t.Fatalf("ClassifyFailure(nil) = %v, want nil", got)
	}
}
