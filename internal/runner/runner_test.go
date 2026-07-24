package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const helperEnvironment = "COTERIX_RUNNER_HELPER"

func TestRunnerHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvironment) != "1" {
		return
	}

	args := helperArguments(os.Args)
	if len(args) == 0 {
		helperExit("missing helper scenario")
	}

	switch args[0] {
	case "success":
		_, _ = fmt.Fprint(os.Stdout, "stdout-one\nstdout-tail")
		_, _ = fmt.Fprint(os.Stderr, "stderr-one\nstderr-two\n")
		os.Exit(0)
	case "nonzero":
		_, _ = fmt.Fprintln(os.Stderr, "intentional failure")
		os.Exit(17)
	case "hang":
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		for {
			time.Sleep(time.Hour)
		}
	case "keepalive":
		if len(args) != 4 {
			helperExit("keepalive requires count, interval, and stream")
		}
		count, err := strconv.Atoi(args[1])
		if err != nil {
			helperExit(err.Error())
		}
		intervalMillis, err := strconv.Atoi(args[2])
		if err != nil {
			helperExit(err.Error())
		}
		output := os.Stdout
		if args[3] == string(StreamStderr) {
			output = os.Stderr
		}
		for index := 0; index < count; index++ {
			_, _ = fmt.Fprintf(output, "tick-%d\n", index)
			time.Sleep(time.Duration(intervalMillis) * time.Millisecond)
		}
		os.Exit(0)
	case "retry-output":
		if len(args) != 4 {
			helperExit("retry-output requires output, counter, and canonical paths")
		}
		outputPath, counterPath, canonicalPath := args[1], args[2], args[3]
		if _, err := os.Lstat(outputPath); err == nil {
			os.Exit(91)
		} else if !errors.Is(err, os.ErrNotExist) {
			helperExit(err.Error())
		}
		if canonicalPath != "-" {
			content, err := os.ReadFile(canonicalPath)
			if err != nil {
				helperExit(err.Error())
			}
			if string(content) != "canonical" {
				os.Exit(92)
			}
		}
		attempt := incrementCounter(counterPath)
		payload := []byte(`{"ok":`)
		if attempt > 1 {
			payload = []byte(`{"ok":true}`)
		}
		if err := os.WriteFile(outputPath, payload, 0o600); err != nil {
			helperExit(err.Error())
		}
		os.Exit(0)
	case "count-and-fail":
		if len(args) != 2 {
			helperExit("count-and-fail requires a counter path")
		}
		incrementCounter(args[1])
		os.Exit(17)
	case "count-and-hang":
		if len(args) != 2 {
			helperExit("count-and-hang requires a counter path")
		}
		incrementCounter(args[1])
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		for {
			time.Sleep(time.Hour)
		}
	case "mutate-canonical":
		if len(args) != 4 {
			helperExit("mutate-canonical requires output, counter, and canonical paths")
		}
		incrementCounter(args[2])
		if err := os.WriteFile(args[3], []byte("mutated"), 0o600); err != nil {
			helperExit(err.Error())
		}
		if err := os.WriteFile(args[1], []byte(`{"ok":`), 0o600); err != nil {
			helperExit(err.Error())
		}
		os.Exit(0)
	case "spawn-child":
		if len(args) != 2 {
			helperExit("spawn-child requires a pid path")
		}
		child := exec.Command(os.Args[0], "-test.run=^TestRunnerHelperProcess$", "--", "child-loop")
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			helperExit(err.Error())
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			helperExit(err.Error())
		}
		_, _ = fmt.Fprintln(os.Stdout, "child-ready")
		_ = child.Wait()
		os.Exit(0)
	case "child-loop":
		for {
			time.Sleep(time.Hour)
		}
	default:
		helperExit("unknown helper scenario: " + args[0])
	}
}

func TestRunStreamsLinesAndWritesLogs(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, _ := helperRequest(t, EffectReadOnly, "success")
	var (
		mu    sync.Mutex
		lines []Line
	)
	request.OnLine = func(line Line) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	}

	result, err := executor.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Exit != 0 || result.TimedOut {
		t.Fatalf("Run() result = %+v, want successful exit", result)
	}

	assertFileContent(t, result.StdoutLog, "stdout-one\nstdout-tail")
	assertFileContent(t, result.StderrLog, "stderr-one\nstderr-two\n")

	mu.Lock()
	defer mu.Unlock()
	assertStreamLines(t, lines, StreamStdout, []string{"stdout-one", "stdout-tail"})
	assertStreamLines(t, lines, StreamStderr, []string{"stderr-one", "stderr-two"})
}

