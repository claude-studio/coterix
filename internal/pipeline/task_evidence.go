package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/prompt"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const (
	tasksDirectoryName = "tasks"
	gateEvidenceName   = "gate.json"
	reviewEvidenceName = "review.json"
)

type gateEvidence struct {
	Command      []string `json:"command"`
	CWD          string   `json:"cwd"`
	CandidateSHA string   `json:"candidate_sha"`
	Exit         int      `json:"exit"`
	TimedOut     bool     `json:"timed_out"`
	StdoutLog    string   `json:"stdout_log"`
	StderrLog    string   `json:"stderr_log"`
}

type gateEvidenceWire struct {
	Command      *[]string `json:"command"`
	CWD          *string   `json:"cwd"`
	CandidateSHA *string   `json:"candidate_sha"`
	Exit         *int      `json:"exit"`
	TimedOut     *bool     `json:"timed_out"`
	StdoutLog    *string   `json:"stdout_log"`
	StderrLog    *string   `json:"stderr_log"`
}

type repairEvidence struct {
	Gate     gateEvidence
	Findings []cli.ReviewFinding
}

func (cycle *TaskCycle) assessCandidate(
	ctx context.Context,
	currentRun *Run,
	task *PlanTask,
) error {
	taskState := currentRun.State.Tasks[task.ID]
	if taskState.CandidateSHA == nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: candidate task %s has no candidate_sha", task.ID),
		)
	}
	candidateSHA := *taskState.CandidateSHA

	if err := cycle.clearTaskEvidence(currentRun, task.ID); err != nil {
		return cycle.fail(currentRun, err)
	}
	if err := currentRun.SaveState(); err != nil {
		return cycle.fail(currentRun, err)
	}

	gate, gateFailed, err := cycle.runGate(
		ctx,
		currentRun,
		task.ID,
		candidateSHA,
	)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	taskState.GateResult = stringPointer(taskEvidenceRelativePath(
		task.ID,
		gateEvidenceName,
	))
	if gateFailed {
		if err := currentRun.State.TransitionTask(
			task.ID,
			state.TaskRepairing,
		); err != nil {
			return cycle.fail(currentRun, err)
		}
		if err := currentRun.SaveState(); err != nil {
			return cycle.fail(currentRun, err)
		}
		return nil
	}
	if err := currentRun.SaveState(); err != nil {
		return cycle.fail(currentRun, err)
	}

	review, result, err := cycle.runImplementationReview(
		ctx,
		currentRun,
		task,
		candidateSHA,
	)
	if err != nil {
		return cycle.handleReviewFailure(
			currentRun,
			task.ID,
			result,
			err,
		)
	}
	taskState.ReviewResult = stringPointer(taskEvidenceRelativePath(
		task.ID,
		reviewEvidenceName,
	))
	if !review.Verdict.Clean {
		if err := currentRun.State.TransitionTask(
			task.ID,
			state.TaskRepairing,
		); err != nil {
			return cycle.fail(currentRun, err)
		}
		if err := currentRun.SaveState(); err != nil {
			return cycle.fail(currentRun, err)
		}
		return nil
	}
	if err := currentRun.SaveState(); err != nil {
		return cycle.fail(currentRun, err)
	}
	return cycle.confirmTask(currentRun, task.ID, gate, review)
}

