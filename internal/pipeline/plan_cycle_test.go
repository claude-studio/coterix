package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	pipelineHelperEnvironment = "COTERIX_PIPELINE_HELPER"
	pipelineScenarioEnv       = "COTERIX_PIPELINE_SCENARIO"
	pipelineRunDirEnv         = "COTERIX_PIPELINE_RUN_DIR"
	pipelineCounterDirEnv     = "COTERIX_PIPELINE_COUNTER_DIR"
)

const draftPlan = `# Draft plan

## T2: Second task
- [ ] Implement the second task
Acceptance: The second task is complete
Verify: go test ./...

## T1: First task
- [ ] Implement the first task
Acceptance: The first task is complete
Verify: go test ./internal/pipeline/...
`

const revisedPlan = `# Revised plan

## T2: Second task
- [ ] Implement the reviewed second task
Acceptance: The reviewed second task is complete
Verify: go test ./...

## T3: Replacement task
- [ ] Implement the replacement task
Acceptance: The replacement task is complete
Verify: go test ./internal/pipeline/...
`

func TestPlanCycleHelperProcess(t *testing.T) {
	if os.Getenv(pipelineHelperEnvironment) != "1" {
		return
	}
	args := argumentsAfterDoubleDash(os.Args)
	if len(args) == 0 {
		pipelineHelperFail("missing helper role")
	}
	role := args[0]
	var rendered string
	if role == "claude" {
		if len(args) != 2 {
			pipelineHelperFail("claude helper requires a prompt argument")
		}
		rendered = args[1]
	} else if role == "codex" {
		content, err := io.ReadAll(os.Stdin)
		if err != nil {
			pipelineHelperFail(err.Error())
		}
		rendered = string(content)
	} else {
		pipelineHelperFail("unknown helper role: " + role)
	}

	runDir := os.Getenv(pipelineRunDirEnv)
	counterDir := os.Getenv(pipelineCounterDirEnv)
	scenario := os.Getenv(pipelineScenarioEnv)
	if runDir == "" || counterDir == "" || scenario == "" {
		pipelineHelperFail("helper environment is incomplete")
	}

	switch role {
	case "claude":
		if strings.Contains(rendered, "This is a REVISION.") {
			runPlanReviserHelper(runDir, counterDir, scenario, rendered)
			return
		}
		runPlanWriterHelper(runDir, counterDir, scenario, rendered)
	case "codex":
		runPlanReviewerHelper(runDir, counterDir, scenario, rendered)
	}
}

func TestPlanCycleCleanConvergesAndPersistsHash(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "clean", 2, 5)
	runPlanCycle(t, run)

	if run.State.Phase != state.PhaseAwaitingApproval {
		t.Fatalf("phase = %s, want awaiting_approval", run.State.Phase)
	}
	if run.State.PlanRound != 1 {
		t.Fatalf("plan_round = %d, want 1", run.State.PlanRound)
	}
	assertPersistedPlanHash(t, run)
	if got := strings.Join(run.State.TaskOrder, ","); got != "T2,T1" {
		t.Fatalf("task_order = %q, want document order T2,T1", got)
	}
	if len(run.State.Tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(run.State.Tasks))
	}
	for _, taskID := range run.State.TaskOrder {
		task := run.State.Tasks[taskID]
		if task == nil || task.Status != state.TaskOpen || task.Attempt != 0 {
			t.Fatalf("task %s = %#v, want fresh open state", taskID, task)
		}
	}
	if run.State.CurrentTaskID != nil {
		t.Fatalf("current_task_id = %q, want nil", *run.State.CurrentTaskID)
	}
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviewer", 1)
	assertHelperCount(t, counters, "reviser", 0)

	reloaded, err := OpenRun(run.RepoRoot, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State.PlanHash == nil ||
		*reloaded.State.PlanHash != *run.State.PlanHash ||
		reloaded.State.Phase != state.PhaseAwaitingApproval {
		t.Fatalf("persisted state = %#v", reloaded.State)
	}
}

func TestPlanCycleValidDirtyVerdictRevises(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "dirty", 1, 5)
	runPlanCycle(t, run)

	if run.State.Phase != state.PhaseAwaitingApproval ||
		run.State.PlanRound != 2 {
		t.Fatalf(
			"state = phase %s round %d, want awaiting_approval round 2",
			run.State.Phase,
			run.State.PlanRound,
		)
	}
	assertPersistedPlanHash(t, run)
	assertFileContent(t, filepath.Join(run.Dir, planFileName), revisedPlan)
	if got := strings.Join(run.State.TaskOrder, ","); got != "T2,T3" {
		t.Fatalf("task_order = %q, want rematerialized T2,T3", got)
	}
	if _, stale := run.State.Tasks["T1"]; stale {
		t.Fatal("clean revision retained a stale task")
	}
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviser", 1)
	assertHelperCount(t, counters, "reviewer", 2)
}

