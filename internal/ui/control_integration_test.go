package ui

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const integrationPlan = `# UI control integration

## T1: Exercise the shared controller
- [ ] Prove UI actions use the pipeline control plane
Acceptance: The persisted core state crosses the expected boundary
Verify: go test ./internal/ui/...
`

var errIntegrationExecutorReleased = errors.New("integration executor released")

type blockingPlanExecutor struct {
	entered chan runner.RunRequest
	release chan struct{}
	once    sync.Once
}

func newBlockingPlanExecutor() *blockingPlanExecutor {
	return &blockingPlanExecutor{
		entered: make(chan runner.RunRequest, 1),
		release: make(chan struct{}),
	}
}

func (executor *blockingPlanExecutor) Run(
	ctx context.Context,
	request runner.RunRequest,
) (runner.RunResult, error) {
	select {
	case executor.entered <- request:
	default:
	}
	result := runner.RunResult{
		Exit:      -1,
		StdoutLog: request.StdoutLog,
		StderrLog: request.StderrLog,
	}
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case <-executor.release:
		return result, errIntegrationExecutorReleased
	}
}

func (executor *blockingPlanExecutor) stop() {
	executor.once.Do(func() {
		close(executor.release)
	})
}

func TestModelActionsDrivePersistedPipelineControllerTransitions(t *testing.T) {
	t.Run("approve reaches implementing", func(t *testing.T) {
		root, currentRun := seedAwaitingIntegrationRun(t, "approve-run")
		executor := newBlockingPlanExecutor()
		controller := pipeline.NewController(executor)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			executor.stop()
		})
		current := integrationModel(t, ctx, cancel, controller, root, currentRun.ID)

		updated, command := current.Update(printableKey('a'))
		current = updated.(model)
		if command == nil || current.operation != operationApprove {
			cancel()
			executor.stop()
			t.Fatal("approve key did not start the UI approve operation")
		}
		done := runIntegrationCommand(command)
		request := waitForIntegrationExecutor(t, executor)
		if request.Effect != runner.EffectMutating {
			stopIntegrationOperation(t, cancel, executor, done)
			t.Fatalf("approve dispatched effect=%d, want mutating", request.Effect)
		}

		persisted := openIntegrationRun(t, root, currentRun.ID)
		task := persisted.State.Tasks["T1"]
		if persisted.State.Phase != state.PhaseImplementing ||
			persisted.State.ApprovedPlanHash == nil ||
			persisted.State.CurrentTaskID == nil ||
			*persisted.State.CurrentTaskID != "T1" ||
			task == nil ||
			task.Status != state.TaskOpen ||
			task.Attempt != 1 ||
			task.BaseSHA == nil {
			stopIntegrationOperation(t, cancel, executor, done)
			t.Fatalf("persisted approve boundary = %#v", persisted.State)
		}
		stopIntegrationOperation(t, cancel, executor, done)
	})

	t.Run("reject response reaches planning", func(t *testing.T) {
		root, currentRun := seedAwaitingIntegrationRun(t, "reject-run")
		executor := newBlockingPlanExecutor()
		controller := pipeline.NewController(executor)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			executor.stop()
		})
		current := integrationModel(t, ctx, cancel, controller, root, currentRun.ID)

		updated, command := current.Update(printableKey('r'))
		current = updated.(model)
		if command != nil || current.prompt != promptReject {
			cancel()
			executor.stop()
			t.Fatal("reject key did not open the UI response prompt")
		}
		const response = "strengthen the verification evidence"
		updated, _ = current.Update(tea.PasteMsg{Content: response})
		current = updated.(model)
		updated, command = current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		if command == nil || current.operation != operationReject {
			cancel()
			executor.stop()
			t.Fatal("reject confirmation did not start the UI reject operation")
		}
		done := runIntegrationCommand(command)
		request := waitForIntegrationExecutor(t, executor)
		if !strings.Contains(integrationRequestPrompt(request), response) {
			stopIntegrationOperation(t, cancel, executor, done)
			t.Fatal("UI rejection response did not reach the core plan reviser")
		}

		persisted := openIntegrationRun(t, root, currentRun.ID)
		if persisted.State.Phase != state.PhasePlanning ||
			persisted.State.PendingAction != nil ||
			persisted.State.PlanRound != 2 {
			stopIntegrationOperation(t, cancel, executor, done)
			t.Fatalf("persisted reject boundary = %#v", persisted.State)
		}
		stopIntegrationOperation(t, cancel, executor, done)
	})

	t.Run("pending response confirm reaches planning", func(t *testing.T) {
		root, currentRun := seedPausedIntegrationRun(t, "resume-run")
		executor := newBlockingPlanExecutor()
		controller := pipeline.NewController(executor)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			executor.stop()
		})
		current := integrationModel(t, ctx, cancel, controller, root, currentRun.ID)

		updated, command := current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		if command != nil || current.prompt != promptResume {
			cancel()
			executor.stop()
			t.Fatal("pending-action enter did not open the UI response prompt")
		}
		const response = "use the pipeline package"
		updated, _ = current.Update(tea.PasteMsg{Content: response})
		current = updated.(model)
		updated, command = current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		if command == nil || current.operation != operationResume {
			cancel()
			executor.stop()
			t.Fatal("pending-action confirmation did not start UI resume")
		}
		done := runIntegrationCommand(command)
		request := waitForIntegrationExecutor(t, executor)
		if !strings.Contains(integrationRequestPrompt(request), response) {
			stopIntegrationOperation(t, cancel, executor, done)
			t.Fatal("UI pending response did not reach the core plan reviser")
		}

		persisted := openIntegrationRun(t, root, currentRun.ID)
		if persisted.State.Phase != state.PhasePlanning ||
			persisted.State.PendingAction != nil ||
			persisted.State.PlanRound != 2 {
			stopIntegrationOperation(t, cancel, executor, done)
			t.Fatalf("persisted resume boundary = %#v", persisted.State)
		}
		stopIntegrationOperation(t, cancel, executor, done)
	})
}