func (cycle *TaskCycle) runGate(
	ctx context.Context,
	currentRun *Run,
	taskID string,
	candidateSHA string,
) (gateEvidence, bool, error) {
	if err := cycle.verifyCandidateBoundary(
		ctx,
		currentRun,
		taskID,
		candidateSHA,
	); err != nil {
		return gateEvidence{}, false, fmt.Errorf(
			"pipeline: gate precondition failed: %w",
			err,
		)
	}
	taskDir, err := ensureTaskEvidenceDirectory(currentRun, taskID)
	if err != nil {
		return gateEvidence{}, false, err
	}
	gatePath := filepath.Join(taskDir, gateEvidenceName)
	if err := removeEvidenceFile(gatePath); err != nil {
		return gateEvidence{}, false, err
	}

	logSuffix, err := randomLogSuffix()
	if err != nil {
		return gateEvidence{}, false, err
	}
	logPrefix := fmt.Sprintf("task-%s-%s-gate-%s", taskID, candidateSHA, logSuffix)
	request := runner.RunRequest{
		Command:     currentRun.Config.GateCommand[0],
		Args:        append([]string(nil), currentRun.Config.GateCommand[1:]...),
		Dir:         filepath.Join(currentRun.RepoRoot, currentRun.Config.GateCWD),
		IdleTimeout: currentRun.Config.IdleTimeout(),
		MaxRetries:  0,
		Effect:      runner.EffectReadOnly,
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
	}
	result, runErr := cycle.Executor.Run(ctx, request)
	if result.StdoutLog == "" {
		result.StdoutLog = request.StdoutLog
	}
	if result.StderrLog == "" {
		result.StderrLog = request.StderrLog
	}
	stdoutLog, err := runRelativeRegularFile(currentRun, result.StdoutLog)
	if err != nil {
		return gateEvidence{}, false, fmt.Errorf(
			"pipeline: validate gate stdout log: %w",
			err,
		)
	}
	stderrLog, err := runRelativeRegularFile(currentRun, result.StderrLog)
	if err != nil {
		return gateEvidence{}, false, fmt.Errorf(
			"pipeline: validate gate stderr log: %w",
			err,
		)
	}
	evidence := gateEvidence{
		Command:      append([]string(nil), currentRun.Config.GateCommand...),
		CWD:          currentRun.Config.GateCWD,
		CandidateSHA: candidateSHA,
		Exit:         result.Exit,
		TimedOut:     result.TimedOut,
		StdoutLog:    stdoutLog,
		StderrLog:    stderrLog,
	}
	if err := writeGateEvidence(gatePath, evidence); err != nil {
		return gateEvidence{}, false, err
	}

	verifyContext := context.WithoutCancel(nonNilContext(ctx))
	if err := cycle.verifyCandidateBoundary(
		verifyContext,
		currentRun,
		taskID,
		candidateSHA,
	); err != nil {
		return gateEvidence{}, false, fmt.Errorf(
			"pipeline: gate postcondition failed: %w",
			err,
		)
	}
	if err := validateGateEvidence(
		currentRun,
		taskID,
		candidateSHA,
		evidence,
		false,
	); err != nil {
		return gateEvidence{}, false, err
	}

	var interrupted *runner.InterruptedError
	if ctx.Err() != nil || errors.As(runErr, &interrupted) {
		if runErr != nil {
			return gateEvidence{}, false, runErr
		}
		return gateEvidence{}, false, ctx.Err()
	}
	failed := runErr != nil || evidence.Exit != 0 || evidence.TimedOut
	return evidence, failed, nil
}

