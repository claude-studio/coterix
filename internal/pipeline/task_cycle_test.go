package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const (
	taskCycleHelperEnvironment = "COTERIX_TASK_CYCLE_HELPER"
	taskCycleScenarioEnv       = "COTERIX_TASK_CYCLE_SCENARIO"
	taskCycleRepoRootEnv       = "COTERIX_TASK_CYCLE_REPO_ROOT"
	taskCycleRunDirEnv         = "COTERIX_TASK_CYCLE_RUN_DIR"
	taskCycleExpectedTaskEnv   = "COTERIX_TASK_CYCLE_EXPECTED_TASK"
)

const taskCyclePlan = `# Task cycle plan

## T2: Ordered first task
- [ ] Implement the ordered first task
Acceptance: The ordered first task is committed
Verify: go test ./internal/pipeline/...

## T1: Ordered second task
- [ ] Implement the ordered second task
Acceptance: The ordered second task remains open
Verify: go test ./internal/pipeline/...
`

const taskCycleFirstBody = `## T2: Ordered first task
- [ ] Implement the ordered first task
Acceptance: The ordered first task is committed
Verify: go test ./internal/pipeline/...`

func TestTaskCycleHelperProcess(t *testing.T) {
	if os.Getenv(taskCycleHelperEnvironment) != "1" {
		return
	}
	arguments := taskCycleArgumentsAfterDoubleDash(os.Args)
	if len(arguments) != 1 || arguments[0] != "codex" {
		taskCycleHelperFail("task helper requires the codex role")
	}
	rendered, err := io.ReadAll(os.Stdin)
	if err != nil {
		taskCycleHelperFail(err.Error())
	}

	repoRoot := os.Getenv(taskCycleRepoRootEnv)
	runDir := os.Getenv(taskCycleRunDirEnv)
	scenario := os.Getenv(taskCycleScenarioEnv)
	taskID := os.Getenv(taskCycleExpectedTaskEnv)
	if repoRoot == "" || runDir == "" || scenario == "" || taskID == "" {
		taskCycleHelperFail("task helper environment is incomplete")
	}
	taskCycleIncrementHelperCount(filepath.Join(runDir, "impl.count"))

	current, err := state.Load(filepath.Join(runDir, stateFileName))
	if err != nil {
		taskCycleHelperFail(err.Error())
	}
	head := strings.TrimSpace(taskCycleHelperGit(repoRoot, "rev-parse", "--verify", "HEAD"))
	task := current.Tasks[taskID]
	if current.Phase != state.PhaseImplementing ||
		current.CurrentTaskID == nil ||
		*current.CurrentTaskID != taskID ||
		task == nil ||
		task.Status != state.TaskOpen ||
		task.Attempt != 1 ||
		task.BaseSHA == nil ||
		*task.BaseSHA != head ||
		task.CandidateSHA != nil {
		taskCycleHelperFail(fmt.Sprintf(
			"implementation snapshot was not persisted before invocation: %#v",
			current,
		))
	}

	planPath := filepath.Join(runDir, planFileName)
	planInfo, err := os.Stat(planPath)
	if err != nil {
		taskCycleHelperFail(err.Error())
	}
	if planInfo.Mode().Perm()&0o222 != 0 {
		taskCycleHelperFail("approved plan was writable during implementation")
	}
	promptText := string(rendered)
	if !strings.Contains(promptText, planPath) ||
		!strings.Contains(
			promptText,
			"Task to implement THIS iteration:\n"+taskCycleFirstBody,
		) {
		taskCycleHelperFail("implementation prompt omitted the frozen plan or ordered task body")
	}

	trackedPath := filepath.Join(repoRoot, "tracked.txt")
	switch scenario {
	case "success":
		taskCycleHelperWrite(trackedPath, "candidate\n")
		taskCycleHelperGit(repoRoot, "add", "tracked.txt")
		taskCycleHelperGit(
			repoRoot,
			"-c", "user.name=Coterix Task Test",
			"-c", "user.email=task@example.invalid",
			"commit", "-qm", "implement ordered task",
		)
	case "no-commit":
		return
	case "dirty":
		taskCycleHelperWrite(trackedPath, "dirty candidate\n")
	case "nonzero":
		_, _ = fmt.Fprintln(os.Stderr, "intentional implementation failure")
		os.Exit(23)
	case "timeout":
		time.Sleep(10 * time.Second)
	case "auth":
		_, _ = fmt.Fprintln(os.Stderr, "API key auth is missing a key")
		os.Exit(17)
	case "auth-dirty":
		taskCycleHelperWrite(trackedPath, "partial auth mutation\n")
		_, _ = fmt.Fprintln(os.Stderr, "API key auth is missing a key")
		os.Exit(17)
	case "auth-commit":
		taskCycleHelperWrite(trackedPath, "partial auth commit\n")
		taskCycleHelperGit(repoRoot, "add", "tracked.txt")
		taskCycleHelperGit(
			repoRoot,
			"-c", "user.name=Coterix Task Test",
			"-c", "user.email=task@example.invalid",
			"commit", "-qm", "partial commit before auth failure",
		)
		_, _ = fmt.Fprintln(os.Stderr, "API key auth is missing a key")
		os.Exit(17)
	default:
		taskCycleHelperFail("unknown task helper scenario: " + scenario)
	}
}

