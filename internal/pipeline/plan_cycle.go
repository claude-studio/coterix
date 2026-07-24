package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/prompt"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const (
	planFileName      = "plan.md"
	nextPlanFileName  = "plan.next.md"
	questionsFileName = "questions.md"
	planReviewFile    = "plan-review.json"
)

// PlanExecutor is the subprocess boundary used by the plan cycle.
type PlanExecutor interface {
	Run(context.Context, runner.RunRequest) (runner.RunResult, error)
}

// PlanCycle drives planning until it either needs human input, fails, or
// reaches awaiting_approval.
type PlanCycle struct {
	Executor PlanExecutor
	OnLine   func(runner.Line)
	observer Observer
}

// NewPlanCycle constructs a plan cycle around a subprocess executor.
func NewPlanCycle(executor PlanExecutor) *PlanCycle {
	return &PlanCycle{Executor: executor}
}

// Run advances one planning run to a terminal boundary. Feedback is used by
// the next revision, and override is the optional one-shot token returned by a
// human plan-cap resume or plan rejection.
func (cycle *PlanCycle) Run(
	ctx context.Context,
	currentRun *Run,
	feedback string,
	override *state.OneShotOverride,
) error {
	if cycle == nil || cycle.Executor == nil {
		return fmt.Errorf("pipeline: a plan executor is required")
	}
	if currentRun == nil || currentRun.State == nil || currentRun.adapter == nil {
		return fmt.Errorf("pipeline: an initialized run is required")
	}
	if err := currentRun.State.Validate(); err != nil {
		return fmt.Errorf("pipeline: validate run state: %w", err)
	}
	if currentRun.State.Phase != state.PhasePlanning {
		return fmt.Errorf(
			"pipeline: plan cycle requires planning phase, got %s",
			currentRun.State.Phase,
		)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	planPath := filepath.Join(currentRun.Dir, planFileName)
	reviewReady := false
	if currentRun.State.PlanHash != nil && strings.TrimSpace(feedback) == "" {
		if err := requirePlanHash(planPath, *currentRun.State.PlanHash); err != nil {
			return cycle.fail(currentRun, err)
		}
		reviewReady = true
	}

	for {
		if !reviewReady {
			if err := currentRun.State.BeginPlanRound(
				currentRun.Config.MaxPlanRounds,
				override,
			); err != nil {
				var capErr *state.CapError
				if !errors.As(err, &capErr) {
					return cycle.fail(currentRun, err)
				}
				if pauseErr := currentRun.State.PauseForPlanCap(
					fmt.Sprintf(
						"Plan round cap reached (%d >= %d). Respond to continue one more round.",
						capErr.Current,
						capErr.Maximum,
					),
				); pauseErr != nil {
					return cycle.fail(currentRun, pauseErr)
				}
				return currentRun.SaveState()
			}
			override = nil
			if err := currentRun.SaveState(); err != nil {
				return cycle.fail(currentRun, err)
			}

			stepResult, role, result, err := cycle.runPlanner(
				ctx,
				currentRun,
				feedback,
			)
			if err != nil {
				return cycle.handleStepFailure(currentRun, role, result, err)
			}
			switch result := stepResult.(type) {
			case cli.Questions:
				if err := currentRun.State.PauseForPlanQuestion(
					string(result.Content),
				); err != nil {
					return cycle.fail(currentRun, err)
				}
				return currentRun.SaveState()
			case cli.PlanFile:
				if err := adoptPlan(currentRun, result); err != nil {
					return cycle.fail(currentRun, err)
				}
			default:
				return cycle.fail(
					currentRun,
					fmt.Errorf("pipeline: planner returned no validated result"),
				)
			}
			feedback = ""
		}

		targetHash := *currentRun.State.PlanHash
		verdict, result, err := cycle.runPlanReview(
			ctx,
			currentRun,
			targetHash,
		)
		if hashErr := requirePlanHash(planPath, targetHash); hashErr != nil {
			return cycle.fail(currentRun, hashErr)
		}
		if err != nil {
			return cycle.handleStepFailure(
				currentRun,
				cli.RolePlanReviewer,
				result,
				err,
			)
		}

		if verdict.Clean {
			return cycle.acceptCleanPlan(currentRun, targetHash)
		}

		// Only a fully validated and hash-bound dirty verdict reaches this
		// branch. Malformed or mismatched verdicts fail inside the review
		// validator after their bounded read-only retries are exhausted.
		feedback = formatPlanFindings(verdict.Findings)
		reviewReady = false
	}
}

func (cycle *PlanCycle) runPlanner(
	ctx context.Context,
	currentRun *Run,
	feedback string,
) (cli.StepResult, cli.Role, runner.RunResult, error) {
	planPath := filepath.Join(currentRun.Dir, planFileName)
	nextPlanPath := filepath.Join(currentRun.Dir, nextPlanFileName)
	questionsPath := filepath.Join(currentRun.Dir, questionsFileName)

	role := cli.RolePlanWriter
	hasCanonicalPlan := currentRun.State.PlanHash != nil
	if hasCanonicalPlan {
		if err := requirePlanHash(planPath, *currentRun.State.PlanHash); err != nil {
			return nil, role, runner.RunResult{}, err
		}
		role = cli.RolePlanReviser
	}

	values := prompt.Values{
		"REQUEST":           currentRun.Request,
		"CURRENT_PLAN_PATH": "",
		"PLAN_OUTPUT_PATH":  nextPlanPath,
		"QUESTIONS_PATH":    questionsPath,
	}
	if hasCanonicalPlan {
		values["CURRENT_PLAN_PATH"] = planPath
		values["FEEDBACK"] = feedback
	} else if strings.TrimSpace(feedback) != "" {
		// A planner may ask before any canonical plan exists. Keep the
		// immutable request unchanged while including the human clarification
		// in the next initial draft.
		values["REQUEST"] = currentRun.Request +
			"\n\nHuman clarification:\n" + feedback
	}
	rendered, err := prompt.Render(prompt.PlanTemplate, values)
	if err != nil {
		return nil, role, runner.RunResult{}, err
	}

	paths := cli.ResultPaths{
		PlanOutput: nextPlanPath,
		Questions:  questionsPath,
	}
	if role == cli.RolePlanReviser {
		paths.CurrentPlan = planPath
	}
	policy, err := currentRun.adapter.PolicyForRole(role, paths)
	if err != nil {
		return nil, role, runner.RunResult{}, err
	}

	var validated cli.StepResult
	request, cliName, err := cycle.agentRequest(
		currentRun,
		role,
		rendered,
		policy,
	)
	if err != nil {
		return nil, role, runner.RunResult{}, err
	}
	request.ValidateResult = func(
		_ context.Context,
		_ runner.RunResult,
	) error {
		attempt := currentRun.adapter.NewAttempt()
		result, validateErr := attempt.ValidatePlannerResult(
			role,
			paths,
			ValidatePlan,
		)
		if validateErr != nil {
			return validateErr
		}
		validated = result
		return nil
	}

	finish := observeStep(
		cycle.observer,
		currentRun,
		StepPlan,
		string(role),
		cliName,
		&request,
	)
	result, runErr := cycle.Executor.Run(ctx, request)
	finish(result, runErr)
	if runErr != nil {
		return nil, role, result, runErr
	}
	if validated == nil {
		return nil, role, result, fmt.Errorf(
			"pipeline: %s completed without validating its designated result",
			cliName,
		)
	}
	return validated, role, result, nil
}

func (cycle *PlanCycle) runPlanReview(
	ctx context.Context,
	currentRun *Run,
	targetHash string,
) (cli.ReviewVerdict, runner.RunResult, error) {
	planPath := filepath.Join(currentRun.Dir, planFileName)
	reviewPath := filepath.Join(currentRun.Dir, planReviewFile)
	if err := requirePlanHash(planPath, targetHash); err != nil {
		return cli.ReviewVerdict{}, runner.RunResult{}, err
	}

	rendered, err := prompt.Render(prompt.PlanReviewTemplate, prompt.Values{
		"PLAN_PATH":   planPath,
		"PLAN_HASH":   targetHash,
		"REQUEST":     currentRun.Request,
		"REVIEW_PATH": reviewPath,
	})
	if err != nil {
		return cli.ReviewVerdict{}, runner.RunResult{}, err
	}
	policy, err := currentRun.adapter.PolicyForRole(
		cli.RolePlanReviewer,
		cli.ResultPaths{Review: reviewPath},
	)
	if err != nil {
		return cli.ReviewVerdict{}, runner.RunResult{}, err
	}
	request, cliName, err := cycle.agentRequest(
		currentRun,
		cli.RolePlanReviewer,
		rendered,
		policy,
	)
	if err != nil {
		return cli.ReviewVerdict{}, runner.RunResult{}, err
	}
	// This path is also checked explicitly before and after the logical review.
	// Supplying it here lets executors that protect read-only canonical inputs
	// enforce the same invariant around individual retry attempts.
	request.CanonicalPaths = []string{planPath}

	var validated *cli.ReviewJSON
	request.ValidateResult = func(
		_ context.Context,
		_ runner.RunResult,
	) error {
		if err := requirePlanHash(planPath, targetHash); err != nil {
			return err
		}
		attempt := currentRun.adapter.NewAttempt()
		review, validateErr := attempt.ValidateReviewResult(
			cli.RolePlanReviewer,
			reviewPath,
		)
		if validateErr != nil {
			return validateErr
		}
		if review.Verdict.TargetPlanHash != targetHash {
			return fmt.Errorf(
				"pipeline: review target_plan_hash %q does not match %q",
				review.Verdict.TargetPlanHash,
				targetHash,
			)
		}
		if err := requirePlanHash(planPath, targetHash); err != nil {
			return err
		}
		validated = &review
		return nil
	}

	finish := observeStep(
		cycle.observer,
		currentRun,
		StepPlanReview,
		string(cli.RolePlanReviewer),
		cliName,
		&request,
	)
	result, runErr := cycle.Executor.Run(ctx, request)
	finish(result, runErr)
	if hashErr := requirePlanHash(planPath, targetHash); hashErr != nil {
		return cli.ReviewVerdict{}, result, hashErr
	}
	if runErr != nil {
		return cli.ReviewVerdict{}, result, runErr
	}
	if validated == nil {
		return cli.ReviewVerdict{}, result, fmt.Errorf(
			"pipeline: plan reviewer completed without validating its designated result",
		)
	}
	return validated.Verdict, result, nil
}

func (cycle *PlanCycle) agentRequest(
	currentRun *Run,
	role cli.Role,
	rendered string,
	policy cli.OutputPolicy,
) (runner.RunRequest, string, error) {
	config, err := currentRun.Config.CLIForRole(role)
	if err != nil {
		return runner.RunRequest{}, "", err
	}
	cliName, exists := currentRun.Config.Roles[role]
	if !exists {
		return runner.RunRequest{}, "", fmt.Errorf(
			"pipeline: role %q has no CLI mapping",
			role,
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
		"plan-round-%d-%s-%s",
		currentRun.State.PlanRound,
		role,
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

func adoptPlan(currentRun *Run, result cli.PlanFile) error {
	nextPlanPath := filepath.Join(currentRun.Dir, nextPlanFileName)
	planPath := filepath.Join(currentRun.Dir, planFileName)
	if result.Path != nextPlanPath {
		return fmt.Errorf(
			"pipeline: planner result path %q does not match %q",
			result.Path,
			nextPlanPath,
		)
	}
	if _, err := ParsePlan(result.Content); err != nil {
		return fmt.Errorf("pipeline: revalidate adopted plan: %w", err)
	}
	expectedHash := hashBytes(result.Content)
	if err := os.Rename(nextPlanPath, planPath); err != nil {
		return fmt.Errorf("pipeline: adopt validated plan: %w", err)
	}
	if err := requirePlanHash(planPath, expectedHash); err != nil {
		return err
	}

	currentRun.State.PlanHash = stringPointer(expectedHash)
	if err := currentRun.SaveState(); err != nil {
		return fmt.Errorf("pipeline: persist adopted plan hash: %w", err)
	}
	return nil
}

func (cycle *PlanCycle) acceptCleanPlan(
	currentRun *Run,
	targetHash string,
) error {
	planPath := filepath.Join(currentRun.Dir, planFileName)
	if err := requirePlanHash(planPath, targetHash); err != nil {
		return cycle.fail(currentRun, err)
	}
	content, err := readRegularFile(planPath)
	if err != nil {
		return cycle.fail(currentRun, err)
	}
	parsed, err := ParsePlan(content)
	if err != nil {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: parse clean plan: %w", err),
		)
	}
	if hashBytes(content) != targetHash {
		return cycle.fail(
			currentRun,
			fmt.Errorf("pipeline: plan changed while materializing tasks"),
		)
	}

	taskOrder := make([]string, 0, len(parsed.Tasks))
	tasks := make(map[string]*state.TaskState, len(parsed.Tasks))
	for _, task := range parsed.Tasks {
		taskOrder = append(taskOrder, task.ID)
		tasks[task.ID] = &state.TaskState{Status: state.TaskOpen}
	}
	currentRun.State.TaskOrder = taskOrder
	currentRun.State.Tasks = tasks
	currentRun.State.CurrentTaskID = nil
	if err := currentRun.State.TransitionPhase(
		state.PhaseAwaitingApproval,
	); err != nil {
		return cycle.fail(currentRun, err)
	}
	if err := currentRun.SaveState(); err != nil {
		return err
	}
	return nil
}

func (cycle *PlanCycle) handleStepFailure(
	currentRun *Run,
	role cli.Role,
	result runner.RunResult,
	runErr error,
) error {
	var authErr *cli.AuthFailure
	classified := runErr
	if !errors.As(runErr, &authErr) {
		cliName := currentRun.Config.Roles[role]
		stderr, _ := readLogTail(result.StderrLog, 64<<10)
		classified = cli.ClassifyFailure(cliName, runErr, stderr)
		_ = errors.As(classified, &authErr)
	}
	if authErr != nil {
		if err := currentRun.State.PauseForAuth(
			nil,
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
	return cycle.fail(currentRun, classified)
}

func (cycle *PlanCycle) fail(currentRun *Run, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("pipeline: planning failed")
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

func requirePlanHash(path, expected string) error {
	content, err := readRegularFile(path)
	if err != nil {
		return err
	}
	actual := hashBytes(content)
	if actual != expected {
		return fmt.Errorf(
			"pipeline: sha256(%s) = %s, want %s",
			filepath.Base(path),
			actual,
			expected,
		)
	}
	return nil
}

func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum)
}

func formatPlanFindings(findings []cli.ReviewFinding) string {
	var output strings.Builder
	output.WriteString("Address every plan review finding:\n")
	for _, finding := range findings {
		fmt.Fprintf(
			&output,
			"- [%s] %s",
			finding.Severity,
			finding.ID,
		)
		if finding.TaskID != nil {
			fmt.Fprintf(&output, " (task %s)", *finding.TaskID)
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

func readLogTail(path string, maximum int64) ([]byte, error) {
	if path == "" || maximum <= 0 {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - maximum
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, maximum))
}

func randomLogSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("pipeline: generate log suffix: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func stringPointer(value string) *string {
	return &value
}