func integrationModel(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	controller *pipeline.Controller,
	root string,
	runID string,
) model {
	t.Helper()
	currentTheme, err := loadTheme()
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := controller.Status(ctx, root, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("Status(%q) returned %d runs", runID, len(statuses))
	}
	current := newModel(
		ctx,
		cancel,
		controller,
		root,
		"integration request",
		currentTheme,
		false,
	)
	current.hasStatus = true
	current.status = statuses[0]
	current.operation = ""
	return current
}

func runIntegrationCommand(command tea.Cmd) <-chan tea.Msg {
	done := make(chan tea.Msg, 1)
	go func() {
		runIntegrationMessage(command(), done)
	}()
	return done
}

func runIntegrationMessage(message tea.Msg, done chan<- tea.Msg) {
	switch message := message.(type) {
	case operationDoneMsg:
		done <- message
	case tea.BatchMsg:
		for _, command := range message {
			if command == nil {
				continue
			}
			child := command()
			if _, ok := child.(spinner.TickMsg); ok {
				continue
			}
			runIntegrationMessage(child, done)
			return
		}
	}
}

func waitForIntegrationExecutor(
	t *testing.T,
	executor *blockingPlanExecutor,
) runner.RunRequest {
	t.Helper()
	select {
	case request := <-executor.entered:
		return request
	case <-time.After(3 * time.Second):
		executor.stop()
		t.Fatal("UI core operation did not reach the blocking executor")
		return runner.RunRequest{}
	}
}

func stopIntegrationOperation(
	t *testing.T,
	cancel context.CancelFunc,
	executor *blockingPlanExecutor,
	done <-chan tea.Msg,
) {
	t.Helper()
	cancel()
	executor.stop()
	select {
	case message := <-done:
		if _, ok := message.(operationDoneMsg); !ok {
			t.Fatalf("UI operation returned unexpected message %T", message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked UI core operation did not stop")
	}
}

func seedAwaitingIntegrationRun(
	t *testing.T,
	runID string,
) (string, *pipeline.Run) {
	t.Helper()
	root, config := newIntegrationRepository(t)
	currentRun := createIntegrationRun(t, root, runID, config)
	currentRun.State.TaskOrder = []string{"T1"}
	currentRun.State.Tasks = map[string]*state.TaskState{
		"T1": {Status: state.TaskOpen},
	}
	if err := currentRun.State.TransitionPhase(
		state.PhaseAwaitingApproval,
	); err != nil {
		t.Fatal(err)
	}
	if err := currentRun.SaveState(); err != nil {
		t.Fatal(err)
	}
	return root, currentRun
}

func seedPausedIntegrationRun(
	t *testing.T,
	runID string,
) (string, *pipeline.Run) {
	t.Helper()
	root, config := newIntegrationRepository(t)
	currentRun := createIntegrationRun(t, root, runID, config)
	if err := currentRun.State.PauseForPlanQuestion(
		"Choose the implementation package.",
	); err != nil {
		t.Fatal(err)
	}
	if err := currentRun.SaveState(); err != nil {
		t.Fatal(err)
	}
	return root, currentRun
}

func createIntegrationRun(
	t *testing.T,
	root string,
	runID string,
	config cli.Config,
) *pipeline.Run {
	t.Helper()
	currentRun, err := pipeline.CreateRun(
		root,
		runID,
		"Exercise UI control integration.",
		config,
	)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(currentRun.Dir, "plan.md")
	if err := os.WriteFile(planPath, []byte(integrationPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(integrationPlan))
	planHash := fmt.Sprintf("%x", sum)
	currentRun.State.PlanHash = &planHash
	currentRun.State.PlanRound = 1
	if err := currentRun.SaveState(); err != nil {
		t.Fatal(err)
	}
	return currentRun
}

func newIntegrationRepository(t *testing.T) (string, cli.Config) {
	t.Helper()
	root := t.TempDir()
	integrationGit(t, root, "init", "-q")
	if err := os.WriteFile(
		filepath.Join(root, ".gitignore"),
		[]byte(".coterix/runs/\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "tracked.txt"),
		[]byte("base\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	integrationGit(t, root, "add", ".")
	integrationGit(
		t,
		root,
		"-c",
		"user.name=Coterix UI Test",
		"-c",
		"user.email=ui@example.invalid",
		"commit",
		"-qm",
		"test: seed UI integration",
	)
	return root, integrationConfig()
}

func integrationConfig() cli.Config {
	return cli.Config{
		CLIs: map[string]cli.CliConfig{
			"claude": {
				Command: "claude",
				Args:    []string{},
				Stdin:   false,
				Env:     map[string]string{},
			},
			"codex": {
				Command: "codex",
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
		MaxPlanRounds:   5,
		MaxTaskAttempts: 3,
		GateCommand:     []string{"true"},
		GateCWD:         ".",
	}
}

func openIntegrationRun(
	t *testing.T,
	root string,
	runID string,
) *pipeline.Run {
	t.Helper()
	currentRun, err := pipeline.OpenRun(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	return currentRun
}

func integrationRequestPrompt(request runner.RunRequest) string {
	if len(request.Stdin) != 0 {
		return string(request.Stdin)
	}
	if len(request.Args) == 0 {
		return ""
	}
	return request.Args[len(request.Args)-1]
}

func integrationGit(
	t *testing.T,
	root string,
	arguments ...string,
) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf(
			"git %s: %v\n%s",
			strings.Join(arguments, " "),
			err,
			output,
		)
	}
	return strings.TrimSpace(string(output))
}