func TestPlanCycleReviserRetryPreservesCanonicalPlan(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "reviser-retry", 1, 5)
	runPlanCycle(t, run)

	if run.State.Phase != state.PhaseAwaitingApproval {
		t.Fatalf("phase = %s, want awaiting_approval", run.State.Phase)
	}
	assertFileContent(t, filepath.Join(run.Dir, planFileName), revisedPlan)
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviser", 2)
	assertHelperCount(t, counters, "reviewer", 2)
}

func TestPlanCycleInvalidVerdictFailsAfterReadOnlyRetries(t *testing.T) {
	tests := []string{
		"review-missing",
		"review-malformed",
		"review-schema",
		"review-inconsistent",
		"review-target-mismatch",
	}
	for _, scenario := range tests {
		t.Run(scenario, func(t *testing.T) {
			run, counters := newPlanCycleTestRun(t, scenario, 2, 5)
			err := executePlanCycle(t, run)
			if err == nil {
				t.Fatal("PlanCycle.Run() unexpectedly succeeded")
			}
			if run.State.Phase != state.PhaseFailed ||
				run.State.LastError == nil {
				t.Fatalf("failed state = %#v", run.State)
			}
			assertHelperCount(t, counters, "writer", 1)
			assertHelperCount(t, counters, "reviewer", 3)
			assertHelperCount(t, counters, "reviser", 0)
		})
	}
}

func TestPlanCycleRejectsInvalidPlanStructure(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "invalid-plan", 1, 5)
	err := executePlanCycle(t, run)
	if err == nil {
		t.Fatal("PlanCycle.Run() unexpectedly accepted an invalid plan")
	}
	if run.State.Phase != state.PhaseFailed {
		t.Fatalf("phase = %s, want failed", run.State.Phase)
	}
	assertHelperCount(t, counters, "writer", 2)
	assertHelperCount(t, counters, "reviewer", 0)
}

func TestPlanCycleQuestionsPauseForInput(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "questions", 0, 5)
	runPlanCycle(t, run)

	if run.State.Phase != state.PhasePausedForInput ||
		run.State.PendingAction == nil {
		t.Fatalf("paused state = %#v", run.State)
	}
	action := run.State.PendingAction
	if action.Kind != state.PendingPlanQuestion ||
		action.ResumePhase != state.PhasePlanning ||
		action.TaskID != nil ||
		action.Response != nil ||
		!strings.Contains(action.Prompt, "Which target") {
		t.Fatalf("pending action = %#v", action)
	}
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviewer", 0)
}

func TestPlanCycleCapPausesBeforeStartingAnotherRound(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "clean", 0, 1)
	run.State.PlanRound = 1
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}

	runPlanCycle(t, run)
	if run.State.Phase != state.PhasePausedForInput ||
		run.State.PendingAction == nil ||
		run.State.PendingAction.Kind != state.PendingPlanCap ||
		run.State.PendingAction.Response != nil {
		t.Fatalf("cap state = %#v", run.State)
	}
	if run.State.PlanRound != 1 {
		t.Fatalf("cap changed plan_round to %d", run.State.PlanRound)
	}
	assertHelperCount(t, counters, "writer", 0)
	assertHelperCount(t, counters, "reviewer", 0)
}

func TestPlanCycleLastAllowedRoundCanSucceed(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "clean", 0, 1)
	runPlanCycle(t, run)

	if run.State.Phase != state.PhaseAwaitingApproval ||
		run.State.PlanRound != 1 ||
		run.State.PendingAction != nil {
		t.Fatalf("last allowed round state = %#v", run.State)
	}
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviewer", 1)
}

