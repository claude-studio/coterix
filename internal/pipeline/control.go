package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

// Controller is the shared control plane used by the command-line and UI
// surfaces. It deliberately owns orchestration decisions while callers only
// parse input and render the returned status.
type Controller struct {
	Executor PlanExecutor

	mu     sync.Mutex
	active map[string]struct{}
}

// RunStatus is a read-only summary of one persisted run.
type RunStatus struct {
	RunID            string                     `json:"run_id"`
	Phase            state.Phase                `json:"phase"`
	PlanHash         *string                    `json:"plan_hash"`
	ApprovedPlanHash *string                    `json:"approved_plan_hash"`
	PlanRound        int                        `json:"plan_round"`
	PendingAction    *state.PendingAction       `json:"pending_action"`
	TaskOrder        []string                   `json:"task_order"`
	CurrentTaskID    *string                    `json:"current_task_id"`
	Tasks            map[string]state.TaskState `json:"tasks"`
	LastError        *string                    `json:"last_error"`
}

// NewController constructs the shared control plane around the subprocess
// executor used by planning and implementation cycles.
func NewController(executor PlanExecutor) *Controller {
	return &Controller{
		Executor: executor,
		active:   make(map[string]struct{}),
	}
}

// Run enforces repository/configuration preconditions, creates one immutable
// run snapshot, and drives its planning cycle to the next control boundary.
func (controller *Controller) Run(
	ctx context.Context,
	repoRoot string,
	request string,
) (RunStatus, error) {
	if controller == nil || controller.Executor == nil {
		return RunStatus{}, fmt.Errorf("pipeline: a control-plane executor is required")
	}
	ctx = nonNilContext(ctx)

	root, config, err := runtimePreconditions(ctx, repoRoot)
	if err != nil {
		return RunStatus{}, err
	}
	currentRun, err := CreateRun(root, "", request, config)
	if err != nil {
		return RunStatus{}, err
	}

	release, err := controller.begin(root, currentRun.ID)
	if err != nil {
		return statusFromRun(currentRun), err
	}
	defer release()

	err = NewPlanCycle(controller.Executor).Run(ctx, currentRun, "", nil)
	return statusFromRun(currentRun), err
}

// Approve binds approval to the current plan bytes, freezes plan.md, enters
// implementing, and advances the first task to its candidate boundary.
func (controller *Controller) Approve(
	ctx context.Context,
	repoRoot string,
	runID string,
) (RunStatus, error) {
	if controller == nil || controller.Executor == nil {
		return RunStatus{}, fmt.Errorf(
			"pipeline: a control-plane executor is required to approve and implement",
		)
	}
	ctx = nonNilContext(ctx)
	root, err := repositoryRoot(ctx, repoRoot)
	if err != nil {
		return RunStatus{}, err
	}
	release, err := controller.begin(root, runID)
	if err != nil {
		return RunStatus{}, err
	}
	defer release()

	currentRun, err := OpenRun(root, runID)
	if err != nil {
		return RunStatus{}, err
	}
	if currentRun.State.Phase != state.PhaseAwaitingApproval {
		return statusFromRun(currentRun), fmt.Errorf(
			"pipeline: approve requires awaiting_approval phase, got %s",
			currentRun.State.Phase,
		)
	}
	if currentRun.State.PlanHash == nil {
		return statusFromRun(currentRun), fmt.Errorf(
			"pipeline: approve requires a persisted plan_hash",
		)
	}

	planPath := filepath.Join(currentRun.Dir, planFileName)
	// Approval always trusts the file bytes, never a comparison between state
	// fields alone.
	if err := requirePlanHash(planPath, *currentRun.State.PlanHash); err != nil {
		return statusFromRun(currentRun), fmt.Errorf(
			"pipeline: approve plan hash check failed: %w",
			err,
		)
	}

	originalMode, err := freezePlan(planPath)
	if err != nil {
		return statusFromRun(currentRun), err
	}
	rollbackMode := true
	defer func() {
		if rollbackMode {
			_ = os.Chmod(planPath, originalMode)
		}
	}()

	previousPhase := currentRun.State.Phase
	previousApproved := cloneString(currentRun.State.ApprovedPlanHash)
	approvedHash := *currentRun.State.PlanHash
	currentRun.State.ApprovedPlanHash = &approvedHash

	// This is the apply-boundary rehash. VerifyApprovedPlan is also exported
	// inside the internal package so the task cycle can repeat it immediately
	// before every later application step.
	if err := VerifyApprovedPlan(currentRun); err != nil {
		currentRun.State.ApprovedPlanHash = previousApproved
		return statusFromRun(currentRun), fmt.Errorf(
			"pipeline: apply plan hash check failed: %w",
			err,
		)
	}
	if err := currentRun.State.TransitionPhase(state.PhaseImplementing); err != nil {
		currentRun.State.ApprovedPlanHash = previousApproved
		return statusFromRun(currentRun), err
	}
	if err := currentRun.SaveState(); err != nil {
		currentRun.State.Phase = previousPhase
		currentRun.State.ApprovedPlanHash = previousApproved
		return statusFromRun(currentRun), err
	}

	rollbackMode = false
	err = NewTaskCycle(controller.Executor).Run(ctx, currentRun)
	return statusFromRun(currentRun), err
}