func (cycle *TaskCycle) runImplementationReview(
	ctx context.Context,
	currentRun *Run,
	task *PlanTask,
	candidateSHA string,
) (cli.ReviewJSON, runner.RunResult, error) {
	taskDir, err := ensureTaskEvidenceDirectory(currentRun, task.ID)
	if err != nil {
		return cli.ReviewJSON{}, runner.RunResult{}, err
	}
	planPath := filepath.Join(currentRun.Dir, planFileName)
	reviewPath := filepath.Join(taskDir, reviewEvidenceName)
	rendered, err := prompt.Render(
		prompt.ImplReviewTemplate,
		prompt.Values{
			"PLAN_PATH":     planPath,
			"PLAN_HASH":     *currentRun.State.ApprovedPlanHash,
			"TASK_ID":       task.ID,
			"BASE_SHA":      *currentRun.State.Tasks[task.ID].BaseSHA,
			"CANDIDATE_SHA": candidateSHA,
			"REVIEW_PATH":   reviewPath,
		},
	)
	if err != nil {
		return cli.ReviewJSON{}, runner.RunResult{}, err
	}
	policy, err := currentRun.adapter.PolicyForRole(
		cli.RoleImplReviewer,
		cli.ResultPaths{Review: reviewPath},
	)
	if err != nil {
		return cli.ReviewJSON{}, runner.RunResult{}, err
	}
	request, _, err := cycle.reviewRequest(
		currentRun,
		task.ID,
		rendered,
		policy,
	)
	if err != nil {
		return cli.ReviewJSON{}, runner.RunResult{}, err
	}
	request.CanonicalPaths = append(
		request.CanonicalPaths,
		planPath,
		filepath.Join(taskDir, gateEvidenceName),
	)
	request.PrepareAttempt = func(attemptContext context.Context, _ int) error {
		return cycle.verifyCandidateBoundary(
			attemptContext,
			currentRun,
			task.ID,
			candidateSHA,
		)
	}

	var validated *cli.ReviewJSON
	request.ValidateResult = func(
		validateContext context.Context,
		_ runner.RunResult,
	) error {
		if err := cycle.verifyCandidateBoundary(
			validateContext,
			currentRun,
			task.ID,
			candidateSHA,
		); err != nil {
			return err
		}
		attempt := currentRun.adapter.NewAttempt()
		review, validateErr := attempt.ValidateReviewResult(
			cli.RoleImplReviewer,
			reviewPath,
		)
		if validateErr != nil {
			return validateErr
		}
		if err := validateImplementationReviewTargets(
			review,
			*currentRun.State.ApprovedPlanHash,
			task.ID,
			candidateSHA,
		); err != nil {
			return err
		}
		if err := cycle.verifyCandidateBoundary(
			validateContext,
			currentRun,
			task.ID,
			candidateSHA,
		); err != nil {
			return err
		}
		validated = &review
		return nil
	}

	result, runErr := cycle.Executor.Run(ctx, request)
	verifyContext := context.WithoutCancel(nonNilContext(ctx))
	if err := cycle.verifyCandidateBoundary(
		verifyContext,
		currentRun,
		task.ID,
		candidateSHA,
	); err != nil {
		return cli.ReviewJSON{}, result, fmt.Errorf(
			"pipeline: implementation review postcondition failed: %w",
			err,
		)
	}
	if runErr != nil {
		return cli.ReviewJSON{}, result, runErr
	}
	if validated == nil {
		return cli.ReviewJSON{}, result, fmt.Errorf(
			"pipeline: implementation reviewer completed without validating its designated result",
		)
	}
	if err := replaceReviewEvidence(reviewPath, validated.Content); err != nil {
		return cli.ReviewJSON{}, result, err
	}
	return *validated, result, nil
}

func (cycle *TaskCycle) reviewRequest(
	currentRun *Run,
	taskID string,
	rendered string,
	policy cli.OutputPolicy,
) (runner.RunRequest, string, error) {
	config, err := currentRun.Config.CLIForRole(cli.RoleImplReviewer)
	if err != nil {
		return runner.RunRequest{}, "", err
	}
	cliName, exists := currentRun.Config.Roles[cli.RoleImplReviewer]
	if !exists {
		return runner.RunRequest{}, "", fmt.Errorf(
			"pipeline: role %q has no CLI mapping",
			cli.RoleImplReviewer,
		)
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
		currentRun.State.Tasks[taskID].Attempt,
		cli.RoleImplReviewer,
		logSuffix,
	)
	return runner.RunRequest{
		Command:        config.Command,
		Args:           args,
		Dir:            currentRun.RepoRoot,
		Env:            config.Env,
		Stdin:          stdin,
		IdleTimeout:    currentRun.Config.IdleTimeout(),
		MaxRetries:     currentRun.Config.MaxRetries,
		Effect:         policy.Effect,
		StdoutLog:      filepath.Join(currentRun.Dir, "logs", logPrefix+".stdout.log"),
		StderrLog:      filepath.Join(currentRun.Dir, "logs", logPrefix+".stderr.log"),
		OutputPaths:    policy.OutputPaths,
		CanonicalPaths: policy.CanonicalPaths,
		OnLine:         cycle.OnLine,
	}, cliName, nil
}

