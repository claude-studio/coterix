package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/prompt"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

// TaskCycle advances one approved-plan task from open to candidate. Gate,
// review, repair, and confirmation belong to the next pipeline milestone.
type TaskCycle struct {
	Executor PlanExecutor
	OnLine   func(runner.Line)
}

// NewTaskCycle constructs an implementation cycle around a subprocess executor.
func NewTaskCycle(executor PlanExecutor) *TaskCycle {
	return &TaskCycle{Executor: executor}
}

// Run advances the current task to the candidate boundary. It never advances
// past a candidate because that task must still pass the later gate and review.
func (cycle *TaskCycle) Run(ctx context.Context, currentRun *Run) error {
	if cycle == nil || cycle.Executor == nil {
		return fmt.Errorf("pipeline: a task executor is required")
	}
	if currentRun == nil || currentRun.State == nil || currentRun.adapter == nil {
		return fmt.Errorf("pipeline: an initialized run is required")
	}
	if err := currentRun.State.Validate(); err != nil {
		return fmt.Errorf("pipeline: validate run state: %w", err)
	}
	if currentRun.State.Phase != state.PhaseImplementing {
		return fmt.Errorf(
			"pipeline: task cycle requires implementing phase, got %s",
			currentRun.State.Phase,
		)
	}
	ctx = nonNilContext(ctx)

	plan, err := cycle.approvedPlan(currentRun)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	task, cursorChanged, err := currentTask(plan, currentRun.State)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	if cursorChanged {
		if err := currentRun.SaveState(); err != nil {
			return cycle.fail(currentRun, err)
		}
	}
	if task == nil {
		return nil
	}

	taskState := currentRun.State.Tasks[task.ID]
	history, err := recentCommitHistory(ctx, currentRun.RepoRoot)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	rendered, err := prompt.Render(
		prompt.ImplementationTemplate,
		prompt.Values{
			"PLAN_PATH":      filepath.Join(currentRun.Dir, planFileName),
			"TASK_BODY":      task.Body,
			"COMMIT_HISTORY": history,
			"STALL_NOTE":     "",
		},
	)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	request, cliName, err := cycle.implementationRequest(
		currentRun,
		task.ID,
		rendered,
	)
	if err != nil {
		return cycle.fail(currentRun, err)
	}

	guard := runner.GitMutationGuard{}
	start, err := guard.Capture(ctx, currentRun.RepoRoot)
	if err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: implementation precondition failed: %w", err),
		)
	}

	previousBase := cloneString(taskState.BaseSHA)
	previousAttempt := taskState.Attempt
	taskState.BaseSHA = stringPointer(start.Head)
	if err := currentRun.State.BeginTaskAttempt(
		task.ID,
		currentRun.Config.MaxTaskAttempts,
		nil,
	); err != nil {
		taskState.BaseSHA = previousBase
		taskState.Attempt = previousAttempt
		var capErr *state.CapError
		if errors.As(err, &capErr) {
			if pauseErr := currentRun.State.PauseForTaskCap(
				task.ID,
				fmt.Sprintf(
					"Task %s attempt cap reached (%d >= %d). Respond with retry or abort.",
					task.ID,
					capErr.Current,
					capErr.Maximum,
				),
			); pauseErr != nil {
				return cycle.fail(currentRun, errors.Join(err, pauseErr))
			}
			if saveErr := currentRun.SaveState(); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return nil
		}
		return cycle.fail(currentRun, err)
	}
	if err := currentRun.SaveState(); err != nil {
		taskState.BaseSHA = previousBase
		taskState.Attempt = previousAttempt
		return cycle.fail(currentRun, err)
	}

	// Recheck both frozen plan bytes and the exact git snapshot at the apply
	// boundary, after the starting evidence is durable and before the subprocess.
	if err := VerifyApprovedPlan(currentRun); err != nil {
		return cycle.fail(currentRun, err)
	}
	if err := guard.Verify(ctx, currentRun.RepoRoot, start); err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: implementation snapshot changed before start: %w", err),
		)
	}

	result, runErr := cycle.Executor.Run(ctx, request)
	if runErr != nil {
		return cycle.handleExecutionFailure(
			ctx,
			currentRun,
			task.ID,
			cliName,
			start,
			request.StderrLog,
			result,
			runErr,
		)
	}

	candidate, err := cli.ValidateCommittedCandidate(
		ctx,
		cli.RoleImplWriter,
		currentRun.RepoRoot,
		start.Head,
	)
	if err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: implementation postcondition failed: %w", err),
		)
	}
	if err := VerifyApprovedPlan(currentRun); err != nil {
		return cycle.fail(currentRun, err)
	}
	if err := guard.Verify(
		ctx,
		currentRun.RepoRoot,
		runner.MutationSnapshot{Head: candidate.CandidateSHA},
	); err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: candidate changed before it could be recorded: %w", err),
		)
	}

	taskState.CandidateSHA = stringPointer(candidate.CandidateSHA)
	if err := currentRun.State.TransitionTask(task.ID, state.TaskCandidate); err != nil {
		taskState.CandidateSHA = nil
		return cycle.fail(currentRun, err)
	}
	if err := currentRun.SaveState(); err != nil {
		return cycle.fail(currentRun, err)
	}
	return nil
}

