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
`

const taskCycleFirstBody = `## T2: Ordered first task
- [ ] Implement the ordered first task
Acceptance: The ordered first task is committed
Verify: go test ./internal/pipeline/...`

func TestTaskCycleHelperProcess(t *testing.T) {
	arguments := taskCycleArgumentsAfterDoubleDash(os.Args)
	if len(arguments) == 0 {
		return
	}
	role := arguments[0]
	if role == "gate" {
		if len(arguments) != 5 {
			taskCycleHelperFail("gate helper arguments are incomplete")
		}
		scenario := arguments[1]
		repoRoot := arguments[2]
		runDir := arguments[3]
		count := taskCycleIncrementHelperCount(filepath.Join(runDir, "gate.count"))
		switch scenario {
		case "gate-fail-repair", "gate-fail-cap":
			if count == 1 {
				_, _ = fmt.Fprintln(os.Stdout, "gate stdout failure")
				_, _ = fmt.Fprintln(os.Stderr, "gate stderr failure")
				os.Exit(23)
			}
		case "gate-timeout":
			time.Sleep(10 * time.Second)
		case "gate-head-drift":
			taskCycleHelperWrite(
				filepath.Join(repoRoot, "tracked.txt"),
				"gate changed HEAD\n",
			)
			taskCycleHelperGit(repoRoot, "add", "tracked.txt")
			taskCycleHelperGit(
				repoRoot,
				"-c", "user.name=Coterix Task Test",
				"-c", "user.email=task@example.invalid",
				"commit", "-qm", "gate changed HEAD",
			)
		case "gate-dirty":
			taskCycleHelperWrite(
				filepath.Join(repoRoot, "tracked.txt"),
				"gate dirtied worktree\n",
			)
		}
		_, _ = fmt.Fprintln(os.Stdout, "trusted gate passed")
		return
	}
	if os.Getenv(taskCycleHelperEnvironment) != "1" {
		return
	}
	if len(arguments) != 1 || (role != "codex" && role != "claude") {
		taskCycleHelperFail("task helper requires the codex or claude role")
	}
	rendered, err := io.ReadAll(os.Stdin)
	if err != nil {
		taskCycleHelperFail(err.Error())
	}

	repoRoot := os.Getenv(taskCycleRepoRootEnv)
	runDir := os.Getenv(taskCycleRunDirEnv)
	scenario := os.Getenv(taskCycleScenarioEnv)
	if repoRoot == "" || runDir == "" || scenario == "" {
		taskCycleHelperFail("task helper environment is incomplete")
	}
	current, err := state.Load(filepath.Join(runDir, stateFileName))
	if err != nil {
		taskCycleHelperFail(err.Error())
	}
	if current.CurrentTaskID == nil {
		taskCycleHelperFail("task helper state has no current task")
	}
	taskID := *current.CurrentTaskID
	task := current.Tasks[taskID]
	head := strings.TrimSpace(taskCycleHelperGit(repoRoot, "rev-parse", "--verify", "HEAD"))
	promptText := string(rendered)

	if role == "claude" {
		count := taskCycleIncrementHelperCount(filepath.Join(runDir, "review.count"))
		if current.ApprovedPlanHash == nil || task == nil ||
			task.Status != state.TaskCandidate ||
			task.CandidateSHA == nil ||
			*task.CandidateSHA != head {
			taskCycleHelperFail("implementation review state is incomplete")
		}
		if scenario == "review-auth" {
			_, _ = fmt.Fprintln(os.Stderr, "Not logged in")
			os.Exit(17)
		}
		if scenario == "review-head-drift" && count == 1 {
			taskCycleHelperWrite(
				filepath.Join(repoRoot, "tracked.txt"),
				"review changed HEAD\n",
			)
			taskCycleHelperGit(repoRoot, "add", "tracked.txt")
			taskCycleHelperGit(
				repoRoot,
				"-c", "user.name=Coterix Task Test",
				"-c", "user.email=task@example.invalid",
				"commit", "-qm", "review changed HEAD",
			)
		}
		reviewPath := filepath.Join(
			runDir,
			tasksDirectoryName,
			taskID,
			reviewEvidenceName,
		)
		if scenario == "review-missing" {
			return
		}
		if scenario == "review-malformed" {
			taskCycleHelperWrite(reviewPath, `{"schema_version":`)
			return
		}
		planHash := *current.ApprovedPlanHash
		reviewTaskID := taskID
		candidateSHA := *task.CandidateSHA
		clean := true
		findings := "[]"
		switch scenario {
		case "review-target-mismatch":
			candidateSHA = strings.Repeat("0", 40)
		case "review-task-mismatch":
			reviewTaskID = "T999"
		case "review-plan-mismatch":
			planHash = strings.Repeat("f", 64)
		case "review-inconsistent":
			findings = `[{"id":"f1","severity":"major","location":"tracked.txt:1","issue":"blocking issue","requested_change":"fix it"}]`
		case "review-dirty-repair",
			"review-dirty-cap",
			"fixer-auth-once",
			"fixer-no-commit",
			"fixer-dirty":
			if count == 1 {
				clean = false
				findings = `[{"id":"f1","severity":"major","location":"tracked.txt:1","issue":"candidate is incomplete","requested_change":"complete it"}]`
			}
		case "review-minor":
			findings = `[{"id":"f1","severity":"minor","location":null,"issue":"non-blocking note","requested_change":"consider later"}]`
		}
		review := fmt.Sprintf(
			`{"schema_version":1,"plan_hash":%q,"task_id":%q,"candidate_sha":%q,"clean":%t,"findings":%s}`,
			planHash,
			reviewTaskID,
			candidateSHA,
			clean,
			findings,
		)
		taskCycleHelperWrite(reviewPath, review)
		return
	}

	planPath := filepath.Join(runDir, planFileName)
	planInfo, err := os.Stat(planPath)
	if err != nil {
		taskCycleHelperFail(err.Error())
	}
	if planInfo.Mode().Perm()&0o222 != 0 {
		taskCycleHelperFail("approved plan was writable during mutation")
	}
	trackedPath := filepath.Join(repoRoot, "tracked.txt")
	if strings.Contains(promptText, "You are a FIXER.") {
		count := taskCycleIncrementHelperCount(filepath.Join(runDir, "fix.count"))
		if current.Phase != state.PhaseImplementing ||
			task == nil ||
			task.Status != state.TaskRepairing ||
			task.Attempt < 2 ||
			task.CandidateSHA == nil ||
			*task.CandidateSHA != head {
			taskCycleHelperFail(fmt.Sprintf(
				"fixer snapshot was not persisted before invocation: %#v",
				current,
			))
		}
		if scenario == "fixer-auth-once" && count == 1 {
			_, _ = fmt.Fprintln(os.Stderr, "API key auth is missing a key")
			os.Exit(17)
		}
		if scenario == "fixer-no-commit" {
			return
		}
		if scenario == "fixer-dirty" {
			taskCycleHelperWrite(trackedPath, "dirty fixer output\n")
			return
		}
		if scenario == "gate-fail-repair" &&
			(!strings.Contains(promptText, "exit: 23") ||
				!strings.Contains(promptText, "gate stderr failure")) {
			taskCycleHelperFail("fixer prompt omitted trusted gate failure")
		}
		if (scenario == "review-dirty-repair" ||
			scenario == "review-dirty-cap" ||
			scenario == "fixer-auth-once" ||
			scenario == "fixer-no-commit" ||
			scenario == "fixer-dirty") &&
			!strings.Contains(promptText, "candidate is incomplete") {
			taskCycleHelperFail("fixer prompt omitted review finding")
		}
		taskCycleHelperWrite(
			trackedPath,
			fmt.Sprintf("fixed %s attempt %d\n", taskID, task.Attempt),
		)
		taskCycleHelperGit(repoRoot, "add", "tracked.txt")
		taskCycleHelperGit(
			repoRoot,
			"-c", "user.name=Coterix Task Test",
			"-c", "user.email=task@example.invalid",
			"commit", "-qm", "repair task candidate",
		)
		return
	}

	taskCycleIncrementHelperCount(filepath.Join(runDir, "impl.count"))
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

	if !strings.Contains(promptText, planPath) ||
		!strings.Contains(promptText, "## "+taskID+":") {
		taskCycleHelperFail("implementation prompt omitted the frozen plan or ordered task body")
	}

	switch scenario {
	case "success",
		"review-minor",
		"gate-fail-repair",
		"gate-fail-cap",
		"gate-timeout",
		"gate-head-drift",
		"gate-dirty",
		"review-dirty-repair",
		"review-dirty-cap",
		"review-target-mismatch",
		"review-task-mismatch",
		"review-plan-mismatch",
		"review-inconsistent",
		"review-malformed",
		"review-missing",
		"review-auth",
		"review-head-drift",
		"fixer-auth-once",
		"fixer-no-commit",
		"fixer-dirty":
		taskCycleHelperWrite(
			trackedPath,
			fmt.Sprintf("candidate %s\n", taskID),
		)
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

func TestTaskCycleConfirmsTaskWithoutChangingPlan(t *testing.T) {
	fixture := newTaskCycleTestRun(t, "success")
	beforePlan := taskCycleReadFile(t, filepath.Join(fixture.run.Dir, planFileName))

	if err := executeTaskCycle(t, fixture.run); err != nil {
		t.Fatalf("TaskCycle.Run() error = %v", err)
	}

	reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
	if reloaded.State.Phase != state.PhaseDone {
		t.Fatalf("phase = %s, want done", reloaded.State.Phase)
	}
	if reloaded.State.CurrentTaskID == nil ||
		*reloaded.State.CurrentTaskID != "T2" {
		t.Fatalf("current_task_id = %v, want T2", reloaded.State.CurrentTaskID)
	}
	first := reloaded.State.Tasks["T2"]
	head := taskCycleGitHead(t, fixture.run.RepoRoot)
	if first == nil ||
		first.Status != state.TaskConfirmed ||
		first.Attempt != 1 ||
		first.BaseSHA == nil ||
		*first.BaseSHA != fixture.baseSHA ||
		first.CandidateSHA == nil ||
		*first.CandidateSHA != head ||
		*first.CandidateSHA == *first.BaseSHA ||
		first.GateResult == nil ||
		*first.GateResult != filepath.Join("tasks", "T2", "gate.json") ||
		first.ReviewResult == nil ||
		*first.ReviewResult != filepath.Join("tasks", "T2", "review.json") {
		t.Fatalf("T2 state = %#v", first)
	}
	if got := strings.TrimSpace(controlGit(t, fixture.run.RepoRoot, "status", "--porcelain")); got != "" {
		t.Fatalf("candidate worktree is dirty: %q", got)
	}
	afterPlan := taskCycleReadFile(t, filepath.Join(fixture.run.Dir, planFileName))
	if !bytes.Equal(afterPlan, beforePlan) {
		t.Fatal("task cycle changed frozen plan.md")
	}
	if bytes.Contains(afterPlan, []byte("- [x]")) ||
		bytes.Count(afterPlan, []byte("- [ ]")) != 1 {
		t.Fatalf("task cycle ticked a plan checkbox: %q", afterPlan)
	}
	taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
	taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 1)
	taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "review.count"), 1)
}

func TestTaskCycleRepairsFailedGateAndDirtyReview(t *testing.T) {
	tests := []struct {
		scenario    string
		wantReviews int
	}{
		{scenario: "gate-fail-repair", wantReviews: 1},
		{scenario: "review-dirty-repair", wantReviews: 2},
	}

	for _, test := range tests {
		t.Run(test.scenario, func(t *testing.T) {
			fixture := newTaskCycleTestRun(t, test.scenario)
			if err := executeTaskCycle(t, fixture.run); err != nil {
				t.Fatalf("TaskCycle.Run() error = %v", err)
			}

			reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
			task := reloaded.State.Tasks["T2"]
			finalCandidate := taskCycleGitHead(t, fixture.run.RepoRoot)
			originalCandidate := strings.TrimSpace(
				controlGit(t, fixture.run.RepoRoot, "rev-parse", "HEAD^"),
			)
			if reloaded.State.Phase != state.PhaseDone ||
				task == nil ||
				task.Status != state.TaskConfirmed ||
				task.Attempt != 2 ||
				task.BaseSHA == nil ||
				*task.BaseSHA != fixture.baseSHA ||
				task.CandidateSHA == nil ||
				*task.CandidateSHA != finalCandidate ||
				originalCandidate == finalCandidate ||
				originalCandidate == fixture.baseSHA {
				t.Fatalf("repaired task state = %#v", reloaded.State)
			}
			taskCycleAssertFinalEvidence(
				t,
				reloaded,
				"T2",
				finalCandidate,
				originalCandidate,
			)
			taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "fix.count"), 1)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 2)
			taskCycleAssertCount(
				t,
				filepath.Join(fixture.run.Dir, "review.count"),
				test.wantReviews,
			)
		})
	}
}

func TestTaskCycleInvalidImplementationReviewFailsWithoutRepair(t *testing.T) {
	tests := []struct {
		scenario        string
		wantReviews     int
		wantHeadMatches bool
	}{
		{scenario: "review-missing", wantReviews: 4, wantHeadMatches: true},
		{scenario: "review-malformed", wantReviews: 4, wantHeadMatches: true},
		{scenario: "review-inconsistent", wantReviews: 4, wantHeadMatches: true},
		{scenario: "review-target-mismatch", wantReviews: 4, wantHeadMatches: true},
		{scenario: "review-task-mismatch", wantReviews: 4, wantHeadMatches: true},
		{scenario: "review-plan-mismatch", wantReviews: 4, wantHeadMatches: true},
		{scenario: "review-head-drift", wantReviews: 1},
	}

	for _, test := range tests {
		t.Run(test.scenario, func(t *testing.T) {
			fixture := newTaskCycleTestRun(t, test.scenario)
			if err := executeTaskCycle(t, fixture.run); err == nil {
				t.Fatal("TaskCycle.Run() accepted an invalid implementation review")
			}

			reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
			task := reloaded.State.Tasks["T2"]
			if reloaded.State.Phase != state.PhaseFailed ||
				reloaded.State.LastError == nil ||
				reloaded.State.PendingAction != nil ||
				task == nil ||
				task.Status != state.TaskCandidate ||
				task.Attempt != 1 ||
				task.CandidateSHA == nil ||
				task.GateResult == nil ||
				*task.GateResult != filepath.Join("tasks", "T2", "gate.json") ||
				task.ReviewResult != nil {
				t.Fatalf("invalid-review fail-safe state = %#v", reloaded.State)
			}
			head := taskCycleGitHead(t, fixture.run.RepoRoot)
			if got := head == *task.CandidateSHA; got != test.wantHeadMatches {
				t.Fatalf(
					"HEAD matches candidate_sha = %t, want %t (HEAD=%s candidate=%s)",
					got,
					test.wantHeadMatches,
					head,
					*task.CandidateSHA,
				)
			}
			if got := strings.TrimSpace(
				controlGit(t, fixture.run.RepoRoot, "status", "--porcelain"),
			); got != "" {
				t.Fatalf("invalid-review fail-safe worktree is dirty: %q", got)
			}
			taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 1)
			taskCycleAssertCount(
				t,
				filepath.Join(fixture.run.Dir, "review.count"),
				test.wantReviews,
			)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "fix.count"), 0)
		})
	}
}

func TestTaskCycleRepairCapPausesAndRetryUsesFixer(t *testing.T) {
	tests := []struct {
		scenario          string
		wantPausedReviews int
		wantFinalReviews  int
	}{
		{
			scenario:          "gate-fail-cap",
			wantPausedReviews: 0,
			wantFinalReviews:  1,
		},
		{
			scenario:          "review-dirty-cap",
			wantPausedReviews: 1,
			wantFinalReviews:  2,
		},
	}

	for _, test := range tests {
		t.Run(test.scenario, func(t *testing.T) {
			fixture := newTaskCycleTestRunWithMaxTaskAttempts(
				t,
				test.scenario,
				1,
			)
			if err := executeTaskCycle(t, fixture.run); err != nil {
				t.Fatalf("TaskCycle.Run() cap error = %v", err)
			}

			paused := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
			task := paused.State.Tasks["T2"]
			action := paused.State.PendingAction
			if paused.State.Phase != state.PhasePausedForInput ||
				paused.State.LastError != nil ||
				action == nil ||
				action.Kind != state.PendingTaskCap ||
				action.ResumePhase != state.PhaseImplementing ||
				action.TaskID == nil ||
				*action.TaskID != "T2" ||
				action.Response != nil ||
				task == nil ||
				task.Status != state.TaskRepairing ||
				task.Attempt != 1 ||
				task.CandidateSHA == nil ||
				task.GateResult == nil {
				t.Fatalf("repair cap state = %#v", paused.State)
			}
			if test.wantPausedReviews == 0 && task.ReviewResult != nil {
				t.Fatalf("failed gate linked review evidence: %#v", task)
			}
			if test.wantPausedReviews > 0 && task.ReviewResult == nil {
				t.Fatalf("dirty review was not linked before cap: %#v", task)
			}
			pausedCandidate := *task.CandidateSHA
			taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "fix.count"), 0)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 1)
			taskCycleAssertCount(
				t,
				filepath.Join(fixture.run.Dir, "review.count"),
				test.wantPausedReviews,
			)

			executor := runner.New(runner.WithoutSignalHandling())
			defer executor.Close()
			retry := "retry"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			status, err := NewController(executor).Resume(
				ctx,
				fixture.run.RepoRoot,
				fixture.run.ID,
				&retry,
			)
			if err != nil {
				t.Fatalf("Resume(retry) error = %v", err)
			}
			if status.Phase != state.PhaseDone ||
				status.PendingAction != nil ||
				status.Tasks["T2"].Status != state.TaskConfirmed ||
				status.Tasks["T2"].Attempt != 2 {
				t.Fatalf("repair cap retry status = %#v", status)
			}

			reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
			finalCandidate := taskCycleGitHead(t, fixture.run.RepoRoot)
			if finalCandidate == pausedCandidate {
				t.Fatal("repair cap retry did not create a new candidate")
			}
			taskCycleAssertFinalEvidence(
				t,
				reloaded,
				"T2",
				finalCandidate,
				pausedCandidate,
			)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "fix.count"), 1)
			taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 2)
			taskCycleAssertCount(
				t,
				filepath.Join(fixture.run.Dir, "review.count"),
				test.wantFinalReviews,
			)
		})
	}
}

func TestTaskCycleReviewerAndFixerAuthPause(t *testing.T) {
	t.Run("reviewer auth", func(t *testing.T) {
		fixture := newTaskCycleTestRun(t, "review-auth")
		if err := executeTaskCycle(t, fixture.run); err != nil {
			t.Fatalf("TaskCycle.Run() reviewer auth error = %v", err)
		}

		reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		task := reloaded.State.Tasks["T2"]
		taskCycleAssertImplementingAuthPause(t, reloaded)
		if task.Status != state.TaskCandidate ||
			task.Attempt != 1 ||
			task.CandidateSHA == nil ||
			task.GateResult == nil ||
			task.ReviewResult != nil {
			t.Fatalf("reviewer auth task state = %#v", task)
		}
		taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 1)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "review.count"), 4)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "fix.count"), 0)
	})

	t.Run("fixer auth resumes through writer route", func(t *testing.T) {
		fixture := newTaskCycleTestRun(t, "fixer-auth-once")
		if err := executeTaskCycle(t, fixture.run); err != nil {
			t.Fatalf("TaskCycle.Run() fixer auth error = %v", err)
		}

		paused := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		task := paused.State.Tasks["T2"]
		taskCycleAssertImplementingAuthPause(t, paused)
		if task.Status != state.TaskRepairing ||
			task.Attempt != 2 ||
			task.CandidateSHA == nil ||
			task.GateResult == nil ||
			task.ReviewResult == nil {
			t.Fatalf("fixer auth task state = %#v", task)
		}
		pausedCandidate := *task.CandidateSHA
		taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 1)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "review.count"), 1)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "fix.count"), 1)

		executor := runner.New(runner.WithoutSignalHandling())
		defer executor.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		status, err := NewController(executor).Resume(
			ctx,
			fixture.run.RepoRoot,
			fixture.run.ID,
			nil,
		)
		if err != nil {
			t.Fatalf("response-free fixer auth Resume() error = %v", err)
		}
		if status.Phase != state.PhaseDone ||
			status.PendingAction != nil ||
			status.Tasks["T2"].Status != state.TaskConfirmed ||
			status.Tasks["T2"].Attempt != 3 {
			t.Fatalf("fixer auth resume status = %#v", status)
		}
		reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
		finalCandidate := taskCycleGitHead(t, fixture.run.RepoRoot)
		taskCycleAssertFinalEvidence(
			t,
			reloaded,
			"T2",
			finalCandidate,
			pausedCandidate,
		)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "gate.count"), 2)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "review.count"), 2)
		taskCycleAssertCount(t, filepath.Join(fixture.run.Dir, "fix.count"), 2)
	})
}

func TestTaskCycleRepairAndGateBoundariesFailSafe(t *testing.T) {
	tests := []struct {
		scenario          string
		wantStatus        state.TaskStatus
		wantAttempt       int
		wantClean         bool
		wantHeadMatches   bool
		wantGateLink      bool
		wantReviewLink    bool
		wantGateCalls     int
		wantReviewCalls   int
		wantFixCalls      int
		wantTrackedOutput string
	}{
		{
			scenario:          "fixer-no-commit",
			wantStatus:        state.TaskRepairing,
			wantAttempt:       2,
			wantClean:         true,
			wantHeadMatches:   true,
			wantGateLink:      true,
			wantReviewLink:    true,
			wantGateCalls:     1,
			wantReviewCalls:   1,
			wantFixCalls:      1,
			wantTrackedOutput: "candidate T2\n",
		},
		{
			scenario:          "fixer-dirty",
			wantStatus:        state.TaskRepairing,
			wantAttempt:       2,
			wantHeadMatches:   true,
			wantGateLink:      true,
			wantReviewLink:    true,
			wantGateCalls:     1,
			wantReviewCalls:   1,
			wantFixCalls:      1,
			wantTrackedOutput: "dirty fixer output\n",
		},
		{
			scenario:          "gate-head-drift",
			wantStatus:        state.TaskCandidate,
			wantAttempt:       1,
			wantClean:         true,
			wantGateCalls:     1,
			wantTrackedOutput: "gate changed HEAD\n",
		},
		{
			scenario:          "gate-dirty",
			wantStatus:        state.TaskCandidate,
			wantAttempt:       1,
			wantHeadMatches:   true,
			wantGateCalls:     1,
			wantTrackedOutput: "gate dirtied worktree\n",
		},
	}

	for _, test := range tests {
		t.Run(test.scenario, func(t *testing.T) {
			fixture := newTaskCycleTestRun(t, test.scenario)
			if err := executeTaskCycle(t, fixture.run); err == nil {
				t.Fatal("TaskCycle.Run() accepted a broken repair/gate boundary")
			}

			reloaded := controlOpenRun(t, fixture.run.RepoRoot, fixture.run.ID)
			task := reloaded.State.Tasks["T2"]
			if reloaded.State.Phase != state.PhaseFailed ||
				reloaded.State.LastError == nil ||
				reloaded.State.PendingAction != nil ||
				task == nil ||
				task.Status != test.wantStatus ||
				task.Attempt != test.wantAttempt ||
				task.CandidateSHA == nil ||
				(task.GateResult != nil) != test.wantGateLink ||
				(task.ReviewResult != nil) != test.wantReviewLink {
				t.Fatalf("boundary fail-safe state = %#v", reloaded.State)
			}
			head := taskCycleGitHead(t, fixture.run.RepoRoot)
			if got := head == *task.CandidateSHA; got != test.wantHeadMatches {
				t.Fatalf(
					"HEAD matches candidate_sha = %t, want %t (HEAD=%s candidate=%s)",
					got,
					test.wantHeadMatches,
					head,
					*task.CandidateSHA,
				)
			}
			clean := strings.TrimSpace(
				controlGit(t, fixture.run.RepoRoot, "status", "--porcelain"),
			) == ""
			if clean != test.wantClean {
				t.Fatalf("worktree clean = %t, want %t", clean, test.wantClean)
			}
			if got := string(taskCycleReadFile(
				t,
				filepath.Join(fixture.run.RepoRoot, "tracked.txt"),
			)); got != test.wantTrackedOutput {
				t.Fatalf("tracked.txt = %q, want %q", got, test.wantTrackedOutput)
			}
			taskCycleAssertHelperCount(t, fixture.run.Dir, 1)
			taskCycleAssertCount(
				t,
				filepath.Join(fixture.run.Dir, "gate.count"),
				test.wantGateCalls,
			)
			taskCycleAssertCount(
				t,
				filepath.Join(fixture.run.Dir, "review.count"),
				test.wantReviewCalls,
			)
			taskCycleAssertCount(
				t,
				filepath.Join(fixture.run.Dir, "fix.count"),
				test.wantFixCalls,
			)
		})
	}
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
	return newTaskCycleTestRunWithMaxTaskAttempts(t, scenario, 0)
}

func newTaskCycleTestRunWithMaxTaskAttempts(
	t *testing.T,
	scenario string,
	maxTaskAttempts int,
) taskCycleFixture {
	t.Helper()
	repoRoot := t.TempDir()
	runID := "task-cycle-run"
	runDir := filepath.Join(repoRoot, ".coterix", "runs", runID)

	config := testConfig(t)
	if maxTaskAttempts > 0 {
		config.MaxTaskAttempts = maxTaskAttempts
	}
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
	config.CLIs["claude"] = cli.CliConfig{
		Command: command,
		Args: []string{
			"-test.run=^TestTaskCycleHelperProcess$",
			"--",
			"claude",
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
	config.GateCommand = []string{
		command,
		"-test.run=^TestTaskCycleHelperProcess$",
		"--",
		"gate",
		scenario,
		repoRoot,
		runDir,
		"T2",
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
	currentRun.State.TaskOrder = []string{"T2"}
	currentRun.State.Tasks = map[string]*state.TaskState{
		"T2": {Status: state.TaskOpen},
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

func taskCycleAssertFinalEvidence(
	t *testing.T,
	currentRun *Run,
	taskID string,
	candidateSHA string,
	staleCandidateSHA string,
) {
	t.Helper()
	task := currentRun.State.Tasks[taskID]
	if task == nil ||
		task.GateResult == nil ||
		*task.GateResult != taskEvidenceRelativePath(taskID, gateEvidenceName) ||
		task.ReviewResult == nil ||
		*task.ReviewResult != taskEvidenceRelativePath(taskID, reviewEvidenceName) {
		t.Fatalf("task %s evidence links = %#v", taskID, task)
	}
	gate, err := readGateEvidence(filepath.Join(currentRun.Dir, *task.GateResult))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGateEvidence(
		currentRun,
		taskID,
		candidateSHA,
		gate,
		true,
	); err != nil {
		t.Fatal(err)
	}
	review, err := currentRun.adapter.NewAttempt().ValidateReviewResult(
		cli.RoleImplReviewer,
		filepath.Join(currentRun.Dir, *task.ReviewResult),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateImplementationReviewTargets(
		review,
		*currentRun.State.ApprovedPlanHash,
		taskID,
		candidateSHA,
	); err != nil {
		t.Fatal(err)
	}
	if !review.Verdict.Clean {
		t.Fatalf("final review is dirty: %#v", review.Verdict)
	}
	if staleCandidateSHA != "" &&
		(gate.CandidateSHA == staleCandidateSHA ||
			review.Verdict.CandidateSHA == staleCandidateSHA) {
		t.Fatalf(
			"repair left stale evidence for candidate %s: gate=%#v review=%#v",
			staleCandidateSHA,
			gate,
			review.Verdict,
		)
	}
}

func taskCycleAssertImplementingAuthPause(t *testing.T, currentRun *Run) {
	t.Helper()
	action := currentRun.State.PendingAction
	task := currentRun.State.Tasks["T2"]
	if currentRun.State.Phase != state.PhasePausedForInput ||
		currentRun.State.LastError != nil ||
		action == nil ||
		action.Kind != state.PendingAuth ||
		action.ResumePhase != state.PhaseImplementing ||
		action.TaskID == nil ||
		*action.TaskID != "T2" ||
		action.Response != nil ||
		task == nil ||
		task.CandidateSHA == nil {
		t.Fatalf("implementing auth pause state = %#v", currentRun.State)
	}
	if head := taskCycleGitHead(t, currentRun.RepoRoot); head != *task.CandidateSHA {
		t.Fatalf("auth pause HEAD = %s, want candidate %s", head, *task.CandidateSHA)
	}
	if got := strings.TrimSpace(
		controlGit(t, currentRun.RepoRoot, "status", "--porcelain"),
	); got != "" {
		t.Fatalf("auth pause worktree is dirty: %q", got)
	}
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
	taskCycleAssertCount(t, filepath.Join(runDir, "impl.count"), want)
}

func taskCycleAssertCount(t *testing.T, path string, want int) {
	t.Helper()
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

func taskCycleIncrementHelperCount(path string) int {
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
	return value
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