func (cycle *TaskCycle) handleReviewFailure(
	currentRun *Run,
	taskID string,
	result runner.RunResult,
	runErr error,
) error {
	var authErr *cli.AuthFailure
	classified := runErr
	if !errors.As(runErr, &authErr) {
		cliName := currentRun.Config.Roles[cli.RoleImplReviewer]
		stderr, _ := readLogTail(result.StderrLog, 64<<10)
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

func (cycle *TaskCycle) repairTask(
	ctx context.Context,
	currentRun *Run,
	task *PlanTask,
	override *state.OneShotOverride,
) error {
	taskState := currentRun.State.Tasks[task.ID]
	if taskState.CandidateSHA == nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: repairing task %s has no candidate_sha", task.ID),
		)
	}
	oldCandidate := *taskState.CandidateSHA
	repair, err := cycle.loadRepairEvidence(currentRun, task.ID, oldCandidate)
	if err != nil {
		return cycle.fail(currentRun, err)
	}

	guard := runner.GitMutationGuard{}
	start, err := guard.Capture(ctx, currentRun.RepoRoot)
	if err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: fixer precondition failed: %w", err),
		)
	}
	if start.Head != oldCandidate {
		return cycle.fail(
			currentRun,
			fmt.Errorf(
				"pipeline: fixer HEAD %s does not match candidate_sha %s",
				start.Head,
				oldCandidate,
			),
		)
	}
	previousAttempt := taskState.Attempt
	if err := currentRun.State.BeginTaskAttempt(
		task.ID,
		currentRun.Config.MaxTaskAttempts,
		override,
	); err != nil {
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
		taskState.Attempt = previousAttempt
		return cycle.fail(currentRun, err)
	}
	if err := VerifyApprovedPlan(currentRun); err != nil {
		return cycle.fail(currentRun, err)
	}
	if err := guard.Verify(ctx, currentRun.RepoRoot, start); err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: fixer snapshot changed before start: %w", err),
		)
	}

	rendered, err := prompt.Render(
		prompt.FixTemplate,
		prompt.Values{
			"PLAN_PATH":     filepath.Join(currentRun.Dir, planFileName),
			"TASK_ID":       task.ID,
			"CANDIDATE_SHA": oldCandidate,
			"FINDINGS":      formatImplementationFindings(repair.Findings),
			"GATE_OUTPUT":   formatGateFailure(currentRun, repair.Gate),
		},
	)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	request, cliName, err := cycle.fixerRequest(
		currentRun,
		task.ID,
		rendered,
	)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	result, runErr := cycle.Executor.Run(ctx, request)
	if runErr != nil {
		return cycle.handleMutationFailure(
			ctx,
			currentRun,
			task.ID,
			cliName,
			"fixer",
			start,
			request.StderrLog,
			result,
			runErr,
		)
	}
	candidate, err := cli.ValidateCommittedCandidate(
		ctx,
		cli.RoleFixer,
		currentRun.RepoRoot,
		oldCandidate,
	)
	if err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: fixer postcondition failed: %w", err),
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
			fmt.Errorf("pipeline: repaired candidate changed before it could be recorded: %w", err),
		)
	}
	if err := cycle.clearTaskEvidence(currentRun, task.ID); err != nil {
		return cycle.fail(currentRun, err)
	}
	taskState.CandidateSHA = stringPointer(candidate.CandidateSHA)
	if err := currentRun.State.TransitionTask(
		task.ID,
		state.TaskCandidate,
	); err != nil {
		taskState.CandidateSHA = stringPointer(oldCandidate)
		return cycle.fail(currentRun, err)
	}
	if err := currentRun.SaveState(); err != nil {
		return cycle.fail(currentRun, err)
	}
	return nil
}

