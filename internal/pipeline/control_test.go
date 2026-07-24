package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const controlPlan = `# Control plan

## T1: Control task
- [ ] Implement the control task
Acceptance: The control task is complete
Verify: go test ./internal/pipeline/...
`

const controlRevisedPlan = `# Revised control plan

## T1: Revised control task
- [ ] Implement the revised control task
Acceptance: The revised control task is complete
Verify: go test ./internal/pipeline/...
`

type controlPlanExecutor struct {
	mu sync.Mutex

	prompts       []string
	plannerCalls  int
	reviewerCalls int
	reviewClean   []bool

	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (executor *controlPlanExecutor) Run(
	ctx context.Context,
	request runner.RunRequest,
) (runner.RunResult, error) {
	if request.PrepareAttempt != nil {
		if err := request.PrepareAttempt(ctx, 1); err != nil {
			return runner.RunResult{}, err
		}
	}
	for _, path := range request.OutputPaths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return runner.RunResult{}, err
		}
	}
	for _, path := range []string{request.StdoutLog, request.StderrLog} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return runner.RunResult{}, err
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return runner.RunResult{}, err
		}
	}

	prompt := controlRequestPrompt(request)
	executor.mu.Lock()
	executor.prompts = append(executor.prompts, prompt)
	executor.mu.Unlock()

	if executor.entered != nil {
		executor.enteredOnce.Do(func() { close(executor.entered) })
		select {
		case <-executor.release:
		case <-ctx.Done():
			return runner.RunResult{}, ctx.Err()
		}
	}

	switch request.Effect {
	case runner.EffectArtifactOnly:
		executor.mu.Lock()
		executor.plannerCalls++
		executor.mu.Unlock()
		content := controlPlan
		if strings.Contains(prompt, "This is a REVISION.") {
			content = controlRevisedPlan
		}
		if len(request.OutputPaths) != 2 {
			return runner.RunResult{}, fmt.Errorf(
				"control fake: planner output paths = %d, want 2",
				len(request.OutputPaths),
			)
		}
		if err := os.WriteFile(
			request.OutputPaths[0],
			[]byte(content),
			0o600,
		); err != nil {
			return runner.RunResult{}, err
		}

	case runner.EffectReadOnly:
		if len(request.OutputPaths) == 0 {
			break
		}
		if filepath.Base(request.OutputPaths[0]) == reviewEvidenceName {
			runDir := filepath.Dir(filepath.Dir(filepath.Dir(request.OutputPaths[0])))
			current, err := state.Load(filepath.Join(runDir, stateFileName))
			if err != nil {
				return runner.RunResult{}, err
			}
			taskID := filepath.Base(filepath.Dir(request.OutputPaths[0]))
			task := current.Tasks[taskID]
			if current.ApprovedPlanHash == nil || task == nil ||
				task.CandidateSHA == nil {
				return runner.RunResult{}, fmt.Errorf(
					"control fake: implementation review state is incomplete",
				)
			}
			review := fmt.Sprintf(
				`{"schema_version":1,"plan_hash":%q,"task_id":%q,"candidate_sha":%q,"clean":true,"findings":[]}`,
				*current.ApprovedPlanHash,
				taskID,
				*task.CandidateSHA,
			)
			if err := os.WriteFile(
				request.OutputPaths[0],
				[]byte(review),
				0o600,
			); err != nil {
				return runner.RunResult{}, err
			}
			break
		}

		executor.mu.Lock()
		reviewIndex := executor.reviewerCalls
		executor.reviewerCalls++
		clean := true
		if reviewIndex < len(executor.reviewClean) {
			clean = executor.reviewClean[reviewIndex]
		}
		executor.mu.Unlock()

		if len(request.OutputPaths) != 1 ||
			len(request.CanonicalPaths) == 0 {
			return runner.RunResult{}, fmt.Errorf(
				"control fake: reviewer paths are incomplete",
			)
		}
		plan, err := os.ReadFile(request.CanonicalPaths[0])
		if err != nil {
			return runner.RunResult{}, err
		}
		targetHash := hashBytes(plan)
		review := cleanReviewJSON(targetHash)
		if !clean {
			review = dirtyReviewJSON(targetHash, false)
		}
		if err := os.WriteFile(
			request.OutputPaths[0],
			[]byte(review),
			0o600,
		); err != nil {
			return runner.RunResult{}, err
		}

	case runner.EffectMutating:
		if request.MaxRetries != 0 {
			return runner.RunResult{}, fmt.Errorf(
				"control fake: mutating max retries = %d, want 0",
				request.MaxRetries,
			)
		}
		if err := os.WriteFile(
			filepath.Join(request.Dir, "tracked.txt"),
			[]byte("implemented\n"),
			0o600,
		); err != nil {
			return runner.RunResult{}, err
		}
		if err := controlFakeGit(request.Dir, "add", "tracked.txt"); err != nil {
			return runner.RunResult{}, err
		}
		if err := controlFakeGit(
			request.Dir,
			"-c",
			"user.name=Coterix Control Test",
			"-c",
			"user.email=control@example.invalid",
			"commit",
			"-qm",
			"implement control task",
		); err != nil {
			return runner.RunResult{}, err
		}

	default:
		return runner.RunResult{}, fmt.Errorf(
			"control fake: unsupported effect %d",
			request.Effect,
		)
	}

	result := runner.RunResult{
		Exit:      0,
		StdoutLog: request.StdoutLog,
		StderrLog: request.StderrLog,
	}
	if request.ValidateResult != nil {
		if err := request.ValidateResult(ctx, result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (executor *controlPlanExecutor) counts() (int, int) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.plannerCalls, executor.reviewerCalls
}

func (executor *controlPlanExecutor) containsPrompt(fragment string) bool {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, prompt := range executor.prompts {
		if strings.Contains(prompt, fragment) {
			return true
		}
	}
	return false
}

func (executor *controlPlanExecutor) promptCount() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return len(executor.prompts)
}