func TestRunNonzeroExitIsTyped(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, _ := helperRequest(t, EffectReadOnly, "nonzero")
	result, err := executor.Run(context.Background(), request)

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run() error = %T %v, want *ExitError", err, err)
	}
	if result.Exit != 17 || result.TimedOut {
		t.Fatalf("Run() result = %+v, want exit 17 without timeout", result)
	}
	if exitErr.Attempt != 1 {
		t.Fatalf("ExitError.Attempt = %d, want 1", exitErr.Attempt)
	}
}

func TestRunIdleTimeout(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, _ := helperRequest(t, EffectReadOnly, "hang")
	request.IdleTimeout = 500 * time.Millisecond

	started := time.Now()
	result, err := executor.Run(context.Background(), request)
	elapsed := time.Since(started)

	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Run() error = %T %v, want *TimeoutError", err, err)
	}
	if !result.TimedOut {
		t.Fatalf("Run() result = %+v, want TimedOut", result)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run() returned after %v, want bounded idle termination", elapsed)
	}
}

func TestRunIdleTimeoutIsNotTotalTimeout(t *testing.T) {
	for _, stream := range []Stream{StreamStdout, StreamStderr} {
		t.Run(string(stream), func(t *testing.T) {
			executor := New(WithoutSignalHandling())
			defer executor.Close()

			request, _ := helperRequest(
				t,
				EffectReadOnly,
				"keepalive",
				"8",
				"300",
				string(stream),
			)
			request.IdleTimeout = 2 * time.Second

			started := time.Now()
			result, err := executor.Run(context.Background(), request)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.TimedOut {
				t.Fatalf("Run() result = %+v, want activity to reset timeout", result)
			}
			if elapsed <= request.IdleTimeout {
				t.Fatalf("Run() elapsed = %v, want total runtime above idle timeout %v", elapsed, request.IdleTimeout)
			}
		})
	}
}

func TestRunResultValidationErrorIsTyped(t *testing.T) {
	tests := []struct {
		name     string
		scenario string
	}{
		{name: "missing", scenario: "success"},
		{name: "malformed", scenario: "retry-output"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := New(WithoutSignalHandling())
			defer executor.Close()

			request, dir := helperRequest(t, EffectReadOnly, test.scenario)
			output := filepath.Join(dir, "result.json")
			counter := filepath.Join(dir, "attempts")
			request.OutputPaths = []string{output}
			if test.scenario == "retry-output" {
				request.Args = append(request.Args, output, counter, "-")
			}
			request.ValidateResult = jsonValidator(output)

			_, err := executor.Run(context.Background(), request)
			var resultErr *ResultError
			if !errors.As(err, &resultErr) {
				t.Fatalf("Run() error = %T %v, want *ResultError", err, err)
			}
		})
	}
}