func (cycle *TaskCycle) fixerRequest(
	currentRun *Run,
	taskID string,
	rendered string,
) (runner.RunRequest, string, error) {
	config, err := currentRun.Config.CLIForRole(cli.RoleFixer)
	if err != nil {
		return runner.RunRequest{}, "", err
	}
	cliName, exists := currentRun.Config.Roles[cli.RoleImplWriter]
	if !exists {
		return runner.RunRequest{}, "", fmt.Errorf(
			"pipeline: writer route %q has no CLI mapping",
			cli.RoleImplWriter,
		)
	}
	policy, err := currentRun.adapter.PolicyForRole(
		cli.RoleFixer,
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
		currentRun.State.Tasks[taskID].Attempt,
		cli.RoleFixer,
		logSuffix,
	)
	return runner.RunRequest{
		Command:     config.Command,
		Args:        args,
		Dir:         currentRun.RepoRoot,
		Env:         config.Env,
		Stdin:       stdin,
		IdleTimeout: currentRun.Config.IdleTimeout(),
		MaxRetries:  0,
		Effect:      policy.Effect,
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

func (cycle *TaskCycle) loadRepairEvidence(
	currentRun *Run,
	taskID string,
	candidateSHA string,
) (repairEvidence, error) {
	taskState := currentRun.State.Tasks[taskID]
	gateRelative := taskEvidenceRelativePath(taskID, gateEvidenceName)
	if taskState.GateResult == nil || *taskState.GateResult != gateRelative {
		return repairEvidence{}, fmt.Errorf(
			"pipeline: repairing task %s lacks its exact gate evidence link",
			taskID,
		)
	}
	gatePath := filepath.Join(currentRun.Dir, gateRelative)
	gate, err := readGateEvidence(gatePath)
	if err != nil {
		return repairEvidence{}, err
	}
	if err := validateGateEvidence(
		currentRun,
		taskID,
		candidateSHA,
		gate,
		false,
	); err != nil {
		return repairEvidence{}, err
	}
	if gate.Exit != 0 || gate.TimedOut {
		if taskState.ReviewResult != nil {
			return repairEvidence{}, fmt.Errorf(
				"pipeline: failed gate for task %s must not carry review evidence",
				taskID,
			)
		}
		return repairEvidence{Gate: gate}, nil
	}

	reviewRelative := taskEvidenceRelativePath(taskID, reviewEvidenceName)
	if taskState.ReviewResult == nil ||
		*taskState.ReviewResult != reviewRelative {
		return repairEvidence{}, fmt.Errorf(
			"pipeline: repairing task %s lacks its exact review evidence link",
			taskID,
		)
	}
	reviewPath := filepath.Join(currentRun.Dir, reviewRelative)
	attempt := currentRun.adapter.NewAttempt()
	review, err := attempt.ValidateReviewResult(
		cli.RoleImplReviewer,
		reviewPath,
	)
	if err != nil {
		return repairEvidence{}, err
	}
	if err := validateImplementationReviewTargets(
		review,
		*currentRun.State.ApprovedPlanHash,
		taskID,
		candidateSHA,
	); err != nil {
		return repairEvidence{}, err
	}
	if review.Verdict.Clean {
		return repairEvidence{}, fmt.Errorf(
			"pipeline: repairing task %s has a clean review",
			taskID,
		)
	}
	return repairEvidence{
		Gate:     gate,
		Findings: review.Verdict.Findings,
	}, nil
}

func (cycle *TaskCycle) confirmTask(
	currentRun *Run,
	taskID string,
	gate gateEvidence,
	review cli.ReviewJSON,
) error {
	taskState := currentRun.State.Tasks[taskID]
	if taskState.BaseSHA == nil || taskState.CandidateSHA == nil ||
		*taskState.BaseSHA == *taskState.CandidateSHA {
		return cycle.fail(
			currentRun,
			fmt.Errorf(
				"pipeline: task %s cannot be confirmed without distinct base and candidate SHAs",
				taskID,
			),
		)
	}
	candidateSHA := *taskState.CandidateSHA
	if err := cycle.verifyCandidateBoundary(
		context.Background(),
		currentRun,
		taskID,
		candidateSHA,
	); err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: confirmation boundary failed: %w", err),
		)
	}
	gateRelative := taskEvidenceRelativePath(taskID, gateEvidenceName)
	reviewRelative := taskEvidenceRelativePath(taskID, reviewEvidenceName)
	if taskState.GateResult == nil || *taskState.GateResult != gateRelative ||
		taskState.ReviewResult == nil || *taskState.ReviewResult != reviewRelative {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: task %s evidence links do not match fixed paths", taskID),
		)
	}
	if err := validateGateEvidence(
		currentRun,
		taskID,
		candidateSHA,
		gate,
		true,
	); err != nil {
		return cycle.fail(currentRun, err)
	}
	persistedGate, err := readGateEvidence(
		filepath.Join(currentRun.Dir, gateRelative),
	)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	if !equalGateEvidence(gate, persistedGate) {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: persisted gate evidence changed before confirmation"),
		)
	}
	if review.Path != filepath.Join(currentRun.Dir, reviewRelative) {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: review path %q does not match fixed task evidence path", review.Path),
		)
	}
	if err := validateImplementationReviewTargets(
		review,
		*currentRun.State.ApprovedPlanHash,
		taskID,
		candidateSHA,
	); err != nil {
		return cycle.fail(currentRun, err)
	}
	if !review.Verdict.Clean {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: task %s review is not clean", taskID),
		)
	}
	reviewInfo, err := os.Lstat(review.Path)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	if reviewInfo.Mode()&os.ModeSymlink != 0 ||
		!reviewInfo.Mode().IsRegular() ||
		reviewInfo.Size() != int64(len(review.Content)) {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: persisted review evidence changed before confirmation"),
		)
	}
	if err := currentRun.State.TransitionTask(
		taskID,
		state.TaskConfirmed,
	); err != nil {
		return cycle.fail(currentRun, err)
	}
	if err := currentRun.SaveState(); err != nil {
		return cycle.fail(currentRun, err)
	}
	return nil
}

