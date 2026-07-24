package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var errRunnerStopped = errors.New("runner stopped")

// Runner executes and supervises subprocesses.
type Runner struct {
	mu       sync.Mutex
	active   map[int]*os.Process
	stopped  bool
	shutdown chan struct{}
	stopOnce sync.Once

	signalChannel <-chan os.Signal
	ownedSignals  chan os.Signal
	mutationGuard MutationGuard
}

// New constructs a Runner. By default it watches interrupt and termination
// signals and uses GitMutationGuard for mutating commands.
func New(options ...Option) *Runner {
	settings := runnerOptions{
		handleSignals: true,
		mutationGuard: GitMutationGuard{},
	}
	for _, option := range options {
		option(&settings)
	}

	runner := &Runner{
		active:        make(map[int]*os.Process),
		shutdown:      make(chan struct{}),
		mutationGuard: settings.mutationGuard,
	}

	if settings.handleSignals {
		signals := settings.signalChannel
		if signals == nil {
			owned := make(chan os.Signal, 1)
			signal.Notify(owned, terminationSignals()...)
			runner.ownedSignals = owned
			signals = owned
		}
		runner.signalChannel = signals
		go runner.watchSignals()
	}

	return runner
}

// Close stops the Runner and kills every tracked process group. It is
// idempotent.
func (runner *Runner) Close() error {
	runner.stop()
	return nil
}

// Run executes a logical step and applies the retry policy for its effect.
func (runner *Runner) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	normalized, err := normalizeRequest(request)
	if err != nil {
		return RunResult{}, err
	}

	result := RunResult{
		Exit:      -1,
		StdoutLog: normalized.StdoutLog,
		StderrLog: normalized.StderrLog,
	}
	if runner.isStopped() {
		return result, &InterruptedError{Result: result, Err: errRunnerStopped}
	}

	attempts := 1
	if normalized.Effect != EffectMutating {
		attempts += normalized.MaxRetries
	}

	var canonicalSnapshot map[string][]byte
	if normalized.Effect != EffectMutating && len(normalized.CanonicalPaths) > 0 {
		canonicalSnapshot, err = snapshotCanonicalPaths(normalized.CanonicalPaths)
		if err != nil {
			return result, &SafetyError{
				Attempt: 1,
				Result:  result,
				Err:     fmt.Errorf("capture canonical input: %w", err),
			}
		}
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := contextOrShutdownError(ctx, runner.shutdown); err != nil {
			return result, &InterruptedError{
				Attempt: attempt,
				Result:  result,
				Err:     err,
			}
		}

		if normalized.Effect != EffectMutating {
			if err := clearOutputPaths(normalized.OutputPaths); err != nil {
				lastErr = &ExecutionError{
					Attempt: attempt,
					Result:  result,
					Err:     fmt.Errorf("prepare output paths: %w", err),
				}
				if attempt < attempts {
					continue
				}
				return result, lastErr
			}
		}

		if normalized.PrepareAttempt != nil {
			if err := normalized.PrepareAttempt(ctx, attempt); err != nil {
				lastErr = &ExecutionError{
					Attempt: attempt,
					Result:  result,
					Err:     fmt.Errorf("prepare attempt: %w", err),
				}
				if canonicalErr := verifyCanonicalPaths(canonicalSnapshot); canonicalErr != nil {
					return result, &SafetyError{
						Attempt: attempt,
						Result:  result,
						Err: errors.Join(
							lastErr,
							fmt.Errorf("canonical input changed during attempt preparation: %w", canonicalErr),
						),
					}
				}
				if normalized.Effect != EffectMutating && attempt < attempts {
					continue
				}
				return result, lastErr
			}
		}
		if err := verifyCanonicalPaths(canonicalSnapshot); err != nil {
			return result, &SafetyError{
				Attempt: attempt,
				Result:  result,
				Err:     fmt.Errorf("canonical input changed before subprocess start: %w", err),
			}
		}

		var snapshot MutationSnapshot
		if normalized.Effect == EffectMutating {
			snapshot, err = runner.mutationGuard.Capture(ctx, normalized.Dir)
			if err != nil {
				return result, &SafetyError{
					Attempt: attempt,
					Result:  result,
					Err:     fmt.Errorf("capture starting snapshot: %w", err),
				}
			}
		}

		result, lastErr = runner.runAttempt(ctx, normalized, attempt)
		if err := verifyCanonicalPaths(canonicalSnapshot); err != nil {
			return result, &SafetyError{
				Attempt: attempt,
				Result:  result,
				Err: errors.Join(
					lastErr,
					fmt.Errorf("canonical input changed during subprocess: %w", err),
				),
			}
		}
		if lastErr == nil && normalized.ValidateResult != nil {
			if validationErr := normalized.ValidateResult(ctx, result); validationErr != nil {
				lastErr = &ResultError{
					Attempt: attempt,
					Result:  result,
					Err:     validationErr,
				}
			}
		}
		if err := verifyCanonicalPaths(canonicalSnapshot); err != nil {
			return result, &SafetyError{
				Attempt: attempt,
				Result:  result,
				Err: errors.Join(
					lastErr,
					fmt.Errorf("canonical input changed during result validation: %w", err),
				),
			}
		}
		if lastErr == nil {
			return result, nil
		}

		if normalized.Effect == EffectMutating {
			verifyContext := context.WithoutCancel(ctx)
			if verifyErr := runner.mutationGuard.Verify(verifyContext, normalized.Dir, snapshot); verifyErr != nil {
				return result, &SafetyError{
					Attempt: attempt,
					Result:  result,
					Err: errors.Join(
						fmt.Errorf("original command failure: %w", lastErr),
						fmt.Errorf("starting snapshot no longer matches: %w", verifyErr),
					),
				}
			}
			// A mutating command is never retried automatically, even when the
			// starting snapshot still matches.
			return result, lastErr
		}

		var interrupted *InterruptedError
		if errors.As(lastErr, &interrupted) {
			return result, lastErr
		}
	}

	return result, lastErr
}