// Reject returns an awaiting plan to planning and immediately spends the
// returned one-shot override on the human-requested revision.
func (controller *Controller) Reject(
	ctx context.Context,
	repoRoot string,
	runID string,
	response string,
) (RunStatus, error) {
	if controller == nil || controller.Executor == nil {
		return RunStatus{}, fmt.Errorf("pipeline: a control-plane executor is required")
	}
	ctx = nonNilContext(ctx)
	root, err := repositoryRoot(ctx, repoRoot)
	if err != nil {
		return RunStatus{}, err
	}
	release, err := controller.begin(root, runID)
	if err != nil {
		return RunStatus{}, err
	}
	defer release()

	currentRun, err := OpenRun(root, runID)
	if err != nil {
		return RunStatus{}, err
	}
	rejected, err := currentRun.State.RejectPlan(response)
	if err != nil {
		return statusFromRun(currentRun), err
	}
	currentRun.State.ApprovedPlanHash = nil

	err = NewPlanCycle(controller.Executor).Run(
		ctx,
		currentRun,
		rejected.Feedback,
		rejected.Override,
	)
	return statusFromRun(currentRun), err
}

// Status loads persisted state without applying runtime preconditions or
// writing any run artifact. An empty runID returns every run in name order.
func (controller *Controller) Status(
	ctx context.Context,
	repoRoot string,
	runID string,
) ([]RunStatus, error) {
	ctx = nonNilContext(ctx)
	root, err := repositoryRoot(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	if runID != "" {
		currentRun, err := OpenRun(root, runID)
		if err != nil {
			return nil, err
		}
		return []RunStatus{statusFromRun(currentRun)}, nil
	}

	runsDir := filepath.Join(root, ".coterix", "runs")
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, os.ErrNotExist) {
		return []RunStatus{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pipeline: list runs: %w", err)
	}
	statuses := make([]RunStatus, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		currentRun, err := OpenRun(root, entry.Name())
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, statusFromRun(currentRun))
	}
	return statuses, nil
}

// Resume revalidates persisted state against git before clearing the pending
// action. Planning responses are immediately passed to the reviser together
// with any one-shot cap override.
func (controller *Controller) Resume(
	ctx context.Context,
	repoRoot string,
	runID string,
	response *string,
) (RunStatus, error) {
	ctx = nonNilContext(ctx)
	root, err := repositoryRoot(ctx, repoRoot)
	if err != nil {
		return RunStatus{}, err
	}
	release, err := controller.begin(root, runID)
	if err != nil {
		return RunStatus{}, err
	}
	defer release()

	currentRun, err := OpenRun(root, runID)
	if err != nil {
		return RunStatus{}, err
	}
	if currentRun.State.Phase != state.PhasePausedForInput ||
		currentRun.State.PendingAction == nil {
		return statusFromRun(currentRun), fmt.Errorf(
			"pipeline: resume requires paused_for_input with pending_action",
		)
	}
	action := *currentRun.State.PendingAction
	needsTaskRetryExecutor := action.Kind == state.PendingTaskCap &&
		response != nil && *response == "retry"
	if (action.ResumePhase == state.PhasePlanning ||
		(action.ResumePhase == state.PhaseImplementing &&
			(action.Kind == state.PendingAuth || needsTaskRetryExecutor))) &&
		(controller == nil || controller.Executor == nil) {
		return statusFromRun(currentRun), fmt.Errorf(
			"pipeline: a control-plane executor is required to resume %s",
			action.ResumePhase,
		)
	}

	if err := verifyResumeGitFacts(ctx, currentRun); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return statusFromRun(currentRun), contextErr
		}
		failure := fmt.Errorf("pipeline: resume fail-safe: %w", err)
		if failErr := currentRun.State.Fail(failure.Error()); failErr != nil {
			return statusFromRun(currentRun), errors.Join(failure, failErr)
		}
		if saveErr := currentRun.SaveState(); saveErr != nil {
			persisted, reloadErr := reloadStatus(currentRun)
			return persisted, errors.Join(failure, saveErr, reloadErr)
		}
		return statusFromRun(currentRun), failure
	}

	resumed, err := currentRun.State.ResumePending(response)
	if err != nil {
		return statusFromRun(currentRun), err
	}
	if resumed.Aborted {
		if err := currentRun.SaveState(); err != nil {
			persisted, reloadErr := reloadStatus(currentRun)
			return persisted, errors.Join(err, reloadErr)
		}
		return statusFromRun(currentRun), nil
	}
	if currentRun.State.Phase == state.PhaseImplementing {
		if err := currentRun.SaveState(); err != nil {
			persisted, reloadErr := reloadStatus(currentRun)
			return persisted, errors.Join(err, reloadErr)
		}
		err = NewTaskCycle(controller.Executor).RunWithOverride(
			ctx,
			currentRun,
			resumed.Override,
		)
		return statusFromRun(currentRun), err
	}

	feedback := ""
	if resumed.Action.Response != nil {
		feedback = *resumed.Action.Response
	}
	err = NewPlanCycle(controller.Executor).Run(
		ctx,
		currentRun,
		feedback,
		resumed.Override,
	)
	return statusFromRun(currentRun), err
}