func equalGateEvidence(left, right gateEvidence) bool {
	return slices.Equal(left.Command, right.Command) &&
		left.CWD == right.CWD &&
		left.CandidateSHA == right.CandidateSHA &&
		left.Exit == right.Exit &&
		left.TimedOut == right.TimedOut &&
		left.StdoutLog == right.StdoutLog &&
		left.StderrLog == right.StderrLog
}

func (cycle *TaskCycle) verifyCandidateBoundary(
	ctx context.Context,
	currentRun *Run,
	taskID string,
	candidateSHA string,
) error {
	if err := VerifyApprovedPlan(currentRun); err != nil {
		return err
	}
	taskState := currentRun.State.Tasks[taskID]
	if taskState == nil || taskState.BaseSHA == nil ||
		taskState.CandidateSHA == nil {
		return fmt.Errorf("task %s lacks base_sha or candidate_sha", taskID)
	}
	if *taskState.CandidateSHA != candidateSHA {
		return fmt.Errorf(
			"task %s candidate_sha changed from %s to %s",
			taskID,
			candidateSHA,
			*taskState.CandidateSHA,
		)
	}
	if *taskState.BaseSHA == candidateSHA {
		return fmt.Errorf("task %s candidate_sha equals base_sha", taskID)
	}
	return (runner.GitMutationGuard{}).Verify(
		nonNilContext(ctx),
		currentRun.RepoRoot,
		runner.MutationSnapshot{Head: candidateSHA},
	)
}