func TestPlanCycleConsumesPlanCapOverrideOnSameState(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "cap-resume", 0, 1)
	runPlanCycle(t, run)
	if run.State.Phase != state.PhasePausedForInput ||
		run.State.PendingAction == nil ||
		run.State.PendingAction.Kind != state.PendingPlanCap {
		t.Fatalf("initial cap state = %#v", run.State)
	}

	response := "Fix the verification before approval."
	resumed, err := run.State.ResumePending(&response)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Override == nil {
		t.Fatal("ResumePending() did not return a plan override")
	}
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}

	executor := runner.New(runner.WithoutSignalHandling())
	defer executor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := NewPlanCycle(executor).Run(
		ctx,
		run,
		*resumed.Action.Response,
		resumed.Override,
	); err != nil {
		t.Fatalf("PlanCycle.Run() error = %v", err)
	}
	if !resumed.Override.Consumed() {
		t.Fatal("plan override was not consumed")
	}
	if run.State.Phase != state.PhaseAwaitingApproval ||
		run.State.PlanRound != 2 {
		t.Fatalf("resumed state = %#v", run.State)
	}
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviser", 1)
	assertHelperCount(t, counters, "reviewer", 2)
}

func TestPlanCyclePlanningAuthFailurePausesResponseFree(t *testing.T) {
	// "writer-auth-stdout" is the stream-json shape: the marker is in stdout and stderr
	// is empty. Before T13a-2 the classifier only read stderr, so this ran out its
	// retries instead of pausing — and a unit test on ClassifyFailure alone could not
	// prove that plan_cycle actually passes the stdout log (review T13a-2 f6).
	for _, scenario := range []string{
		"writer-auth",
		"writer-auth-stdout",
		"reviewer-auth",
	} {
		t.Run(scenario, func(t *testing.T) {
			run, counters := newPlanCycleTestRun(t, scenario, 0, 5)
			runPlanCycle(t, run)

			if run.State.Phase != state.PhasePausedForInput ||
				run.State.PendingAction == nil {
				t.Fatalf("auth pause state = %#v", run.State)
			}
			action := run.State.PendingAction
			if action.Kind != state.PendingAuth ||
				action.ResumePhase != state.PhasePlanning ||
				action.TaskID != nil ||
				action.Response != nil {
				t.Fatalf("auth pending action = %#v", action)
			}
			if strings.HasPrefix(scenario, "writer-auth") {
				assertHelperCount(t, counters, "writer", 1)
				if run.State.PlanHash != nil {
					t.Fatal("writer auth failure unexpectedly adopted a plan")
				}
			} else {
				assertHelperCount(t, counters, "writer", 1)
				assertHelperCount(t, counters, "reviewer", 1)
				assertPersistedPlanHash(t, run)
			}
		})
	}
}

func TestPlanCycleAuthResumeDoesNotReuseStaleAuthLog(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "reviewer-auth-once", 0, 5)
	runPlanCycle(t, run)
	if run.State.Phase != state.PhasePausedForInput ||
		run.State.PendingAction == nil ||
		run.State.PendingAction.Kind != state.PendingAuth {
		t.Fatalf("auth pause state = %#v", run.State)
	}
	if _, err := run.State.ResumePending(nil); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveState(); err != nil {
		t.Fatal(err)
	}

	err := executePlanCycle(t, run)
	if err == nil {
		t.Fatal("generic failure after auth resume unexpectedly succeeded")
	}
	if run.State.Phase != state.PhaseFailed {
		t.Fatalf("phase = %s, want failed rather than another auth pause", run.State.Phase)
	}
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviewer", 2)
}

func TestPlanCycleFailsIfReviewerChangesCanonicalPlan(t *testing.T) {
	run, counters := newPlanCycleTestRun(t, "review-mutates-plan", 1, 5)
	err := executePlanCycle(t, run)
	if err == nil {
		t.Fatal("PlanCycle.Run() ignored a modified canonical plan")
	}
	if run.State.Phase != state.PhaseFailed {
		t.Fatalf("phase = %s, want failed", run.State.Phase)
	}
	assertHelperCount(t, counters, "writer", 1)
	assertHelperCount(t, counters, "reviewer", 1)
}

func runPlanWriterHelper(runDir, counterDir, scenario string, rendered ...string) {
	incrementHelperCounter(counterDir, "writer")
	if len(rendered) == 1 {
		if strings.Contains(rendered[0], "This is a REVISION.") ||
			!strings.Contains(rendered[0], "Build the requested feature.") ||
			!strings.Contains(rendered[0], filepath.Join(runDir, nextPlanFileName)) ||
			!strings.Contains(rendered[0], filepath.Join(runDir, questionsFileName)) {
			pipelineHelperFail("initial writer prompt has the wrong shape")
		}
	}
	switch scenario {
	case "writer-auth":
		_, _ = fmt.Fprintln(os.Stderr, "Not logged in")
		os.Exit(17)
	case "writer-auth-stdout":
		// stream-json puts the auth marker in stdout and leaves stderr empty — the
		// shape the real CLI produces under `--output-format stream-json`
		// (measured 2026-07-25, T13a-2). stderr is deliberately untouched here.
		_, _ = fmt.Fprintln(os.Stdout,
			`{"type":"system","subtype":"init","session_id":"s1"}`)
		_, _ = fmt.Fprintln(os.Stdout,
			`{"type":"result","subtype":"success","is_error":true,`+
				`"error":"authentication_failed"}`)
		os.Exit(17)
	case "questions":
		helperWriteFile(
			filepath.Join(runDir, questionsFileName),
			"Which target should this plan use?\n",
		)
	case "invalid-plan":
		helperWriteFile(
			filepath.Join(runDir, nextPlanFileName),
			"# Invalid\n\nThis is not a task.\n",
		)
	default:
		helperWriteFile(filepath.Join(runDir, nextPlanFileName), draftPlan)
	}
}