func TestTaskCycleRecordsOrderedCandidateWithoutChangingPlan(t *testing.T) {
	fixture := newTaskCycleTestRun(t, "success")
	beforePlan := taskCycleReadFile(t, filepath.Join(fixture.run.Dir, planFileName))

	if err := executeTaskCycle(t, fixture.run); err != nil {
		t.Fatalf("TaskCycle.Run() error = %v", err)
	}

	reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
	if reloaded.State.Phase != state.PhaseImplementing {
		t.Fatalf("phase = %s, want implementing", reloaded.State.Phase)
	}
	if reloaded.State.CurrentTaskID == nil ||
		*reloaded.State.CurrentTaskID != "T2" {
		t.Fatalf("current_task_id = %v, want T2", reloaded.State.CurrentTaskID)
	}
	first := reloaded.State.Tasks["T2"]
	head := taskCycleGitHead(t, fixture.run.RepoRoot)
	if first == nil ||
		first.Status != state.TaskCandidate ||
		first.Attempt != 1 ||
		first.BaseSHA == nil ||
		*first.BaseSHA != fixture.baseSHA ||
		first.CandidateSHA == nil ||
		*first.CandidateSHA != head ||
		*first.CandidateSHA == *first.BaseSHA ||
		first.GateResult != nil ||
		first.ReviewResult != nil {
		t.Fatalf("T2 state = %#v", first)
	}
	second := reloaded.State.Tasks["T1"]
	if second == nil ||
		second.Status != state.TaskOpen ||
		second.Attempt != 0 ||
		second.BaseSHA != nil ||
		second.CandidateSHA != nil {
		t.Fatalf("T1 state = %#v, want untouched open task", second)
	}
	if got := strings.TrimSpace(controlGit(t, fixture.run.RepoRoot, "status", "--porcelain")); got != "" {
		t.Fatalf("candidate worktree is dirty: %q", got)
	}
	afterPlan := taskCycleReadFile(t, filepath.Join(fixture.run.Dir, planFileName))
	if !bytes.Equal(afterPlan, beforePlan) {
		t.Fatal("task cycle changed frozen plan.md")
	}
	if bytes.Contains(afterPlan, []byte("- [x]")) ||
		bytes.Count(afterPlan, []byte("- [ ]")) != 2 {
		t.Fatalf("task cycle ticked a plan checkbox: %q", afterPlan)
	}
	taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
}