func (cycle *TaskCycle) clearTaskEvidence(
	currentRun *Run,
	taskID string,
) error {
	for _, name := range []string{gateEvidenceName, reviewEvidenceName} {
		path := filepath.Join(
			currentRun.Dir,
			taskEvidenceRelativePath(taskID, name),
		)
		if err := removeEvidenceFile(path); err != nil {
			return err
		}
	}
	taskState := currentRun.State.Tasks[taskID]
	taskState.GateResult = nil
	taskState.ReviewResult = nil
	return nil
}

func ensureTaskEvidenceDirectory(
	currentRun *Run,
	taskID string,
) (string, error) {
	tasksDir := filepath.Join(currentRun.Dir, tasksDirectoryName)
	if err := ensureRealDirectory(tasksDir, 0o700); err != nil {
		return "", err
	}
	taskDir := filepath.Join(tasksDir, taskID)
	if err := ensureRealDirectory(taskDir, 0o700); err != nil {
		return "", err
	}
	return taskDir, nil
}

func taskEvidenceRelativePath(taskID string, name string) string {
	return filepath.Join(tasksDirectoryName, taskID, name)
}

func removeEvidenceFile(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("pipeline: inspect task evidence %q: %w", path, err)
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		return fmt.Errorf("pipeline: task evidence is not a regular file: %q", path)
	default:
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("pipeline: remove stale task evidence %q: %w", path, err)
		}
		return nil
	}
}

func writeGateEvidence(path string, evidence gateEvidence) error {
	content, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return fmt.Errorf("pipeline: encode gate evidence: %w", err)
	}
	content = append(content, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("pipeline: create gate evidence %q: %w", path, err)
	}
	written, writeErr := file.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("pipeline: write gate evidence %q: %w", path, err)
	}
	return nil
}

func replaceReviewEvidence(path string, content []byte) error {
	if err := removeEvidenceFile(path); err != nil {
		return err
	}
	if err := writeExclusive(path, content, 0o600); err != nil {
		return fmt.Errorf("pipeline: persist validated review evidence: %w", err)
	}
	return nil
}

func readGateEvidence(path string) (gateEvidence, error) {
	content, err := readRegularFile(path)
	if err != nil {
		return gateEvidence{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var wire *gateEvidenceWire
	if err := decoder.Decode(&wire); err != nil {
		return gateEvidence{}, fmt.Errorf("pipeline: decode gate evidence: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return gateEvidence{}, fmt.Errorf("pipeline: decode gate evidence: %w", err)
	}
	if wire == nil || wire.Command == nil || wire.CWD == nil ||
		wire.CandidateSHA == nil || wire.Exit == nil || wire.TimedOut == nil ||
		wire.StdoutLog == nil || wire.StderrLog == nil {
		return gateEvidence{}, fmt.Errorf("pipeline: gate evidence is missing required fields")
	}
	return gateEvidence{
		Command:      append([]string(nil), (*wire.Command)...),
		CWD:          *wire.CWD,
		CandidateSHA: *wire.CandidateSHA,
		Exit:         *wire.Exit,
		TimedOut:     *wire.TimedOut,
		StdoutLog:    *wire.StdoutLog,
		StderrLog:    *wire.StderrLog,
	}, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values are not allowed")
	}
	return err
}

func validateGateEvidence(
	currentRun *Run,
	taskID string,
	candidateSHA string,
	evidence gateEvidence,
	requirePassing bool,
) error {
	switch {
	case !slices.Equal(evidence.Command, currentRun.Config.GateCommand):
		return fmt.Errorf("pipeline: task %s gate command does not match config snapshot", taskID)
	case evidence.CWD != currentRun.Config.GateCWD:
		return fmt.Errorf("pipeline: task %s gate cwd does not match config snapshot", taskID)
	case evidence.CandidateSHA != candidateSHA:
		return fmt.Errorf("pipeline: task %s gate candidate_sha does not match", taskID)
	case requirePassing && evidence.Exit != 0:
		return fmt.Errorf("pipeline: task %s gate exit is %d, want 0", taskID, evidence.Exit)
	case requirePassing && evidence.TimedOut:
		return fmt.Errorf("pipeline: task %s gate timed out", taskID)
	}
	for name, relative := range map[string]string{
		"stdout_log": evidence.StdoutLog,
		"stderr_log": evidence.StderrLog,
	} {
		if err := validateRunRelativePath(relative); err != nil {
			return fmt.Errorf("pipeline: validate gate %s: %w", name, err)
		}
		if _, err := runRelativeRegularFile(
			currentRun,
			filepath.Join(currentRun.Dir, relative),
		); err != nil {
			return fmt.Errorf("pipeline: validate gate %s: %w", name, err)
		}
	}
	return nil
}

func validateRunRelativePath(path string) error {
	cleaned := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || cleaned != path ||
		cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q is not a clean run-relative path", path)
	}
	return nil
}

func runRelativeRegularFile(currentRun *Run, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	relative, err := filepath.Rel(currentRun.Dir, absolute)
	if err != nil {
		return "", err
	}
	if relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside run directory", path)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("path %q is not a regular file", path)
	}
	return relative, nil
}