func TestRunReadOnlyClearsOutputAndRetries(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, dir := helperRequest(t, EffectReadOnly, "retry-output")
	output := filepath.Join(dir, "review.json")
	counter := filepath.Join(dir, "attempts")
	if err := os.WriteFile(output, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	request.Args = append(request.Args, output, counter, "-")
	request.OutputPaths = []string{output}
	request.MaxRetries = 1
	request.ValidateResult = jsonValidator(output)

	result, err := executor.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Exit != 0 {
		t.Fatalf("Run() result = %+v, want successful retry", result)
	}
	assertFileContent(t, counter, "2")
	assertFileContent(t, output, `{"ok":true}`)
}

func TestRunArtifactOnlyPreservesCanonicalInput(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, dir := helperRequest(t, EffectArtifactOnly, "retry-output")
	canonical := filepath.Join(dir, "plan.md")
	output := filepath.Join(dir, "plan.next.md")
	counter := filepath.Join(dir, "attempts")
	if err := os.WriteFile(canonical, []byte("canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("partial-stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	request.Args = append(request.Args, output, counter, canonical)
	request.OutputPaths = []string{output}
	request.CanonicalPaths = []string{canonical}
	request.MaxRetries = 1
	request.ValidateResult = jsonValidator(output)

	if _, err := executor.Run(context.Background(), request); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertFileContent(t, canonical, "canonical")
	assertFileContent(t, counter, "2")
	assertFileContent(t, output, `{"ok":true}`)
}

func TestRunArtifactOnlyCanonicalDriftFailsSafeWithoutRetry(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, dir := helperRequest(t, EffectArtifactOnly, "mutate-canonical")
	canonical := filepath.Join(dir, "plan.md")
	output := filepath.Join(dir, "plan.next.md")
	counter := filepath.Join(dir, "attempts")
	if err := os.WriteFile(canonical, []byte("canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	request.Args = append(request.Args, output, counter, canonical)
	request.OutputPaths = []string{output}
	request.CanonicalPaths = []string{canonical}
	request.MaxRetries = 3
	request.ValidateResult = jsonValidator(output)

	_, err := executor.Run(context.Background(), request)
	var safetyErr *SafetyError
	if !errors.As(err, &safetyErr) {
		t.Fatalf("Run() error = %T %v, want *SafetyError", err, err)
	}
	assertFileContent(t, counter, "1")
	assertFileContent(t, canonical, "mutated")
}

func TestRunArtifactPreparationDriftFailsSafeWithoutRetry(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, dir := helperRequest(t, EffectArtifactOnly, "success")
	canonical := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(canonical, []byte("canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	request.CanonicalPaths = []string{canonical}
	request.MaxRetries = 3
	prepareCalls := 0
	request.PrepareAttempt = func(context.Context, int) error {
		prepareCalls++
		if err := os.WriteFile(canonical, []byte("mutated"), 0o600); err != nil {
			return err
		}
		return errors.New("partial preparation failure")
	}

	_, err := executor.Run(context.Background(), request)
	var safetyErr *SafetyError
	if !errors.As(err, &safetyErr) {
		t.Fatalf("Run() error = %T %v, want *SafetyError", err, err)
	}
	if prepareCalls != 1 {
		t.Fatalf("PrepareAttempt calls = %d, want 1", prepareCalls)
	}
	assertFileContent(t, canonical, "mutated")
}

func TestRunRejectsLogPathOverlap(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*RunRequest)
	}{
		{
			name: "output",
			configure: func(request *RunRequest) {
				request.OutputPaths = []string{request.StdoutLog}
			},
		},
		{
			name: "canonical",
			configure: func(request *RunRequest) {
				request.CanonicalPaths = []string{request.StderrLog}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := New(WithoutSignalHandling())
			defer executor.Close()

			request, _ := helperRequest(t, EffectArtifactOnly, "success")
			test.configure(&request)
			if _, err := executor.Run(context.Background(), request); err == nil {
				t.Fatal("Run() accepted an overlapping log path")
			}
		})
	}
}

func TestRunMutatingNeverRetries(t *testing.T) {
	guard := &fakeMutationGuard{snapshot: MutationSnapshot{Head: "base"}}
	executor := New(WithoutSignalHandling(), WithMutationGuard(guard))
	defer executor.Close()

	request, dir := helperRequest(t, EffectMutating, "count-and-fail")
	counter := filepath.Join(dir, "attempts")
	request.Args = append(request.Args, counter)
	request.MaxRetries = 3

	_, err := executor.Run(context.Background(), request)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run() error = %T %v, want *ExitError", err, err)
	}
	assertFileContent(t, counter, "1")
	if guard.captureCalls != 1 || guard.verifyCalls != 1 {
		t.Fatalf(
			"guard calls = capture %d, verify %d; want 1, 1",
			guard.captureCalls,
			guard.verifyCalls,
		)
	}
}

func TestRunMutatingSnapshotMismatchFailsSafe(t *testing.T) {
	guard := &fakeMutationGuard{
		snapshot:  MutationSnapshot{Head: "base"},
		verifyErr: errors.New("HEAD changed"),
	}
	executor := New(WithoutSignalHandling(), WithMutationGuard(guard))
	defer executor.Close()

	request, dir := helperRequest(t, EffectMutating, "count-and-fail")
	counter := filepath.Join(dir, "attempts")
	request.Args = append(request.Args, counter)
	request.MaxRetries = 3

	_, err := executor.Run(context.Background(), request)
	var safetyErr *SafetyError
	if !errors.As(err, &safetyErr) {
		t.Fatalf("Run() error = %T %v, want *SafetyError", err, err)
	}
	assertFileContent(t, counter, "1")
}

func TestRunMutatingTimeoutSnapshotMismatchFailsSafeWithoutRetry(t *testing.T) {
	guard := &fakeMutationGuard{
		snapshot:  MutationSnapshot{Head: "base"},
		verifyErr: errors.New("worktree became dirty"),
	}
	executor := New(WithoutSignalHandling(), WithMutationGuard(guard))
	defer executor.Close()

	request, dir := helperRequest(t, EffectMutating, "count-and-hang")
	counter := filepath.Join(dir, "attempts")
	request.Args = append(request.Args, counter)
	request.IdleTimeout = 500 * time.Millisecond
	request.MaxRetries = 3

	_, err := executor.Run(context.Background(), request)
	var safetyErr *SafetyError
	if !errors.As(err, &safetyErr) {
		t.Fatalf("Run() error = %T %v, want *SafetyError", err, err)
	}
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Run() error = %T %v, want underlying *TimeoutError", err, err)
	}
	assertFileContent(t, counter, "1")
	if guard.captureCalls != 1 || guard.verifyCalls != 1 {
		t.Fatalf(
			"guard calls = capture %d, verify %d; want 1, 1",
			guard.captureCalls,
			guard.verifyCalls,
		)
	}
}

func TestRunMutatingRejectsPreparationHook(t *testing.T) {
	guard := &fakeMutationGuard{snapshot: MutationSnapshot{Head: "base"}}
	executor := New(WithoutSignalHandling(), WithMutationGuard(guard))
	defer executor.Close()

	request, _ := helperRequest(t, EffectMutating, "nonzero")
	called := false
	request.PrepareAttempt = func(context.Context, int) error {
		called = true
		return nil
	}

	if _, err := executor.Run(context.Background(), request); err == nil {
		t.Fatal("Run() accepted a mutating preparation hook")
	}
	if called {
		t.Fatal("mutating preparation hook was called")
	}
	if guard.captureCalls != 0 {
		t.Fatalf("guard capture calls = %d, want 0", guard.captureCalls)
	}
}

func TestGitMutationGuardDetectsDirtyWorktreeAndHeadChange(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	tracked := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	runGit(
		t,
		dir,
		"-c",
		"user.name=Coterix Test",
		"-c",
		"user.email=coterix@example.invalid",
		"commit",
		"-qm",
		"initial",
	)

	guard := GitMutationGuard{}
	snapshot, err := guard.Capture(context.Background(), dir)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if snapshot.Head == "" {
		t.Fatal("Capture() returned an empty HEAD")
	}
	if err := guard.Verify(context.Background(), dir, snapshot); err != nil {
		t.Fatalf("Verify() clean snapshot error = %v", err)
	}

	if err := os.WriteFile(tracked, []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guard.Verify(context.Background(), dir, snapshot); err == nil {
		t.Fatal("Verify() accepted a dirty worktree")
	}

	if err := os.WriteFile(tracked, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "next.txt"), []byte("next"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "next.txt")
	runGit(
		t,
		dir,
		"-c",
		"user.name=Coterix Test",
		"-c",
		"user.email=coterix@example.invalid",
		"commit",
		"-qm",
		"next",
	)
	if err := guard.Verify(context.Background(), dir, snapshot); err == nil {
		t.Fatal("Verify() accepted a changed HEAD")
	}
}

func helperRequest(t *testing.T, effect Effect, scenario string, args ...string) (RunRequest, string) {
	t.Helper()
	dir := t.TempDir()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	commandArgs := []string{"-test.run=^TestRunnerHelperProcess$", "--", scenario}
	commandArgs = append(commandArgs, args...)
	return RunRequest{
		Command:     executable,
		Args:        commandArgs,
		Dir:         dir,
		Env:         map[string]string{helperEnvironment: "1"},
		IdleTimeout: 2 * time.Second,
		Effect:      effect,
		StdoutLog:   filepath.Join(dir, "logs", "stdout.log"),
		StderrLog:   filepath.Join(dir, "logs", "stderr.log"),
	}, dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func helperArguments(args []string) []string {
	for index, arg := range args {
		if arg == "--" {
			return args[index+1:]
		}
	}
	return nil
}

func helperExit(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
}

func incrementCounter(path string) int {
	attempt := 0
	if content, err := os.ReadFile(path); err == nil {
		parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
		if parseErr != nil {
			helperExit(parseErr.Error())
		}
		attempt = parsed
	} else if !errors.Is(err, os.ErrNotExist) {
		helperExit(err.Error())
	}
	attempt++
	if err := os.WriteFile(path, []byte(strconv.Itoa(attempt)), 0o600); err != nil {
		helperExit(err.Error())
	}
	return attempt
}

func jsonValidator(path string) func(context.Context, RunResult) error {
	return func(_ context.Context, _ RunResult) error {
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(content, &result); err != nil {
			return err
		}
		if !result.OK {
			return errors.New("result is not OK")
		}
		return nil
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != want {
		t.Fatalf("ReadFile(%q) = %q, want %q", path, content, want)
	}
}

func assertStreamLines(t *testing.T, lines []Line, stream Stream, want []string) {
	t.Helper()
	var got []string
	for _, line := range lines {
		if line.Stream == stream {
			got = append(got, line.Text)
		}
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("%s lines = %q, want %q", stream, got, want)
	}
}

type fakeMutationGuard struct {
	snapshot MutationSnapshot

	captureErr error
	verifyErr  error

	captureCalls int
	verifyCalls  int
}

func (guard *fakeMutationGuard) Capture(context.Context, string) (MutationSnapshot, error) {
	guard.captureCalls++
	return guard.snapshot, guard.captureErr
}

func (guard *fakeMutationGuard) Verify(context.Context, string, MutationSnapshot) error {
	guard.verifyCalls++
	return guard.verifyErr
}