func (runner *Runner) runAttempt(ctx context.Context, request RunRequest, attempt int) (RunResult, error) {
	result := RunResult{
		Exit:      -1,
		StdoutLog: request.StdoutLog,
		StderrLog: request.StderrLog,
	}

	stdoutLog, err := openLog(request.StdoutLog)
	if err != nil {
		return result, &ExecutionError{Attempt: attempt, Result: result, Err: err}
	}
	defer stdoutLog.Close()

	stderrLog, err := openLog(request.StderrLog)
	if err != nil {
		return result, &ExecutionError{Attempt: attempt, Result: result, Err: err}
	}
	defer stderrLog.Close()

	command := exec.Command(request.Command, request.Args...)
	command.Dir = request.Dir
	command.Env = mergedEnvironment(request.Env)
	if request.Stdin != nil {
		command.Stdin = bytes.NewReader(request.Stdin)
	}
	configureProcess(command)

	activity := make(chan struct{}, 1)
	var lineMu sync.Mutex
	stdout := newStreamWriter(stdoutLog, StreamStdout, attempt, activity, request.OnLine, &lineMu)
	stderr := newStreamWriter(stderrLog, StreamStderr, attempt, activity, request.OnLine, &lineMu)
	command.Stdout = stdout
	command.Stderr = stderr

	if err := command.Start(); err != nil {
		return result, &ExecutionError{
			Attempt: attempt,
			Result:  result,
			Err:     fmt.Errorf("start %q: %w", request.Command, err),
		}
	}

	if !runner.track(command.Process) {
		killErr := killProcessTree(command.Process)
		waitErr := command.Wait()
		stdout.Flush()
		stderr.Flush()
		return result, &InterruptedError{
			Attempt: attempt,
			Result:  result,
			Err:     errors.Join(errRunnerStopped, killErr, waitErr),
		}
	}
	defer runner.untrack(command.Process.Pid)

	waitChannel := make(chan error, 1)
	go func() {
		waitChannel <- command.Wait()
	}()

	waitErr, timedOut, interruptErr := runner.wait(
		ctx,
		waitChannel,
		activity,
		request.IdleTimeout,
		command.Process,
	)
	stdout.Flush()
	stderr.Flush()

	result.Exit = processExitCode(waitErr)
	result.TimedOut = timedOut

	if interruptErr != nil {
		return result, &InterruptedError{
			Attempt: attempt,
			Result:  result,
			Err:     errors.Join(interruptErr, waitErr),
		}
	}
	if timedOut {
		return result, &TimeoutError{
			Attempt: attempt,
			Result:  result,
			Err:     waitErr,
		}
	}
	if waitErr == nil {
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return result, &ExitError{
			Attempt: attempt,
			Result:  result,
			Err:     waitErr,
		}
	}
	return result, &ExecutionError{
		Attempt: attempt,
		Result:  result,
		Err:     waitErr,
	}
}