func runPlanReviserHelper(
	runDir string,
	counterDir string,
	scenario string,
	rendered string,
) {
	attempt := incrementHelperCounter(counterDir, "reviser")
	canonical, err := os.ReadFile(filepath.Join(runDir, planFileName))
	if err != nil {
		pipelineHelperFail(err.Error())
	}
	if string(canonical) != draftPlan {
		pipelineHelperFail("canonical plan was not preserved for reviser")
	}
	if scenario == "cap-resume" {
		if !strings.Contains(rendered, "Fix the verification before approval.") {
			pipelineHelperFail("reviser prompt omitted plan-cap response")
		}
	} else if !strings.Contains(rendered, "Address every plan review finding:") ||
		!strings.Contains(rendered, "Add a runnable verification") ||
		!strings.Contains(rendered, "Polish wording") {
		pipelineHelperFail("reviser prompt omitted validated findings")
	}
	if scenario == "reviser-retry" && attempt == 1 {
		helperWriteFile(
			filepath.Join(runDir, nextPlanFileName),
			"# Invalid retry output\n",
		)
		return
	}
	helperWriteFile(filepath.Join(runDir, nextPlanFileName), revisedPlan)
}

func runPlanReviewerHelper(
	runDir string,
	counterDir string,
	scenario string,
	rendered string,
) {
	attempt := incrementHelperCounter(counterDir, "reviewer")
	plan, err := os.ReadFile(filepath.Join(runDir, planFileName))
	if err != nil {
		pipelineHelperFail(err.Error())
	}
	targetHash := hashBytes(plan)
	persisted, err := state.Load(filepath.Join(runDir, stateFileName))
	if err != nil {
		pipelineHelperFail(err.Error())
	}
	if persisted.PlanHash == nil || *persisted.PlanHash != targetHash {
		pipelineHelperFail("plan hash was not persisted before review")
	}
	if !strings.Contains(rendered, targetHash) ||
		!strings.Contains(rendered, filepath.Join(runDir, planFileName)) {
		pipelineHelperFail("review prompt is not bound to the adopted plan")
	}

	reviewPath := filepath.Join(runDir, planReviewFile)
	switch scenario {
	case "reviewer-auth":
		_, _ = fmt.Fprintln(os.Stderr, "API key auth is missing a key")
		os.Exit(17)
	case "reviewer-auth-once":
		if attempt == 1 {
			_, _ = fmt.Fprintln(os.Stderr, "API key auth is missing a key")
			os.Exit(17)
		}
		_, _ = fmt.Fprintln(os.Stderr, "intentional generic failure")
		os.Exit(19)
	case "review-missing":
		return
	case "review-malformed":
		helperWriteFile(reviewPath, `{"schema_version":`)
		return
	case "review-schema":
		helperWriteFile(
			reviewPath,
			fmt.Sprintf(
				`{"schema_version":1,"target_plan_hash":%q,"clean":true,"findings":[],"extra":true}`,
				targetHash,
			),
		)
		return
	case "review-inconsistent":
		helperWriteFile(
			reviewPath,
			dirtyReviewJSON(targetHash, true),
		)
		return
	case "review-target-mismatch":
		helperWriteFile(
			reviewPath,
			cleanReviewJSON(strings.Repeat("0", 64)),
		)
		return
	case "review-mutates-plan":
		helperWriteFile(reviewPath, cleanReviewJSON(targetHash))
		helperWriteFile(filepath.Join(runDir, planFileName), draftPlan+"\nchanged\n")
		return
	case "dirty", "reviser-retry", "cap-resume":
		if attempt == 1 {
			helperWriteFile(reviewPath, dirtyReviewJSON(targetHash, false))
			return
		}
	}
	helperWriteFile(reviewPath, cleanReviewJSON(targetHash))
}