func TestTaskCyclePreconditionAndApprovedPlanFailSafe(t *testing.T) {
	t.Run("dirty worktree", func(t *testing.T) {
		fixture := newTaskCycleTestRun(t, "success")
		trackedPath := filepath.Join(fixture.run.RepoRoot, "tracked.txt")
		if err := os.WriteFile(trackedPath, []byte("precondition dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := executeTaskCycle(t, fixture.run); err == nil {
			t.Fatal("TaskCycle.Run() accepted a dirty precondition")
		}
		reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		taskCycleAssertFailedOpenTask(t, reloaded)
		if reloaded.State.Tasks["T2"].BaseSHA != nil {
			t.Fatalf("dirty precondition recorded base_sha = %v", reloaded.State.Tasks["T2"].BaseSHA)
		}
		if got := string(taskCycleReadFile(t, trackedPath)); got != "precondition dirty\n" {
			t.Fatalf("fail-safe reconciled dirty content to %q", got)
		}
		taskCycleAssertHelperCount(t, fixture.run.Dir, 0)
	})

	t.Run("approved plan hash mismatch", func(t *testing.T) {
		fixture := newTaskCycleTestRun(t, "success")
		planPath := filepath.Join(fixture.run.Dir, planFileName)
		if err := os.Chmod(planPath, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			planPath,
			[]byte(taskCyclePlan+"\nchanged after approval\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(planPath, 0o400); err != nil {
			t.Fatal(err)
		}

		if err := executeTaskCycle(t, fixture.run); err == nil {
			t.Fatal("TaskCycle.Run() accepted changed approved plan bytes")
		}
		reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		taskCycleAssertFailedOpenTask(t, reloaded)
		if taskCycleGitHead(t, fixture.run.RepoRoot) != fixture.baseSHA {
			t.Fatal("approved-plan fail-safe changed HEAD")
		}
		taskCycleAssertHelperCount(t, fixture.run.Dir, 0)
	})
}

func TestTaskCyclePostconditionFailSafe(t *testing.T) {
	tests := []struct {
		name        string
		scenario    string
		wantClean   bool
		wantTracked string
	}{
		{
			name:        "no commit leaves candidate equal to base",
			scenario:    "no-commit",
			wantClean:   true,
			wantTracked: "base\n",
		},
		{
			name:        "dirty worktree",
			scenario:    "dirty",
			wantClean:   false,
			wantTracked: "dirty candidate\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTaskCycleTestRun(t, test.scenario)
			if err := executeTaskCycle(t, fixture.run); err == nil {
				t.Fatal("TaskCycle.Run() accepted an invalid candidate")
			}

			reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
			taskCycleAssertFailedOpenTask(t, reloaded)
			task := reloaded.State.Tasks["T2"]
			if task.Attempt != 1 ||
				task.BaseSHA == nil ||
				*task.BaseSHA != fixture.baseSHA ||
				task.CandidateSHA != nil {
				t.Fatalf("failed task evidence = %#v", task)
			}
			if head := taskCycleGitHead(t, fixture.run.RepoRoot); head != fixture.baseSHA {
				t.Fatalf("HEAD = %s, want unchanged base %s", head, fixture.baseSHA)
			}
			status := strings.TrimSpace(
				controlGit(t, fixture.run.RepoRoot, "status", "--porcelain"),
			)
			if test.wantClean && status != "" {
				t.Fatalf("worktree status = %q, want clean", status)
			}
			if !test.wantClean && status == "" {
				t.Fatal("fail-safe cleaned the dirty worktree")
			}
			tracked := taskCycleReadFile(
				t,
				filepath.Join(fixture.run.RepoRoot, "tracked.txt"),
			)
			if string(tracked) != test.wantTracked {
				t.Fatalf("tracked.txt = %q, want %q", tracked, test.wantTracked)
			}
			taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
		})
	}
}

func TestTaskCycleMutatingFailuresNeverRetry(t *testing.T) {
	tests := []struct {
		name       string
		scenario   string
		assertType func(*testing.T, error)
	}{
		{
			name:     "nonzero exit",
			scenario: "nonzero",
			assertType: func(t *testing.T, err error) {
				t.Helper()
				var exitErr *runner.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("error = %T %v, want underlying *runner.ExitError", err, err)
				}
			},
		},
		{
			name:     "idle timeout",
			scenario: "timeout",
			assertType: func(t *testing.T, err error) {
				t.Helper()
				var timeoutErr *runner.TimeoutError
				if !errors.As(err, &timeoutErr) {
					t.Fatalf("error = %T %v, want underlying *runner.TimeoutError", err, err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTaskCycleTestRun(t, test.scenario)
			err := executeTaskCycle(t, fixture.run)
			if err == nil {
				t.Fatal("TaskCycle.Run() unexpectedly succeeded")
			}
			test.assertType(t, err)

			reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
			taskCycleAssertFailedOpenTask(t, reloaded)
			if taskCycleGitHead(t, fixture.run.RepoRoot) != fixture.baseSHA {
				t.Fatal("mutating failure changed HEAD")
			}
			if got := strings.TrimSpace(controlGit(t, fixture.run.RepoRoot, "status", "--porcelain")); got != "" {
				t.Fatalf("mutating failure left worktree dirty: %q", got)
			}
			taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
		})
	}
}

func TestTaskCycleAuthPauseRequiresUnchangedMutationSnapshot(t *testing.T) {
	t.Run("clean typed auth failure pauses response-free", func(t *testing.T) {
		fixture := newTaskCycleTestRun(t, "auth")
		if err := executeTaskCycle(t, fixture.run); err != nil {
			t.Fatalf("TaskCycle.Run() auth error = %v", err)
		}

		reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		action := reloaded.State.PendingAction
		task := reloaded.State.Tasks["T2"]
		if reloaded.State.Phase != state.PhasePausedForInput ||
			action == nil ||
			action.Kind != state.PendingAuth ||
			action.ResumePhase != state.PhaseImplementing ||
			action.TaskID == nil ||
			*action.TaskID != "T2" ||
			action.Response != nil ||
			task.Status != state.TaskOpen ||
			task.Attempt != 1 ||
			task.BaseSHA == nil ||
			*task.BaseSHA != fixture.baseSHA ||
			task.CandidateSHA != nil ||
			reloaded.State.LastError != nil {
			t.Fatalf("auth pause state = %#v", reloaded.State)
		}
		if taskCycleGitHead(t, fixture.run.RepoRoot) != fixture.baseSHA {
			t.Fatal("clean auth failure changed HEAD")
		}
		if got := strings.TrimSpace(controlGit(t, fixture.run.RepoRoot, "status", "--porcelain")); got != "" {
			t.Fatalf("clean auth failure left worktree dirty: %q", got)
		}
		taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
	})

	t.Run("partial mutation cannot become auth pause", func(t *testing.T) {
		fixture := newTaskCycleTestRun(t, "auth-dirty")
		err := executeTaskCycle(t, fixture.run)
		if err == nil {
			t.Fatal("TaskCycle.Run() paused after a partial auth mutation")
		}
		var safetyErr *runner.SafetyError
		if !errors.As(err, &safetyErr) {
			t.Fatalf("error = %T %v, want underlying *runner.SafetyError", err, err)
		}

		reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		taskCycleAssertFailedOpenTask(t, reloaded)
		if reloaded.State.PendingAction != nil {
			t.Fatalf("partial mutation created pending auth action: %#v", reloaded.State.PendingAction)
		}
		if got := strings.TrimSpace(controlGit(t, fixture.run.RepoRoot, "status", "--porcelain")); got == "" {
			t.Fatal("fail-safe cleaned the partial auth mutation")
		}
		if got := string(taskCycleReadFile(
			t,
			filepath.Join(fixture.run.RepoRoot, "tracked.txt"),
		)); got != "partial auth mutation\n" {
			t.Fatalf("partial auth mutation = %q", got)
		}
		taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
	})

	t.Run("partial clean commit cannot become auth pause", func(t *testing.T) {
		fixture := newTaskCycleTestRun(t, "auth-commit")
		err := executeTaskCycle(t, fixture.run)
		if err == nil {
			t.Fatal("TaskCycle.Run() paused after a partial auth commit")
		}
		var safetyErr *runner.SafetyError
		if !errors.As(err, &safetyErr) {
			t.Fatalf("error = %T %v, want underlying *runner.SafetyError", err, err)
		}

		reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		taskCycleAssertFailedOpenTask(t, reloaded)
		if reloaded.State.PendingAction != nil {
			t.Fatalf("partial commit created pending auth action: %#v", reloaded.State.PendingAction)
		}
		if head := taskCycleGitHead(t, fixture.run.RepoRoot); head == fixture.baseSHA {
			t.Fatal("partial auth commit did not change HEAD")
		}
		if got := strings.TrimSpace(
			controlGit(t, fixture.run.RepoRoot, "status", "--porcelain"),
		); got != "" {
			t.Fatalf("partial auth commit worktree = %q, want clean", got)
		}
		taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
	})
}

type taskCycleFixture struct {
	run     *Run
	baseSHA string
}

func newTaskCycleTestRun(t *testing.T, scenario string) taskCycleFixture {
	t.Helper()
	repoRoot := t.TempDir()
	runID := "task-cycle-run"
	runDir := filepath.Join(repoRoot, ".coterix", "runs", runID)

	config := testConfig(t)
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	config.CLIs["codex"] = cli.CliConfig{
		Command: command,
		Args: []string{
			"-test.run=^TestTaskCycleHelperProcess$",
			"--",
			"codex",
		},
		Stdin: true,
		Env: map[string]string{
			taskCycleHelperEnvironment: "1",
			taskCycleScenarioEnv:       scenario,
			taskCycleRepoRootEnv:       repoRoot,
			taskCycleRunDirEnv:         runDir,
			taskCycleExpectedTaskEnv:   "T2",
		},
	}
	config.IdleTimeoutSecs = 3
	config.MaxRetries = 3

	controlGit(t, repoRoot, "init", "-q")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".coterix"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repoRoot, ".gitignore"),
		[]byte(".coterix/runs/\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repoRoot, "tracked.txt"),
		[]byte("base\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	controlWriteConfig(t, repoRoot, config)
	controlGit(t, repoRoot, "add", ".")
	controlCommit(t, repoRoot, "task cycle baseline")
	baseSHA := taskCycleGitHead(t, repoRoot)

	currentRun, err := CreateRun(
		repoRoot,
		runID,
		"Implement the approved tasks in order.",
		config,
	)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(currentRun.Dir, planFileName)
	if err := os.WriteFile(planPath, []byte(taskCyclePlan), 0o600); err != nil {
		t.Fatal(err)
	}
	planHash := hashBytes([]byte(taskCyclePlan))
	currentRun.State.PlanHash = stringPointer(planHash)
	currentRun.State.TaskOrder = []string{"T2", "T1"}
	currentRun.State.Tasks = map[string]*state.TaskState{
		"T2": {Status: state.TaskOpen},
		"T1": {Status: state.TaskOpen},
	}
	if err := currentRun.State.TransitionPhase(state.PhaseAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	currentRun.State.ApprovedPlanHash = stringPointer(planHash)
	if _, err := freezePlan(planPath); err != nil {
		t.Fatal(err)
	}
	if err := currentRun.State.TransitionPhase(state.PhaseImplementing); err != nil {
		t.Fatal(err)
	}
	if err := currentRun.SaveState(); err != nil {
		t.Fatal(err)
	}
	return taskCycleFixture{run: currentRun, baseSHA: baseSHA}
}

func executeTaskCycle(t *testing.T, currentRun *Run) error {
	t.Helper()
	executor := runner.New(runner.WithoutSignalHandling())
	defer executor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return NewTaskCycle(executor).Run(ctx, currentRun)
}

func taskCycleAssertFailedOpenTask(t *testing.T, currentRun *Run) {
	t.Helper()
	task := currentRun.State.Tasks["T2"]
	if currentRun.State.Phase != state.PhaseFailed ||
		currentRun.State.LastError == nil ||
		task == nil ||
		task.Status != state.TaskOpen ||
		task.CandidateSHA != nil {
		t.Fatalf("fail-safe state = %#v", currentRun.State)
	}
}

func taskCycleAssertHelperCount(t *testing.T, runDir string, want int) {
	t.Helper()
	path := filepath.Join(runDir, "impl.count")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if want == 0 {
			return
		}
		t.Fatalf("helper count = 0, want %d", want)
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := strconv.Atoi(string(content))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("helper count = %d, want %d", got, want)
	}
}

func taskCycleReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func taskCycleGitHead(t *testing.T, repoRoot string) string {
	t.Helper()
	return strings.TrimSpace(controlGit(t, repoRoot, "rev-parse", "--verify", "HEAD"))
}

func taskCycleArgumentsAfterDoubleDash(arguments []string) []string {
	for index, argument := range arguments {
		if argument == "--" {
			return arguments[index+1:]
		}
	}
	return nil
}

func taskCycleIncrementHelperCount(path string) {
	value := 0
	content, err := os.ReadFile(path)
	if err == nil {
		value, err = strconv.Atoi(string(content))
	}
	if err != nil && !os.IsNotExist(err) {
		taskCycleHelperFail(err.Error())
	}
	value++
	taskCycleHelperWrite(path, strconv.Itoa(value))
}

func taskCycleHelperGit(repoRoot string, arguments ...string) string {
	command := exec.Command("git", arguments...)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	if err != nil {
		taskCycleHelperFail(fmt.Sprintf(
			"git %s: %v\n%s",
			strings.Join(arguments, " "),
			err,
			output,
		))
	}
	return string(output)
}

func taskCycleHelperWrite(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		taskCycleHelperFail(err.Error())
	}
}

func taskCycleHelperFail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(90)
}