func (cycle *TaskCycle) approvedPlan(currentRun *Run) (Plan, error) {
	if err := VerifyApprovedPlan(currentRun); err != nil {
		return Plan{}, fmt.Errorf("pipeline: apply plan hash check failed: %w", err)
	}
	planPath := filepath.Join(currentRun.Dir, planFileName)
	content, err := readRegularFile(planPath)
	if err != nil {
		return Plan{}, err
	}
	if hashBytes(content) != *currentRun.State.ApprovedPlanHash {
		return Plan{}, fmt.Errorf("pipeline: approved plan changed while being read")
	}
	plan, err := ParsePlan(content)
	if err != nil {
		return Plan{}, fmt.Errorf("pipeline: parse approved plan: %w", err)
	}
	if len(plan.Tasks) != len(currentRun.State.TaskOrder) {
		return Plan{}, fmt.Errorf(
			"pipeline: approved plan has %d tasks but state has %d",
			len(plan.Tasks),
			len(currentRun.State.TaskOrder),
		)
	}
	for index, task := range plan.Tasks {
		if currentRun.State.TaskOrder[index] != task.ID {
			return Plan{}, fmt.Errorf(
				"pipeline: approved plan task %d is %s but state cursor order has %s",
				index,
				task.ID,
				currentRun.State.TaskOrder[index],
			)
		}
	}
	return plan, nil
}

// currentTask returns an open task to implement, or nil at the candidate/all
// confirmed boundary. It only advances from a confirmed cursor in task_order.
func currentTask(plan Plan, current *state.State) (*PlanTask, bool, error) {
	start := 0
	if current.CurrentTaskID != nil {
		index := -1
		for candidateIndex, taskID := range current.TaskOrder {
			if taskID == *current.CurrentTaskID {
				index = candidateIndex
				break
			}
		}
		if index < 0 {
			return nil, false, fmt.Errorf(
				"pipeline: current task %q is absent from task_order",
				*current.CurrentTaskID,
			)
		}
		for previous := 0; previous < index; previous++ {
			taskID := current.TaskOrder[previous]
			if current.Tasks[taskID].Status != state.TaskConfirmed {
				return nil, false, fmt.Errorf(
					"pipeline: task %s precedes current task %s but is not confirmed",
					taskID,
					*current.CurrentTaskID,
				)
			}
		}

		taskState := current.Tasks[*current.CurrentTaskID]
		switch taskState.Status {
		case state.TaskOpen:
			task := plan.Tasks[index]
			return &task, false, nil
		case state.TaskCandidate:
			return nil, false, nil
		case state.TaskConfirmed:
			start = index + 1
		default:
			return nil, false, fmt.Errorf(
				"pipeline: task cycle cannot handle current task %s in status %s",
				*current.CurrentTaskID,
				taskState.Status,
			)
		}
	}

	for index := start; index < len(current.TaskOrder); index++ {
		taskID := current.TaskOrder[index]
		taskState := current.Tasks[taskID]
		if taskState.Status == state.TaskConfirmed {
			continue
		}
		if taskState.Status != state.TaskOpen &&
			taskState.Status != state.TaskCandidate {
			return nil, false, fmt.Errorf(
				"pipeline: task cycle cannot select task %s in status %s",
				taskID,
				taskState.Status,
			)
		}
		current.CurrentTaskID = stringPointer(taskID)
		if taskState.Status == state.TaskCandidate {
			return nil, true, nil
		}
		task := plan.Tasks[index]
		return &task, true, nil
	}
	return nil, false, nil
}

