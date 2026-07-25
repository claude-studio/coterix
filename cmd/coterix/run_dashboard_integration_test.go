package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const exactRunPlan = `# Exact run dashboard plan

## T1: Drive the shared controller
- [ ] Enter implementation through the dashboard
Acceptance: Exercise the exact coterix run integration and persist implementing
Verify: go test ./cmd/coterix/...
`

const exactRunWait = 10 * time.Second

type exactRunExecutor struct {
	mutating chan runner.RunRequest
}

func newExactRunExecutor() *exactRunExecutor {
	return &exactRunExecutor{
		mutating: make(chan runner.RunRequest, 1),
	}
}

func (executor *exactRunExecutor) Run(
	ctx context.Context,
	request runner.RunRequest,
) (runner.RunResult, error) {
	result := runner.RunResult{
		Exit:      0,
		StdoutLog: request.StdoutLog,
		StderrLog: request.StderrLog,
	}
	for _, path := range []string{request.StdoutLog, request.StderrLog} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return result, err
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return result, err
		}
	}
	if request.PrepareAttempt != nil {
		if err := request.PrepareAttempt(ctx, 1); err != nil {
			return result, err
		}
	}

	switch request.Effect {
	case runner.EffectArtifactOnly:
		if len(request.OutputPaths) == 0 {
			return result, fmt.Errorf("planner output path is missing")
		}
		if err := os.WriteFile(
			request.OutputPaths[0],
			[]byte(exactRunPlan),
			0o600,
		); err != nil {
			return result, err
		}
	case runner.EffectReadOnly:
		if len(request.OutputPaths) == 0 ||
			len(request.CanonicalPaths) == 0 {
			return result, fmt.Errorf("review paths are missing")
		}
		plan, err := os.ReadFile(request.CanonicalPaths[0])
		if err != nil {
			return result, err
		}
		hash := sha256.Sum256(plan)
		verdict := fmt.Sprintf(
			`{"schema_version":1,"target_plan_hash":"%x","clean":true,"findings":[]}`,
			hash,
		)
		if err := os.WriteFile(
			request.OutputPaths[0],
			[]byte(verdict),
			0o600,
		); err != nil {
			return result, err
		}
	case runner.EffectMutating:
		select {
		case executor.mutating <- request:
		default:
		}
		<-ctx.Done()
		result.Exit = -1
		if request.OnAttemptDone != nil {
			request.OnAttemptDone(runner.AttemptDone{
				Attempt: 1,
				Result:  result,
				Err:     ctx.Err(),
			})
		}
		return result, ctx.Err()
	default:
		return result, fmt.Errorf("unexpected executor effect %d", request.Effect)
	}

	if request.OnLine != nil {
		request.OnLine(runner.Line{
			Attempt: 1,
			Stream:  runner.StreamStdout,
			Text:    "streamed through exact coterix run",
		})
	}
	if request.ValidateResult != nil {
		if err := request.ValidateResult(ctx, result); err != nil {
			return result, err
		}
	}
	if request.OnAttemptDone != nil {
		request.OnAttemptDone(runner.AttemptDone{
			Attempt: 1,
			Result:  result,
		})
	}
	return result, nil
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(content []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(content)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func TestExecuteExactRunDashboardDrivesSharedControllerAndWaitsForCleanup(
	t *testing.T,
) {
	repoRoot, nested := newExactRunRepository(t)
	executor := newExactRunExecutor()
	input, keyboard := io.Pipe()
	t.Cleanup(func() {
		_ = keyboard.Close()
		_ = input.Close()
	})
	var output lockedBuffer
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan int, 1)
	go func() {
		done <- execute(
			ctx,
			pipeline.NewController(executor),
			nested,
			[]string{"run", "exercise exact dashboard entry"},
			&output,
			&stderr,
			charmDashboard{
				executor:    executor,
				input:       input,
				output:      &output,
				interactive: true,
				width:       100,
				height:      30,
			},
		)
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case code := <-done:
		t.Fatalf(
			"dashboard exited before planning: code=%d stderr=%q output=%q",
			code,
			stderr.String(),
			ansi.Strip(output.String()),
		)
	default:
	}

	runID := waitExactRunPhase(
		t,
		repoRoot,
		state.PhaseAwaitingApproval,
	)
	renderDeadline := time.Now().Add(exactRunWait)
	for time.Now().Before(renderDeadline) &&
		!strings.Contains(
			ansi.Strip(output.String()),
			"Exercise the exact coterix run integration",
		) {
		time.Sleep(10 * time.Millisecond)
	}
	if plain := ansi.Strip(output.String()); !strings.Contains(
		plain,
		"Exercise the exact coterix run integration",
	) {
		t.Fatalf("canonical-root plan was not rendered:\n%s", plain)
	}

	approveDeadline := time.Now().Add(exactRunWait)
	approved := false
	for time.Now().Before(approveDeadline) && !approved {
		select {
		case <-executor.mutating:
			approved = true
			continue
		default:
		}
		// Approving takes two keys: `a` arms the confirmation and `\r` commits it
		// (T14 W5). Both are sent each round because the loop may start before the
		// dashboard has reached awaiting_approval.
		if _, err := keyboard.Write([]byte("a\r")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !approved {
		t.Fatal("dashboard approve key did not reach the mutating core step")
	}
	persisted, err := pipeline.OpenRun(repoRoot, runID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State.Phase != state.PhaseImplementing ||
		persisted.State.CurrentTaskID == nil ||
		*persisted.State.CurrentTaskID != "T1" {
		t.Fatalf("approved persisted state = %#v", persisted.State)
	}

	cancel()
	select {
	case code := <-done:
		if code != 1 ||
			!strings.Contains(stderr.String(), context.Canceled.Error()) {
			t.Fatalf(
				"cancelled dashboard code=%d stderr=%q",
				code,
				stderr.String(),
			)
		}
	case <-time.After(exactRunWait):
		t.Fatal("dashboard did not wait for external-cancel core cleanup")
	}
	persisted, err = pipeline.OpenRun(repoRoot, runID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State.Phase != state.PhaseFailed {
		t.Fatalf(
			"dashboard exited before core cleanup: phase=%s",
			persisted.State.Phase,
		)
	}
}

func newExactRunRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	exactRunGit(t, root, "init", "-q")
	exactRunGit(t, root, "config", "user.name", "Coterix Test")
	exactRunGit(t, root, "config", "user.email", "test@example.invalid")
	if err := os.Mkdir(filepath.Join(root, ".coterix"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := cli.Config{
		CLIs: map[string]cli.CliConfig{
			"claude": {
				Command: "fake-claude",
				Args:    []string{},
				Stdin:   false,
				Env:     map[string]string{},
			},
			"codex": {
				Command: "fake-codex",
				Args:    []string{},
				Stdin:   true,
				Env:     map[string]string{},
			},
		},
		Roles: map[cli.Role]string{
			cli.RolePlanWriter:   "claude",
			cli.RolePlanReviewer: "codex",
			cli.RolePlanReviser:  "claude",
			cli.RoleImplWriter:   "codex",
			cli.RoleImplReviewer: "claude",
			cli.RoleFixer:        "codex",
		},
		IdleTimeoutSecs: 5,
		MaxRetries:      0,
		MaxPlanRounds:   3,
		MaxTaskAttempts: 2,
		GateCommand:     []string{"true"},
		GateCWD:         ".",
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(
		filepath.Join(root, ".coterix", "config.json"),
		encoded,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".gitignore"),
		[]byte(".coterix/runs/\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "tracked.txt"),
		[]byte("baseline\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	exactRunGit(t, root, "add", ".")
	exactRunGit(t, root, "commit", "-qm", "test: seed exact run")
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, nested
}

func waitExactRunPhase(
	t *testing.T,
	repoRoot string,
	phase state.Phase,
) string {
	t.Helper()
	var runID string
	waitExactRunCondition(t, func() bool {
		entries, err := os.ReadDir(
			filepath.Join(repoRoot, ".coterix", "runs"),
		)
		if err != nil || len(entries) != 1 {
			return false
		}
		runID = entries[0].Name()
		currentRun, err := pipeline.OpenRun(repoRoot, runID)
		return err == nil && currentRun.State.Phase == phase
	}, "persisted phase "+string(phase))
	return runID
}

func waitExactRunCondition(
	t *testing.T,
	condition func() bool,
	description string,
) {
	t.Helper()
	deadline := time.Now().Add(exactRunWait)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func exactRunGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}