// VerifyApprovedPlan recalculates sha256(plan.md) and requires it to match
// both persisted plan hashes. Task application must call this immediately
// before using the frozen plan.
func VerifyApprovedPlan(currentRun *Run) error {
	if currentRun == nil || currentRun.State == nil {
		return fmt.Errorf("pipeline: an initialized run is required")
	}
	if currentRun.State.PlanHash == nil ||
		currentRun.State.ApprovedPlanHash == nil {
		return fmt.Errorf("pipeline: approved plan hashes are required")
	}
	if *currentRun.State.PlanHash != *currentRun.State.ApprovedPlanHash {
		return fmt.Errorf(
			"pipeline: plan_hash %q does not match approved_plan_hash %q",
			*currentRun.State.PlanHash,
			*currentRun.State.ApprovedPlanHash,
		)
	}
	planPath := filepath.Join(currentRun.Dir, planFileName)
	if err := requirePlanHash(
		planPath,
		*currentRun.State.ApprovedPlanHash,
	); err != nil {
		return err
	}
	info, err := os.Lstat(planPath)
	if err != nil {
		return fmt.Errorf("pipeline: inspect frozen plan.md: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() ||
		info.Mode().Perm()&0o222 != 0 {
		return fmt.Errorf("pipeline: approved plan.md is not frozen")
	}
	return nil
}

func runtimePreconditions(
	ctx context.Context,
	repoRoot string,
) (string, cli.Config, error) {
	root, err := repositoryRoot(ctx, repoRoot)
	if err != nil {
		return "", cli.Config{}, err
	}
	config, err := cli.LoadConfig(filepath.Join(root, ".coterix", "config.json"))
	if err != nil {
		return "", cli.Config{}, err
	}
	if len(config.GateCommand) == 0 ||
		strings.TrimSpace(config.GateCommand[0]) == "" {
		return "", cli.Config{}, fmt.Errorf(
			"pipeline: gate_command must not be empty",
		)
	}
	if _, err := (runner.GitMutationGuard{}).Capture(ctx, root); err != nil {
		return "", cli.Config{}, fmt.Errorf(
			"pipeline: run precondition failed: %w",
			err,
		)
	}
	return root, config, nil
}

func repositoryRoot(ctx context.Context, path string) (string, error) {
	directory, err := resolveRepositoryRoot(path)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(
		nonNilContext(ctx),
		"git",
		"rev-parse",
		"--show-toplevel",
	)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf(
			"pipeline: git repository precondition failed: %s",
			message,
		)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", fmt.Errorf("pipeline: git returned an empty repository root")
	}
	return resolveRepositoryRoot(root)
}

func verifyResumeGitFacts(ctx context.Context, currentRun *Run) error {
	snapshot, err := (runner.GitMutationGuard{}).Capture(ctx, currentRun.RepoRoot)
	if err != nil {
		return err
	}
	if currentRun.State.CurrentTaskID == nil {
		return nil
	}
	taskID := *currentRun.State.CurrentTaskID
	task := currentRun.State.Tasks[taskID]
	if task == nil {
		return nil
	}
	expected := task.CandidateSHA
	evidenceName := "candidate_sha"
	if expected == nil {
		expected = task.BaseSHA
		evidenceName = "base_sha"
	}
	if expected == nil {
		return nil
	}
	if snapshot.Head != *expected {
		return fmt.Errorf(
			"HEAD %s does not match current task %s %s %s",
			snapshot.Head,
			taskID,
			evidenceName,
			*expected,
		)
	}
	return nil
}

func freezePlan(path string) (os.FileMode, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("pipeline: inspect plan before freeze: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return 0, fmt.Errorf("pipeline: plan must be a regular file before freeze")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("pipeline: open plan before freeze: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("pipeline: inspect opened plan before freeze: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return 0, fmt.Errorf("pipeline: plan changed before it could be frozen")
	}

	originalMode := openedInfo.Mode().Perm()
	frozenMode := originalMode &^ 0o222
	if err := file.Chmod(frozenMode); err != nil {
		return 0, fmt.Errorf("pipeline: freeze plan.md: %w", err)
	}
	frozenFileInfo, err := file.Stat()
	if err != nil {
		_ = file.Chmod(originalMode)
		return 0, fmt.Errorf("pipeline: inspect frozen plan.md: %w", err)
	}
	frozenPathInfo, err := os.Lstat(path)
	if err != nil {
		_ = file.Chmod(originalMode)
		return 0, fmt.Errorf("pipeline: inspect frozen plan path: %w", err)
	}
	if frozenPathInfo.Mode()&os.ModeSymlink != 0 ||
		!frozenPathInfo.Mode().IsRegular() ||
		!os.SameFile(frozenFileInfo, frozenPathInfo) ||
		frozenFileInfo.Mode().Perm()&0o222 != 0 ||
		frozenPathInfo.Mode().Perm()&0o222 != 0 {
		_ = file.Chmod(originalMode)
		return 0, fmt.Errorf("pipeline: plan.md did not become read-only")
	}
	return originalMode, nil
}

func (controller *Controller) begin(
	repoRoot string,
	runID string,
) (func(), error) {
	if controller == nil {
		return nil, fmt.Errorf("pipeline: controller is required")
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	key := repoRoot + "\x00" + runID
	controller.mu.Lock()
	if controller.active == nil {
		controller.active = make(map[string]struct{})
	}
	if _, exists := controller.active[key]; exists {
		controller.mu.Unlock()
		return nil, fmt.Errorf(
			"pipeline: run %q is already active in this control plane",
			runID,
		)
	}
	controller.active[key] = struct{}{}
	controller.mu.Unlock()

	return func() {
		controller.mu.Lock()
		delete(controller.active, key)
		controller.mu.Unlock()
	}, nil
}

func statusFromRun(currentRun *Run) RunStatus {
	current := currentRun.State
	tasks := make(map[string]state.TaskState, len(current.Tasks))
	for taskID, task := range current.Tasks {
		if task == nil {
			continue
		}
		tasks[taskID] = state.TaskState{
			Status:       task.Status,
			Attempt:      task.Attempt,
			BaseSHA:      cloneString(task.BaseSHA),
			CandidateSHA: cloneString(task.CandidateSHA),
			GateResult:   cloneString(task.GateResult),
			ReviewResult: cloneString(task.ReviewResult),
		}
	}
	var pending *state.PendingAction
	if current.PendingAction != nil {
		copied := *current.PendingAction
		copied.TaskID = cloneString(copied.TaskID)
		copied.Response = cloneString(copied.Response)
		pending = &copied
	}
	return RunStatus{
		RunID:            currentRun.ID,
		Phase:            current.Phase,
		PlanHash:         cloneString(current.PlanHash),
		ApprovedPlanHash: cloneString(current.ApprovedPlanHash),
		PlanRound:        current.PlanRound,
		PendingAction:    pending,
		TaskOrder:        append([]string(nil), current.TaskOrder...),
		CurrentTaskID:    cloneString(current.CurrentTaskID),
		Tasks:            tasks,
		LastError:        cloneString(current.LastError),
	}
}

func reloadStatus(currentRun *Run) (RunStatus, error) {
	persisted, err := OpenRun(currentRun.RepoRoot, currentRun.ID)
	if err != nil {
		return statusFromRun(currentRun), fmt.Errorf(
			"pipeline: reload persisted status after save failure: %w",
			err,
		)
	}
	return statusFromRun(persisted), nil
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
