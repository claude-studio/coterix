package runner

import (
	"context"
	"os"
	"time"
)

// Effect describes whether an attempted command can change canonical state.
type Effect uint8

const (
	// EffectReadOnly is used by reviewers. Failed attempts may be retried.
	EffectReadOnly Effect = iota + 1
	// EffectArtifactOnly writes a separate attempt artifact while preserving its
	// canonical input. Failed attempts may be retried.
	EffectArtifactOnly
	// EffectMutating may change the repository. It is never retried automatically.
	EffectMutating
)

// Stream identifies a subprocess output stream.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// Line is emitted as subprocess output becomes available.
type Line struct {
	Attempt int
	Stream  Stream
	Text    string
}

// RunRequest describes one logical subprocess step. MaxRetries is the number
// of additional attempts after the initial attempt.
type RunRequest struct {
	Command string
	Args    []string
	Dir     string
	Env     map[string]string
	Stdin   []byte

	IdleTimeout time.Duration
	MaxRetries  int
	Effect      Effect

	StdoutLog string
	StderrLog string

	// OutputPaths are removed before every read-only or artifact-only attempt.
	// CanonicalPaths are never removed and must not overlap OutputPaths.
	OutputPaths    []string
	CanonicalPaths []string

	// PrepareAttempt runs after safe output cleanup and before a read-only or
	// artifact-only subprocess. Mutating commands cannot use this hook because
	// worktree preparation would precede their trusted snapshot.
	PrepareAttempt func(context.Context, int) error
	// ValidateResult validates role-specific output after a zero exit. A
	// validation failure is returned as ResultError and can trigger a safe retry.
	ValidateResult func(context.Context, RunResult) error

	// OnLine receives stdout and stderr lines serially. The trailing newline is
	// omitted; a final unterminated line is still delivered.
	OnLine func(Line)
}

// RunResult contains hard subprocess evidence and paths to its persisted logs.
type RunResult struct {
	Exit      int
	TimedOut  bool
	StdoutLog string
	StderrLog string
}

// MutationSnapshot is the repository state captured immediately before a
// mutating subprocess starts.
type MutationSnapshot struct {
	Head string
}

// MutationGuard captures and verifies the HEAD/clean precondition used to
// fail safely after a mutating command fails.
type MutationGuard interface {
	Capture(context.Context, string) (MutationSnapshot, error)
	Verify(context.Context, string, MutationSnapshot) error
}

type runnerOptions struct {
	handleSignals bool
	signalChannel <-chan os.Signal
	mutationGuard MutationGuard
}

// Option configures a Runner.
type Option func(*runnerOptions)

// WithoutSignalHandling disables the default interrupt/termination watcher.
// Context cancellation and Close still terminate active children.
func WithoutSignalHandling() Option {
	return func(options *runnerOptions) {
		options.handleSignals = false
		options.signalChannel = nil
	}
}

// WithSignalChannel replaces the OS signal subscription. It is primarily
// useful to connect an existing supervisor or a deterministic test channel.
func WithSignalChannel(signals <-chan os.Signal) Option {
	return func(options *runnerOptions) {
		options.handleSignals = true
		options.signalChannel = signals
	}
}

// WithMutationGuard replaces the default git-backed mutation guard.
func WithMutationGuard(guard MutationGuard) Option {
	return func(options *runnerOptions) {
		if guard != nil {
			options.mutationGuard = guard
		}
	}
}