func TestControllerRunPreconditions(t *testing.T) {
	t.Run("non-git repository", func(t *testing.T) {
		executor := &controlPlanExecutor{}
		_, err := NewController(executor).Run(
			context.Background(),
			t.TempDir(),
			"request",
		)
		if err == nil {
			t.Fatal("Run() accepted a non-git directory")
		}
		controlAssertExecutorCounts(t, executor, 0, 0)
	})

	t.Run("unborn HEAD", func(t *testing.T) {
		root, _ := controlNewRepository(t, false, testConfig(t))
		executor := &controlPlanExecutor{}
		_, err := NewController(executor).Run(
			context.Background(),
			root,
			"request",
		)
		if err == nil {
			t.Fatal("Run() accepted an unborn HEAD")
		}
		controlAssertExecutorCounts(t, executor, 0, 0)
		controlAssertNoRuns(t, root)
	})

	t.Run("dirty worktree", func(t *testing.T) {
		root, _ := controlNewRepository(t, true, testConfig(t))
		if err := os.WriteFile(
			filepath.Join(root, "tracked.txt"),
			[]byte("dirty\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		executor := &controlPlanExecutor{}
		_, err := NewController(executor).Run(
			context.Background(),
			root,
			"request",
		)
		if err == nil {
			t.Fatal("Run() accepted a dirty worktree")
		}
		controlAssertExecutorCounts(t, executor, 0, 0)
		controlAssertNoRuns(t, root)
	})

	t.Run("empty gate command", func(t *testing.T) {
		config := testConfig(t)
		config.GateCommand = []string{}
		root, _ := controlNewRepository(t, true, config)
		executor := &controlPlanExecutor{}
		_, err := NewController(executor).Run(
			context.Background(),
			root,
			"request",
		)
		if err == nil {
			t.Fatal("Run() accepted an empty gate command")
		}
		controlAssertExecutorCounts(t, executor, 0, 0)
		controlAssertNoRuns(t, root)
	})

	t.Run("ignored run artifacts remain clean", func(t *testing.T) {
		root, _ := controlNewRepository(t, true, testConfig(t))
		stale := filepath.Join(root, ".coterix", "runs", "stale")
		if err := os.MkdirAll(stale, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(stale, "artifact"),
			[]byte("ignored"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(controlGit(t, root, "status", "--porcelain")); got != "" {
			t.Fatalf("ignored run artifact dirtied repository: %q", got)
		}

		executor := &controlPlanExecutor{}
		status, err := NewController(executor).Run(
			context.Background(),
			root,
			"request",
		)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if status.RunID == "" || status.Phase != state.PhaseAwaitingApproval {
			t.Fatalf("Run() status = %#v", status)
		}
		controlAssertExecutorCounts(t, executor, 1, 1)
	})
}

func TestControllerApproveRehashesAndFreezes(t *testing.T) {
	t.Run("hash mismatch leaves approval unchanged", func(t *testing.T) {
		root, run := controlSeedAwaitingRun(t, 1, 5)
		planPath := filepath.Join(run.Dir, planFileName)
		if err := os.WriteFile(
			planPath,
			[]byte(controlPlan+"\nchanged\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}

		_, err := NewController(&controlPlanExecutor{}).Approve(
			context.Background(),
			root,
			run.ID,
		)
		if err == nil {
			t.Fatal("Approve() accepted changed plan bytes")
		}
		reloaded := controlOpenRun(t, root, run.ID)
		if reloaded.State.Phase != state.PhaseAwaitingApproval ||
			reloaded.State.ApprovedPlanHash != nil {
			t.Fatalf("mismatched approval mutated state: %#v", reloaded.State)
		}
		info, statErr := os.Stat(planPath)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm()&0o200 == 0 {
			t.Fatalf("rejected plan mode = %o, want writable", info.Mode().Perm())
		}
	})

	t.Run("success binds hash and freezes actual plan", func(t *testing.T) {
		root, run := controlSeedAwaitingRun(t, 1, 5)
		planPath := filepath.Join(run.Dir, planFileName)
		wantHash := *run.State.PlanHash

		status, err := NewController(&controlPlanExecutor{}).Approve(
			context.Background(),
			root,
			run.ID,
		)
		if err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
		task := status.Tasks["T1"]
		if status.Phase != state.PhaseDone ||
			status.ApprovedPlanHash == nil ||
			*status.ApprovedPlanHash != wantHash ||
			status.CurrentTaskID == nil ||
			*status.CurrentTaskID != "T1" ||
			task.Status != state.TaskConfirmed ||
			task.BaseSHA == nil ||
			task.CandidateSHA == nil ||
			*task.BaseSHA == *task.CandidateSHA ||
			task.GateResult == nil ||
			task.ReviewResult == nil {
			t.Fatalf("approved status = %#v", status)
		}
		info, err := os.Stat(planPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("frozen plan mode = %o", info.Mode().Perm())
		}

		reloaded := controlOpenRun(t, root, run.ID)
		if err := VerifyApprovedPlan(reloaded); err != nil {
			t.Fatalf("VerifyApprovedPlan() error = %v", err)
		}
		frozenMode := info.Mode().Perm()
		if err := os.Chmod(planPath, frozenMode|0o200); err != nil {
			t.Fatal(err)
		}
		if err := VerifyApprovedPlan(reloaded); err == nil {
			t.Fatal("VerifyApprovedPlan() accepted restored plan write permission")
		}
		if err := os.Chmod(planPath, frozenMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(planPath, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			planPath,
			[]byte(controlRevisedPlan),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if err := VerifyApprovedPlan(reloaded); err == nil {
			t.Fatal("VerifyApprovedPlan() did not rehash changed bytes")
		}
	})
}

func TestControllerRejectPassesFeedbackAndOverridesCap(t *testing.T) {
	root, run := controlSeedAwaitingRun(t, 1, 1)
	executor := &controlPlanExecutor{}
	response := "Change the verification before approval."

	status, err := NewController(executor).Reject(
		context.Background(),
		root,
		run.ID,
		response,
	)
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if status.Phase != state.PhaseAwaitingApproval ||
		status.PlanRound != 2 ||
		status.PendingAction != nil {
		t.Fatalf("rejected status = %#v", status)
	}
	if !executor.containsPrompt(response) {
		t.Fatal("Reject() did not pass human feedback to the reviser")
	}
	controlAssertExecutorCounts(t, executor, 1, 1)
}

func TestControllerResumePlanningFeedbackAndOneShot(t *testing.T) {
	t.Run("plan question response reaches reviser", func(t *testing.T) {
		root, run := controlSeedPlanningPause(
			t,
			state.PendingPlanQuestion,
			1,
			5,
		)
		executor := &controlPlanExecutor{}
		response := "Target the pipeline package."

		status, err := NewController(executor).Resume(
			context.Background(),
			root,
			run.ID,
			&response,
		)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if status.Phase != state.PhaseAwaitingApproval ||
			status.PendingAction != nil ||
			status.PlanRound != 2 {
			t.Fatalf("question resume status = %#v", status)
		}
		if !executor.containsPrompt(response) {
			t.Fatal("plan_question response was not passed to the reviser")
		}
		controlAssertExecutorCounts(t, executor, 1, 1)
	})

	t.Run("plan cap permits only one over-cap revision", func(t *testing.T) {
		root, run := controlSeedPlanningPause(
			t,
			state.PendingPlanCap,
			1,
			1,
		)
		executor := &controlPlanExecutor{reviewClean: []bool{false}}
		response := "Try one more revision."

		status, err := NewController(executor).Resume(
			context.Background(),
			root,
			run.ID,
			&response,
		)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if status.Phase != state.PhasePausedForInput ||
			status.PendingAction == nil ||
			status.PendingAction.Kind != state.PendingPlanCap ||
			status.PlanRound != 2 {
			t.Fatalf("plan-cap resume status = %#v", status)
		}
		if !executor.containsPrompt(response) {
			t.Fatal("plan_cap response was not passed to the reviser")
		}
		controlAssertExecutorCounts(t, executor, 1, 1)
	})
}

func TestControllerResumeAuthResponseRules(t *testing.T) {
	root, run := controlSeedPlanningPause(t, state.PendingAuth, 1, 5)
	executor := &controlPlanExecutor{}
	controller := NewController(executor)
	response := ""

	if _, err := controller.Resume(
		context.Background(),
		root,
		run.ID,
		&response,
	); err == nil {
		t.Fatal("Resume() accepted an auth response")
	}
	reloaded := controlOpenRun(t, root, run.ID)
	if reloaded.State.Phase != state.PhasePausedForInput ||
		reloaded.State.PendingAction == nil ||
		reloaded.State.PendingAction.Kind != state.PendingAuth {
		t.Fatalf("rejected auth response mutated state: %#v", reloaded.State)
	}

	status, err := controller.Resume(
		context.Background(),
		root,
		run.ID,
		nil,
	)
	if err != nil {
		t.Fatalf("response-free auth Resume() error = %v", err)
	}
	if status.Phase != state.PhaseAwaitingApproval ||
		status.PendingAction != nil {
		t.Fatalf("response-free auth status = %#v", status)
	}
	controlAssertExecutorCounts(t, executor, 0, 1)
}

func TestControllerResumeImplementingAuthRunsCurrentTask(t *testing.T) {
	root, run, base := controlSeedOpenAuthPause(t)
	executor := &controlPlanExecutor{}

	status, err := NewController(executor).Resume(
		context.Background(),
		root,
		run.ID,
		nil,
	)
	if err != nil {
		t.Fatalf("Resume() implementing auth error = %v", err)
	}
	task := status.Tasks["T1"]
	if status.Phase != state.PhaseDone ||
		status.PendingAction != nil ||
		status.CurrentTaskID == nil ||
		*status.CurrentTaskID != "T1" ||
		task.Status != state.TaskConfirmed ||
		task.Attempt != 2 ||
		task.BaseSHA == nil ||
		*task.BaseSHA != base ||
		task.CandidateSHA == nil ||
		*task.CandidateSHA == base ||
		task.GateResult == nil ||
		task.ReviewResult == nil {
		t.Fatalf("implementing auth resume status = %#v", status)
	}
	if !executor.containsPrompt("## T1: Control task") {
		t.Fatal("implementing auth resume did not dispatch the current task")
	}
}

func TestControllerResumeImplementingAuthEscalatesReachedTaskCap(t *testing.T) {
	root, run, base := controlSeedOpenAuthPauseAtAttempt(t, 5)
	executor := &controlPlanExecutor{}

	status, err := NewController(executor).Resume(
		context.Background(),
		root,
		run.ID,
		nil,
	)
	if err != nil {
		t.Fatalf("Resume() at task cap error = %v", err)
	}
	action := status.PendingAction
	task := status.Tasks["T1"]
	if status.Phase != state.PhasePausedForInput ||
		action == nil ||
		action.Kind != state.PendingTaskCap ||
		action.ResumePhase != state.PhaseImplementing ||
		action.TaskID == nil ||
		*action.TaskID != "T1" ||
		action.Response != nil ||
		task.Status != state.TaskOpen ||
		task.Attempt != 5 ||
		task.BaseSHA == nil ||
		*task.BaseSHA != base ||
		task.CandidateSHA != nil {
		t.Fatalf("task-cap escalation status = %#v", status)
	}
	if executor.promptCount() != 0 {
		t.Fatal("task-cap escalation dispatched another mutating attempt")
	}
	if got := strings.TrimSpace(
		controlGit(t, root, "rev-parse", "--verify", "HEAD"),
	); got != base {
		t.Fatalf("task-cap escalation changed HEAD from %s to %s", base, got)
	}
}

func TestControllerResumeImplementingAuthRejectsBaseDrift(t *testing.T) {
	root, run, _ := controlSeedOpenAuthPause(t)
	if err := os.WriteFile(
		filepath.Join(root, "tracked.txt"),
		[]byte("drifted\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	controlGit(t, root, "add", "tracked.txt")
	controlCommit(t, root, "drift after auth pause")
	driftedHead := strings.TrimSpace(
		controlGit(t, root, "rev-parse", "--verify", "HEAD"),
	)
	executor := &controlPlanExecutor{}

	if _, err := NewController(executor).Resume(
		context.Background(),
		root,
		run.ID,
		nil,
	); err == nil {
		t.Fatal("Resume() accepted clean HEAD drift from open-task base_sha")
	}
	reloaded := controlOpenRun(t, root, run.ID)
	if reloaded.State.Phase != state.PhaseFailed ||
		reloaded.State.PendingAction != nil ||
		reloaded.State.LastError == nil {
		t.Fatalf("base drift fail-safe state = %#v", reloaded.State)
	}
	if got := strings.TrimSpace(
		controlGit(t, root, "rev-parse", "--verify", "HEAD"),
	); got != driftedHead {
		t.Fatalf("resume fail-safe changed drifted HEAD from %s to %s", driftedHead, got)
	}
	if executor.promptCount() != 0 {
		t.Fatal("resume fail-safe dispatched an executor after base drift")
	}
}

func TestControllerResumeTaskCapResponseRules(t *testing.T) {
	t.Run("invalid responses preserve paused state and retry resumes", func(t *testing.T) {
		root, run := controlSeedTaskCapPause(t)
		controller := NewController(&controlPlanExecutor{})
		statePath := filepath.Join(run.Dir, stateFileName)
		before := controlReadFile(t, statePath)

		if _, err := controller.Resume(
			context.Background(),
			root,
			run.ID,
			nil,
		); err == nil {
			t.Fatal("Resume() accepted a missing task_cap response")
		}
		if got := controlReadFile(t, statePath); string(got) != string(before) {
			t.Fatal("missing task_cap response mutated persisted state")
		}

		invalid := "later"
		if _, err := controller.Resume(
			context.Background(),
			root,
			run.ID,
			&invalid,
		); err == nil {
			t.Fatal("Resume() accepted an invalid task_cap response")
		}
		if got := controlReadFile(t, statePath); string(got) != string(before) {
			t.Fatal("invalid task_cap response mutated persisted state")
		}

		retry := "retry"
		status, err := controller.Resume(
			context.Background(),
			root,
			run.ID,
			&retry,
		)
		if err != nil {
			t.Fatalf("Resume(retry) error = %v", err)
		}
		if status.Phase != state.PhaseDone ||
			status.PendingAction != nil ||
			status.Tasks["T1"].Status != state.TaskConfirmed ||
			status.Tasks["T1"].Attempt != run.Config.MaxTaskAttempts+1 {
			t.Fatalf("task_cap retry status = %#v", status)
		}
	})

	t.Run("abort fails run and current task", func(t *testing.T) {
		root, run := controlSeedTaskCapPause(t)
		abort := "abort"

		status, err := NewController(nil).Resume(
			context.Background(),
			root,
			run.ID,
			&abort,
		)
		if err != nil {
			t.Fatalf("Resume(abort) error = %v", err)
		}
		if status.Phase != state.PhaseFailed ||
			status.PendingAction != nil ||
			status.Tasks["T1"].Status != state.TaskFailed ||
			status.LastError == nil {
			t.Fatalf("task_cap abort status = %#v", status)
		}
		reloaded := controlOpenRun(t, root, run.ID)
		if reloaded.State.Phase != state.PhaseFailed ||
			reloaded.State.Tasks["T1"].Status != state.TaskFailed ||
			reloaded.State.LastError == nil {
			t.Fatalf("persisted task_cap abort state = %#v", reloaded.State)
		}
	})
}

func TestControllerCanceledResumePreservesPausedState(t *testing.T) {
	root, run := controlSeedPlanningPause(
		t,
		state.PendingPlanQuestion,
		1,
		5,
	)
	statePath := filepath.Join(run.Dir, stateFileName)
	before := controlReadFile(t, statePath)
	executor := &controlPlanExecutor{}
	response := "continue"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewController(executor).Resume(
		ctx,
		root,
		run.ID,
		&response,
	); err == nil {
		t.Fatal("Resume() accepted a canceled context")
	}
	if got := controlReadFile(t, statePath); string(got) != string(before) {
		t.Fatal("canceled Resume() mutated persisted state")
	}
	reloaded := controlOpenRun(t, root, run.ID)
	if reloaded.State.Phase != state.PhasePausedForInput ||
		reloaded.State.PendingAction == nil ||
		reloaded.State.PendingAction.Kind != state.PendingPlanQuestion {
		t.Fatalf("canceled resume state = %#v", reloaded.State)
	}
	controlAssertExecutorCounts(t, executor, 0, 0)
}

func TestControllerResumeGitFailSafe(t *testing.T) {
	t.Run("dirty worktree persists failed state", func(t *testing.T) {
		root, run := controlSeedPlanningPause(
			t,
			state.PendingPlanQuestion,
			1,
			5,
		)
		dirtyPath := filepath.Join(root, "tracked.txt")
		if err := os.WriteFile(dirtyPath, []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := &controlPlanExecutor{}
		response := "continue"

		if _, err := NewController(executor).Resume(
			context.Background(),
			root,
			run.ID,
			&response,
		); err == nil {
			t.Fatal("Resume() accepted a dirty worktree")
		}
		reloaded := controlOpenRun(t, root, run.ID)
		if reloaded.State.Phase != state.PhaseFailed ||
			reloaded.State.LastError == nil ||
			!strings.Contains(*reloaded.State.LastError, "worktree is not clean") {
			t.Fatalf("dirty fail-safe state = %#v", reloaded.State)
		}
		content, err := os.ReadFile(dirtyPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "dirty\n" {
			t.Fatalf("fail-safe reconciled dirty content to %q", content)
		}
		controlAssertExecutorCounts(t, executor, 0, 0)
	})

	t.Run("candidate HEAD drift persists failed state", func(t *testing.T) {
		root, run, candidate := controlSeedCandidateAuthPause(t)
		if err := os.WriteFile(
			filepath.Join(root, "tracked.txt"),
			[]byte("next\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		controlGit(t, root, "add", "tracked.txt")
		controlCommit(t, root, "advance HEAD")
		driftedHead := strings.TrimSpace(
			controlGit(t, root, "rev-parse", "--verify", "HEAD"),
		)
		if driftedHead == candidate {
			t.Fatal("test fixture did not advance HEAD")
		}

		executor := &controlPlanExecutor{}
		if _, err := NewController(executor).Resume(
			context.Background(),
			root,
			run.ID,
			nil,
		); err == nil {
			t.Fatal("Resume() accepted candidate HEAD drift")
		}
		reloaded := controlOpenRun(t, root, run.ID)
		if reloaded.State.Phase != state.PhaseFailed ||
			reloaded.State.LastError == nil ||
			!strings.Contains(*reloaded.State.LastError, "candidate_sha") {
			t.Fatalf("HEAD drift fail-safe state = %#v", reloaded.State)
		}
		gotHead := strings.TrimSpace(
			controlGit(t, root, "rev-parse", "--verify", "HEAD"),
		)
		if gotHead != driftedHead {
			t.Fatalf("fail-safe reconciled HEAD to %s, want %s", gotHead, driftedHead)
		}
		controlAssertExecutorCounts(t, executor, 0, 0)
	})
}

func TestControllerStatusIsReadOnly(t *testing.T) {
	root, run := controlSeedAwaitingRun(t, 1, 5)
	statePath := filepath.Join(run.Dir, stateFileName)
	planPath := filepath.Join(run.Dir, planFileName)
	beforeState := controlReadFile(t, statePath)
	beforePlan := controlReadFile(t, planPath)
	beforeInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "tracked.txt"),
		[]byte("dirty status fixture\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	beforeGit := controlGit(t, root, "status", "--porcelain")

	controller := NewController(nil)
	explicit, err := controller.Status(
		context.Background(),
		root,
		run.ID,
	)
	if err != nil {
		t.Fatalf("Status(runID) error = %v", err)
	}
	all, err := controller.Status(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Status(all) error = %v", err)
	}
	if len(explicit) != 1 || explicit[0].RunID != run.ID ||
		len(all) != 1 || all[0].RunID != run.ID {
		t.Fatalf("status results: explicit=%#v all=%#v", explicit, all)
	}
	afterInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(controlReadFile(t, statePath)) != string(beforeState) ||
		string(controlReadFile(t, planPath)) != string(beforePlan) ||
		!afterInfo.ModTime().Equal(beforeInfo.ModTime()) ||
		controlGit(t, root, "status", "--porcelain") != beforeGit {
		t.Fatal("Status() mutated state, plan, timestamps, or git facts")
	}
}

func TestControllerRejectsConcurrentStartForSameRun(t *testing.T) {
	root, run := controlSeedPlanningPause(
		t,
		state.PendingPlanQuestion,
		1,
		5,
	)
	executor := &controlPlanExecutor{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	controller := NewController(executor)
	response := "continue"
	firstDone := make(chan error, 1)
	go func() {
		_, err := controller.Resume(
			context.Background(),
			root,
			run.ID,
			&response,
		)
		firstDone <- err
	}()

	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first Resume() did not reach the executor")
	}
	if _, err := controller.Resume(
		context.Background(),
		root,
		run.ID,
		&response,
	); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("second Resume() error = %v, want already active", err)
	}
	close(executor.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Resume() did not finish")
	}
	controlAssertExecutorCounts(t, executor, 1, 1)
}

func controlRequestPrompt(request runner.RunRequest) string {
	if len(request.Stdin) != 0 {
		return string(request.Stdin)
	}
	if len(request.Args) != 0 {
		return request.Args[len(request.Args)-1]
	}
	return ""
}

func controlNewRepository(
	t *testing.T,
	commit bool,
	config cli.Config,
) (string, cli.Config) {
	t.Helper()
	root := t.TempDir()
	controlGit(t, root, "init", "-q")
	if err := os.MkdirAll(filepath.Join(root, ".coterix"), 0o700); err != nil {
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
		[]byte("base\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	controlWriteConfig(t, root, config)
	if commit {
		controlGit(t, root, "add", ".")
		controlCommit(t, root, "baseline")
	}
	return root, config
}

func controlWriteConfig(t *testing.T, root string, config cli.Config) {
	t.Helper()
	content, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(
		filepath.Join(root, ".coterix", "config.json"),
		content,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func controlSeedAwaitingRun(
	t *testing.T,
	planRound int,
	maxPlanRounds int,
) (string, *Run) {
	t.Helper()
	config := testConfig(t)
	config.MaxPlanRounds = maxPlanRounds
	root, _ := controlNewRepository(t, true, config)
	run, err := CreateRun(root, "control-run", "control request", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(run.Dir, planFileName),
		[]byte(controlPlan),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	planHash := hashBytes([]byte(controlPlan))
	run.State.PlanHash = &planHash
	run.State.PlanRound = planRound
	run.State.TaskOrder = []string{"T1"}
	run.State.Tasks = map[string]*state.TaskState{
		"T1": {Status: state.TaskOpen},
	}
	if err := run.State.TransitionPhase(state.PhaseAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}
	return root, run
}

func controlSeedPlanningPause(
	t *testing.T,
	kind state.PendingKind,
	planRound int,
	maxPlanRounds int,
) (string, *Run) {
	t.Helper()
	root, run := controlSeedAwaitingRun(t, planRound, maxPlanRounds)
	if _, err := run.State.RejectPlan("seed planning pause"); err != nil {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case state.PendingPlanQuestion:
		err = run.State.PauseForPlanQuestion("Which target?")
	case state.PendingPlanCap:
		err = run.State.PauseForPlanCap("Plan cap reached")
	case state.PendingAuth:
		err = run.State.PauseForAuth(nil, "Authenticate externally")
	default:
		t.Fatalf("unsupported planning pause kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}
	return root, run
}

func controlSeedTaskCapPause(t *testing.T) (string, *Run) {
	t.Helper()
	root, run := controlSeedAwaitingRun(t, 1, 5)
	planHash := *run.State.PlanHash
	run.State.ApprovedPlanHash = &planHash
	taskID := "T1"
	run.State.CurrentTaskID = &taskID
	run.State.Tasks[taskID].Attempt = run.Config.MaxTaskAttempts
	if _, err := freezePlan(filepath.Join(run.Dir, planFileName)); err != nil {
		t.Fatal(err)
	}
	if err := run.State.TransitionPhase(state.PhaseImplementing); err != nil {
		t.Fatal(err)
	}
	if err := run.State.PauseForTaskCap(
		taskID,
		"Task attempt cap reached",
	); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}
	return root, run
}

func controlSeedCandidateAuthPause(t *testing.T) (string, *Run, string) {
	t.Helper()
	root, run := controlSeedAwaitingRun(t, 1, 5)
	planHash := *run.State.PlanHash
	run.State.ApprovedPlanHash = &planHash
	taskID := "T1"
	run.State.CurrentTaskID = &taskID
	run.State.Tasks[taskID].Status = state.TaskCandidate
	candidate := strings.TrimSpace(
		controlGit(t, root, "rev-parse", "--verify", "HEAD"),
	)
	run.State.Tasks[taskID].CandidateSHA = &candidate
	if err := run.State.TransitionPhase(state.PhaseImplementing); err != nil {
		t.Fatal(err)
	}
	if err := run.State.PauseForAuth(&taskID, "Authenticate externally"); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}
	return root, run, candidate
}

func controlSeedOpenAuthPause(t *testing.T) (string, *Run, string) {
	return controlSeedOpenAuthPauseAtAttempt(t, 1)
}

func controlSeedOpenAuthPauseAtAttempt(
	t *testing.T,
	attempt int,
) (string, *Run, string) {
	t.Helper()
	root, run := controlSeedAwaitingRun(t, 1, 5)
	planHash := *run.State.PlanHash
	run.State.ApprovedPlanHash = &planHash
	taskID := "T1"
	run.State.CurrentTaskID = &taskID
	base := strings.TrimSpace(
		controlGit(t, root, "rev-parse", "--verify", "HEAD"),
	)
	run.State.Tasks[taskID].Attempt = attempt
	run.State.Tasks[taskID].BaseSHA = &base
	if _, err := freezePlan(filepath.Join(run.Dir, planFileName)); err != nil {
		t.Fatal(err)
	}
	if err := run.State.TransitionPhase(state.PhaseImplementing); err != nil {
		t.Fatal(err)
	}
	if err := run.State.PauseForAuth(&taskID, "Authenticate externally"); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}
	return root, run, base
}

func controlOpenRun(t *testing.T, root, runID string) *Run {
	t.Helper()
	run, err := OpenRun(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func controlAssertExecutorCounts(
	t *testing.T,
	executor *controlPlanExecutor,
	planner int,
	reviewer int,
) {
	t.Helper()
	gotPlanner, gotReviewer := executor.counts()
	if gotPlanner != planner || gotReviewer != reviewer {
		t.Fatalf(
			"executor calls = planner %d reviewer %d, want %d %d",
			gotPlanner,
			gotReviewer,
			planner,
			reviewer,
		)
	}
}

func controlAssertNoRuns(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".coterix", "runs"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("precondition failure created %d run entries", len(entries))
	}
}

func controlReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func controlGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func controlCommit(t *testing.T, root, message string) {
	t.Helper()
	controlGit(
		t,
		root,
		"-c",
		"user.name=Coterix Control Test",
		"-c",
		"user.email=control@example.invalid",
		"commit",
		"-qm",
		message,
	)
}

func controlFakeGit(root string, args ...string) error {
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"git %s: %w\n%s",
			strings.Join(args, " "),
			err,
			output,
		)
	}
	return nil
}