func (runner *Runner) wait(
	ctx context.Context,
	waitChannel <-chan error,
	activity <-chan struct{},
	idleTimeout time.Duration,
	process *os.Process,
) (error, bool, error) {
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()

	for {
		select {
		case waitErr := <-waitChannel:
			if interruptErr := contextOrShutdownError(ctx, runner.shutdown); interruptErr != nil {
				return waitErr, false, interruptErr
			}
			return waitErr, false, nil
		case <-activity:
			resetTimer(timer, idleTimeout)
		case <-timer.C:
			// Output and process exit at the timeout boundary take precedence
			// over declaring an idle timeout.
			select {
			case <-activity:
				timer.Reset(idleTimeout)
				continue
			default:
			}
			select {
			case waitErr := <-waitChannel:
				return waitErr, false, nil
			default:
			}
			killErr := killProcessTree(process)
			waitErr := <-waitChannel
			return errors.Join(waitErr, killErr), true, nil
		case <-ctx.Done():
			killErr := killProcessTree(process)
			waitErr := <-waitChannel
			return errors.Join(waitErr, killErr), false, ctx.Err()
		case <-runner.shutdown:
			killErr := killProcessTree(process)
			waitErr := <-waitChannel
			return errors.Join(waitErr, killErr), false, errRunnerStopped
		}
	}
}

func (runner *Runner) watchSignals() {
	select {
	case _, ok := <-runner.signalChannel:
		if ok {
			runner.stop()
		}
	case <-runner.shutdown:
	}
}

func (runner *Runner) stop() {
	runner.stopOnce.Do(func() {
		if runner.ownedSignals != nil {
			signal.Stop(runner.ownedSignals)
		}

		runner.mu.Lock()
		runner.stopped = true
		close(runner.shutdown)
		processes := make([]*os.Process, 0, len(runner.active))
		for _, process := range runner.active {
			processes = append(processes, process)
		}
		runner.mu.Unlock()

		for _, process := range processes {
			_ = killProcessTree(process)
		}
	})
}

func (runner *Runner) track(process *os.Process) bool {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.stopped {
		return false
	}
	runner.active[process.Pid] = process
	return true
}

func (runner *Runner) untrack(pid int) {
	runner.mu.Lock()
	delete(runner.active, pid)
	runner.mu.Unlock()
}

func (runner *Runner) isStopped() bool {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.stopped
}

func normalizeRequest(request RunRequest) (RunRequest, error) {
	if request.Command == "" {
		return RunRequest{}, fmt.Errorf("runner: command is required")
	}
	if request.IdleTimeout <= 0 {
		return RunRequest{}, fmt.Errorf("runner: idle timeout must be positive")
	}
	if request.MaxRetries < 0 {
		return RunRequest{}, fmt.Errorf("runner: max retries must not be negative")
	}
	if request.Effect < EffectReadOnly || request.Effect > EffectMutating {
		return RunRequest{}, fmt.Errorf("runner: invalid effect %d", request.Effect)
	}
	if request.StdoutLog == "" || request.StderrLog == "" {
		return RunRequest{}, fmt.Errorf("runner: stdout and stderr log paths are required")
	}
	if request.Effect == EffectMutating && len(request.OutputPaths) != 0 {
		return RunRequest{}, fmt.Errorf("runner: mutating commands cannot declare output paths")
	}
	if request.Effect == EffectMutating && request.PrepareAttempt != nil {
		return RunRequest{}, fmt.Errorf("runner: mutating commands cannot declare an attempt preparation hook")
	}

	dir := request.Dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return RunRequest{}, fmt.Errorf("runner: resolve working directory: %w", err)
		}
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return RunRequest{}, fmt.Errorf("runner: resolve working directory: %w", err)
	}
	request.Dir = filepath.Clean(absoluteDir)

	request.StdoutLog, err = resolvePath(request.Dir, request.StdoutLog)
	if err != nil {
		return RunRequest{}, err
	}
	request.StderrLog, err = resolvePath(request.Dir, request.StderrLog)
	if err != nil {
		return RunRequest{}, err
	}
	if request.StdoutLog == request.StderrLog {
		return RunRequest{}, fmt.Errorf("runner: stdout and stderr logs must be distinct")
	}

	request.OutputPaths, err = resolvePaths(request.Dir, request.OutputPaths)
	if err != nil {
		return RunRequest{}, err
	}
	request.CanonicalPaths, err = resolvePaths(request.Dir, request.CanonicalPaths)
	if err != nil {
		return RunRequest{}, err
	}

	canonical := make(map[string]struct{}, len(request.CanonicalPaths))
	for _, path := range request.CanonicalPaths {
		canonical[path] = struct{}{}
	}
	for _, path := range request.OutputPaths {
		if _, overlaps := canonical[path]; overlaps {
			return RunRequest{}, fmt.Errorf("runner: output path %q overlaps a canonical path", path)
		}
	}
	logPaths := map[string]struct{}{
		request.StdoutLog: {},
		request.StderrLog: {},
	}
	for _, path := range request.OutputPaths {
		if _, overlaps := logPaths[path]; overlaps {
			return RunRequest{}, fmt.Errorf("runner: output path %q overlaps a log path", path)
		}
	}
	for _, path := range request.CanonicalPaths {
		if _, overlaps := logPaths[path]; overlaps {
			return RunRequest{}, fmt.Errorf("runner: canonical path %q overlaps a log path", path)
		}
	}

	request.Args = append([]string(nil), request.Args...)
	request.Stdin = append([]byte(nil), request.Stdin...)
	request.Env = cloneEnvironment(request.Env)
	return request, nil
}