func cleanReviewJSON(targetHash string) string {
	return fmt.Sprintf(
		`{"schema_version":1,"target_plan_hash":%q,"clean":true,"findings":[]}`,
		targetHash,
	)
}

func dirtyReviewJSON(targetHash string, clean bool) string {
	return fmt.Sprintf(
		`{"schema_version":1,"target_plan_hash":%q,"clean":%t,"findings":[`+
			`{"id":"f1","severity":"major","task_id":"T1",`+
			`"issue":"The task needs stronger evidence",`+
			`"requested_change":"Add a runnable verification"},`+
			`{"id":"f2","severity":"minor","task_id":null,`+
			`"issue":"The wording is rough",`+
			`"requested_change":"Polish wording"}]}`,
		targetHash,
		clean,
	)
}

func newPlanCycleTestRun(
	t *testing.T,
	scenario string,
	maxRetries int,
	maxPlanRounds int,
) (*Run, string) {
	t.Helper()
	repoRoot := t.TempDir()
	runID := "test-run"
	runDir := filepath.Join(repoRoot, ".coterix", "runs", runID)
	counterDir := filepath.Join(repoRoot, "helper-counters")
	if err := os.Mkdir(counterDir, 0o700); err != nil {
		t.Fatal(err)
	}

	config := testConfig(t)
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		pipelineHelperEnvironment: "1",
		pipelineScenarioEnv:       scenario,
		pipelineRunDirEnv:         runDir,
		pipelineCounterDirEnv:     counterDir,
	}
	config.CLIs["claude"] = cli.CliConfig{
		Command: command,
		Args: []string{
			"-test.run=^TestPlanCycleHelperProcess$",
			"--",
			"claude",
		},
		Stdin: false,
		Env:   environment,
	}
	config.CLIs["codex"] = cli.CliConfig{
		Command: command,
		Args: []string{
			"-test.run=^TestPlanCycleHelperProcess$",
			"--",
			"codex",
		},
		Stdin: true,
		Env:   environment,
	}
	config.IdleTimeoutSecs = 5
	config.MaxRetries = maxRetries
	config.MaxPlanRounds = maxPlanRounds

	run, err := CreateRun(repoRoot, runID, "Build the requested feature.", config)
	if err != nil {
		t.Fatal(err)
	}
	return run, counterDir
}

func runPlanCycle(t *testing.T, run *Run) {
	t.Helper()
	if err := executePlanCycle(t, run); err != nil {
		t.Fatalf("PlanCycle.Run() error = %v", err)
	}
}

func executePlanCycle(t *testing.T, run *Run) error {
	t.Helper()
	executor := runner.New(runner.WithoutSignalHandling())
	defer executor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return NewPlanCycle(executor).Run(ctx, run, "", nil)
}

func assertPersistedPlanHash(t *testing.T, run *Run) {
	t.Helper()
	if run.State.PlanHash == nil {
		t.Fatal("plan_hash is nil")
	}
	content, err := os.ReadFile(filepath.Join(run.Dir, planFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := hashBytes(content); got != *run.State.PlanHash {
		t.Fatalf("state.plan_hash = %s, sha256(plan.md) = %s", *run.State.PlanHash, got)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), content, want)
	}
}

func assertHelperCount(t *testing.T, directory, name string, want int) {
	t.Helper()
	if got := readHelperCount(t, directory, name); got != want {
		t.Fatalf("%s helper count = %d, want %d", name, got, want)
	}
}

func readHelperCount(t *testing.T, directory, name string) int {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, name+".count"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.Atoi(string(content))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func incrementHelperCounter(directory, name string) int {
	path := filepath.Join(directory, name+".count")
	content, err := os.ReadFile(path)
	value := 0
	if err == nil {
		value, err = strconv.Atoi(string(content))
	}
	if err != nil && !os.IsNotExist(err) {
		pipelineHelperFail(err.Error())
	}
	value++
	helperWriteFile(path, strconv.Itoa(value))
	return value
}

func helperWriteFile(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		pipelineHelperFail(err.Error())
	}
}

func argumentsAfterDoubleDash(arguments []string) []string {
	for index, argument := range arguments {
		if argument == "--" {
			return arguments[index+1:]
		}
	}
	return nil
}

func pipelineHelperFail(message string) {
	encoded, _ := json.Marshal(message)
	_, _ = fmt.Fprintln(os.Stderr, string(encoded))
	os.Exit(90)
}