func (cycle *TaskCycle) implementationRequest(
	currentRun *Run,
	taskID string,
	rendered string,
) (runner.RunRequest, string, error) {
	config, err := currentRun.Config.CLIForRole(cli.RoleImplWriter)
	if err != nil {
		return runner.RunRequest{}, "", err
	}
	cliName, exists := currentRun.Config.Roles[cli.RoleImplWriter]
	if !exists {
		return runner.RunRequest{}, "", fmt.Errorf(
			"pipeline: role %q has no CLI mapping",
			cli.RoleImplWriter,
		)
	}
	policy, err := currentRun.adapter.PolicyForRole(
		cli.RoleImplWriter,
		cli.ResultPaths{},
	)
	if err != nil {
		return runner.RunRequest{}, "", err
	}

	args := append([]string(nil), config.Args...)
	var stdin []byte
	if config.Stdin {
		stdin = []byte(rendered)
	} else {
		args = append(args, rendered)
	}
	logSuffix, err := randomLogSuffix()
	if err != nil {
		return runner.RunRequest{}, "", err
	}
	logPrefix := fmt.Sprintf(
		"task-%s-attempt-%d-%s-%s",
		taskID,
		currentRun.State.Tasks[taskID].Attempt+1,
		cli.RoleImplWriter,
		logSuffix,
	)
	return runner.RunRequest{
		Command:     config.Command,
		Args:        args,
		Dir:         currentRun.RepoRoot,
		Env:         config.Env,
		Stdin:       stdin,
		IdleTimeout: currentRun.Config.IdleTimeout(),
		// Mutating steps never receive automatic retries. EffectMutating also
		// independently enforces this inside the production runner.
		MaxRetries: 0,
		Effect:     policy.Effect,
		StdoutLog: filepath.Join(
			currentRun.Dir,
			"logs",
			logPrefix+".stdout.log",
		),
		StderrLog: filepath.Join(
			currentRun.Dir,
			"logs",
			logPrefix+".stderr.log",
		),
		OnLine: cycle.OnLine,
	}, cliName, nil
}

func (cycle *TaskCycle) handleExecutionFailure(
	ctx context.Context,
	currentRun *Run,
	taskID string,
	cliName string,
	start runner.MutationSnapshot,
	stderrLog string,
	result runner.RunResult,
	runErr error,
) error {
	verifyContext := context.WithoutCancel(nonNilContext(ctx))
	guard := runner.GitMutationGuard{}
	if err := guard.Verify(
		verifyContext,
		currentRun.RepoRoot,
		start,
	); err != nil {
		return cycle.fail(
			currentRun,
			errors.Join(
				runErr,
				fmt.Errorf(
					"pipeline: implementation starting snapshot no longer matches: %w",
					err,
				),
			),
		)
	}
	if err := VerifyApprovedPlan(currentRun); err != nil {
		return cycle.fail(currentRun, errors.Join(runErr, err))
	}

	var authErr *cli.AuthFailure
	classified := runErr
	if !errors.As(runErr, &authErr) {
		if result.StderrLog != "" {
			stderrLog = result.StderrLog
		}
		stderr, _ := readLogTail(stderrLog, 64<<10)
		classified = cli.ClassifyFailure(cliName, runErr, stderr)
		_ = errors.As(classified, &authErr)
	}
	if authErr == nil {
		return cycle.fail(currentRun, classified)
	}

	if err := currentRun.State.PauseForAuth(
		stringPointer(taskID),
		fmt.Sprintf(
			"%s authentication failed. Repair credentials outside Coterix, then resume without a response.",
			authErr.CLI,
		),
	); err != nil {
		return cycle.fail(currentRun, errors.Join(classified, err))
	}
	if err := currentRun.SaveState(); err != nil {
		return errors.Join(classified, err)
	}
	return nil
}

func (cycle *TaskCycle) fail(currentRun *Run, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("pipeline: task implementation failed")
	}
	if currentRun == nil || currentRun.State == nil {
		return cause
	}
	if currentRun.State.Phase != state.PhaseFailed {
		if err := currentRun.State.Fail(cause.Error()); err != nil {
			return errors.Join(cause, err)
		}
	}
	if err := currentRun.SaveState(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func recentCommitHistory(ctx context.Context, repoRoot string) (string, error) {
	command := exec.CommandContext(
		nonNilContext(ctx),
		"git",
		"log",
		"--oneline",
		"-n",
		"10",
	)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("pipeline: read recent commit history: %s", message)
	}
	history := strings.TrimSpace(string(output))
	if history == "" {
		return "", fmt.Errorf("pipeline: recent commit history is empty")
	}
	return history, nil
}