func resolvePaths(dir string, paths []string) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		absolute, err := resolvePath(dir, path)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[absolute]; duplicate {
			continue
		}
		seen[absolute] = struct{}{}
		resolved = append(resolved, absolute)
	}
	return resolved, nil
}

func resolvePath(dir, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("runner: path must not be empty")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("runner: resolve path %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

func clearOutputPaths(paths []string) error {
	for _, path := range paths {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("inspect %q: %w", path, err)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("refusing to remove symlink output %q", path)
		case info.IsDir():
			return fmt.Errorf("refusing to remove directory output %q", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale output %q: %w", path, err)
		}
	}
	return nil
}

func snapshotCanonicalPaths(paths []string) (map[string][]byte, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	snapshot := make(map[string][]byte, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("canonical path is a symlink %q", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("canonical path is not a regular file %q", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", path, err)
		}
		snapshot[path] = content
	}
	return snapshot, nil
}

func verifyCanonicalPaths(snapshot map[string][]byte) error {
	for path, want := range snapshot {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("canonical path is no longer a regular file %q", path)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %q: %w", path, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("canonical file changed %q", path)
		}
	}
	return nil
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory for %q: %w", path, err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing symlink log %q", path)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("log path is a directory %q", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect log %q: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log %q: %w", path, err)
	}
	return file, nil
}

func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return nil
	}
	clone := make(map[string]string, len(environment))
	for key, value := range environment {
		clone[key] = value
	}
	return clone
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func contextOrShutdownError(ctx context.Context, shutdown <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-shutdown:
		return errRunnerStopped
	default:
		return nil
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

type streamWriter struct {
	log      io.Writer
	stream   Stream
	attempt  int
	activity chan<- struct{}
	onLine   func(Line)
	lineMu   *sync.Mutex
	pending  []byte
}

func newStreamWriter(
	log io.Writer,
	stream Stream,
	attempt int,
	activity chan<- struct{},
	onLine func(Line),
	lineMu *sync.Mutex,
) *streamWriter {
	return &streamWriter{
		log:      log,
		stream:   stream,
		attempt:  attempt,
		activity: activity,
		onLine:   onLine,
		lineMu:   lineMu,
	}
}

func (writer *streamWriter) Write(data []byte) (int, error) {
	if len(data) > 0 {
		select {
		case writer.activity <- struct{}{}:
		default:
		}
	}
	written, err := writer.log.Write(data)
	if written > 0 {
		writer.pending = append(writer.pending, data[:written]...)
		writer.emitCompleteLines()
	}
	return written, err
}

func (writer *streamWriter) Flush() {
	if len(writer.pending) == 0 {
		return
	}
	writer.emit(writer.pending)
	writer.pending = nil
}

func (writer *streamWriter) emitCompleteLines() {
	for {
		index := bytes.IndexByte(writer.pending, '\n')
		if index < 0 {
			return
		}
		writer.emit(writer.pending[:index])
		writer.pending = writer.pending[index+1:]
	}
}

func (writer *streamWriter) emit(line []byte) {
	if writer.onLine == nil {
		return
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	writer.lineMu.Lock()
	writer.onLine(Line{
		Attempt: writer.attempt,
		Stream:  writer.stream,
		Text:    string(line),
	})
	writer.lineMu.Unlock()
}