func validateImplementationReviewTargets(
	review cli.ReviewJSON,
	planHash string,
	taskID string,
	candidateSHA string,
) error {
	verdict := review.Verdict
	switch {
	case verdict.SchemaVersion != 1 ||
		verdict.Kind != cli.ReviewKindImplementation:
		return fmt.Errorf("pipeline: implementation review schema kind is invalid")
	case verdict.PlanHash != planHash:
		return fmt.Errorf(
			"pipeline: review plan_hash %q does not match %q",
			verdict.PlanHash,
			planHash,
		)
	case verdict.TaskID != taskID:
		return fmt.Errorf(
			"pipeline: review task_id %q does not match %q",
			verdict.TaskID,
			taskID,
		)
	case verdict.CandidateSHA != candidateSHA:
		return fmt.Errorf(
			"pipeline: review candidate_sha %q does not match %q",
			verdict.CandidateSHA,
			candidateSHA,
		)
	default:
		return nil
	}
}

func formatImplementationFindings(findings []cli.ReviewFinding) string {
	if len(findings) == 0 {
		return "None."
	}
	var output strings.Builder
	for _, finding := range findings {
		fmt.Fprintf(&output, "- [%s] %s", finding.Severity, finding.ID)
		if finding.Location != nil {
			fmt.Fprintf(&output, " (%s)", *finding.Location)
		}
		fmt.Fprintf(
			&output,
			": %s Requested change: %s\n",
			finding.Issue,
			finding.RequestedChange,
		)
	}
	return strings.TrimSpace(output.String())
}

func formatGateFailure(currentRun *Run, evidence gateEvidence) string {
	if evidence.Exit == 0 && !evidence.TimedOut {
		return ""
	}
	stdout, _ := readLogTail(
		filepath.Join(currentRun.Dir, evidence.StdoutLog),
		64<<10,
	)
	stderr, _ := readLogTail(
		filepath.Join(currentRun.Dir, evidence.StderrLog),
		64<<10,
	)
	return fmt.Sprintf(
		"command: %q\ncwd: %s\nexit: %d\ntimed_out: %t\nstdout:\n%s\nstderr:\n%s",
		evidence.Command,
		evidence.CWD,
		evidence.Exit,
		evidence.TimedOut,
		strings.TrimSpace(string(stdout)),
		strings.TrimSpace(string(stderr)),
	)
}
