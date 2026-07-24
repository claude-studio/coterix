package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

type eventCollector struct {
	mu     sync.Mutex
	events []Event
}

func (collector *eventCollector) observe(event Event) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.events = append(collector.events, event)
}

func (collector *eventCollector) snapshot() []Event {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]Event(nil), collector.events...)
}

func TestObserveStepChainsCallbacksAndCopiesAttemptEvidence(t *testing.T) {
	collector := &eventCollector{}
	lineCalls := 0
	attemptCalls := 0
	request := runner.RunRequest{
		OnLine: func(line runner.Line) {
			lineCalls++
			if line.Text != "streamed output" {
				t.Fatalf("chained line = %#v", line)
			}
		},
		OnAttemptDone: func(done runner.AttemptDone) {
			attemptCalls++
			if done.Attempt != 2 || !done.Result.TimedOut {
				t.Fatalf("chained attempt = %#v", done)
			}
		},
	}
	currentRun := &Run{ID: "observed-run"}
	finish := observeStep(
		collector.observe,
		currentRun,
		StepPlanReview,
		string(cli.RolePlanReviewer),
		"codex",
		&request,
	)

	line := runner.Line{
		Attempt: 2,
		Stream:  runner.StreamStderr,
		Text:    "streamed output",
	}
	request.OnLine(line)
	attemptErr := errors.New("idle timeout")
	attemptResult := runner.RunResult{
		Exit:      -1,
		TimedOut:  true,
		StdoutLog: "stdout.log",
		StderrLog: "stderr.log",
	}
	request.OnAttemptDone(runner.AttemptDone{
		Attempt: 2,
		Result:  attemptResult,
		Err:     attemptErr,
	})
	finish(attemptResult, attemptErr)

	line.Text = "changed"
	attemptResult.TimedOut = false
	events := collector.snapshot()
	if lineCalls != 1 || attemptCalls != 1 {
		t.Fatalf(
			"chained callbacks: line=%d attempt=%d",
			lineCalls,
			attemptCalls,
		)
	}
	if len(events) != 4 {
		t.Fatalf("events = %#v, want start/log/attempt/finish", events)
	}
	if events[0].Kind != EventStepStarted ||
		events[1].Kind != EventStepLog ||
		events[2].Kind != EventAttemptFinished ||
		events[3].Kind != EventStepFinished {
		t.Fatalf("event order = %#v", events)
	}
	for _, event := range events {
		if event.RunID != currentRun.ID ||
			event.Step != StepPlanReview ||
			event.Role != string(cli.RolePlanReviewer) ||
			event.CLI != "codex" {
			t.Fatalf("event metadata = %#v", event)
		}
	}
	if events[1].Line == nil ||
		events[1].Line.Text != "streamed output" ||
		events[1].Line.Attempt != 2 {
		t.Fatalf("log event = %#v", events[1])
	}
	if events[2].Attempt != 2 ||
		events[2].Result == nil ||
		!events[2].Result.TimedOut ||
		events[2].Err != attemptErr {
		t.Fatalf("attempt event = %#v", events[2])
	}
	if events[3].Result == nil ||
		!events[3].Result.TimedOut ||
		events[3].Err != attemptErr {
		t.Fatalf("finished event = %#v", events[3])
	}
}

func TestControllerObserverEmitsInitialSavedAndPlanningStepEvents(t *testing.T) {
	root, _ := controlNewRepository(t, true, testConfig(t))
	collector := &eventCollector{}
	controller := NewController(
		&controlPlanExecutor{},
		WithObserver(collector.observe),
	)

	status, err := controller.Run(context.Background(), root, "observe planning")
	if err != nil {
		t.Fatalf("Controller.Run() error = %v", err)
	}
	canonicalRoot, err := repositoryRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	events := collector.snapshot()
	if len(events) == 0 ||
		events[0].Kind != EventStateSnapshot ||
		events[0].RepoRoot != canonicalRoot ||
		events[0].RunID == "" ||
		events[0].Status == nil ||
		events[0].Status.RunID != events[0].RunID ||
		events[0].Status.Phase != state.PhasePlanning ||
		events[0].Status.PlanRound != 0 {
		t.Fatalf("initial event = %#v", events)
	}
	if events[0].RunID != status.RunID {
		t.Fatalf("initial run_id = %q, final = %q", events[0].RunID, status.RunID)
	}
	requirePipelineEvent(
		t,
		events,
		EventStepStarted,
		StepPlan,
		string(cli.RolePlanWriter),
		"claude",
	)
	requirePipelineEvent(
		t,
		events,
		EventStepFinished,
		StepPlanReview,
		string(cli.RolePlanReviewer),
		"codex",
	)

	var awaiting *Event
	for index := range events {
		event := &events[index]
		if event.Kind == EventStateSnapshot &&
			event.Status != nil &&
			event.Status.Phase == state.PhaseAwaitingApproval {
			awaiting = event
		}
	}
	if awaiting == nil ||
		awaiting.Status.PlanRound != 1 ||
		awaiting.Status.Tasks["T1"].Status != state.TaskOpen {
		t.Fatalf("awaiting snapshot absent or incomplete: %#v", events)
	}

	awaiting.Status.TaskOrder[0] = "changed"
	awaiting.Status.Tasks["T1"] = state.TaskState{Status: state.TaskFailed}
	persisted := controlOpenRun(t, root, status.RunID)
	if persisted.State.TaskOrder[0] != "T1" ||
		persisted.State.Tasks["T1"].Status != state.TaskOpen {
		t.Fatal("observer mutation changed persisted pipeline state")
	}
}

func TestControllerObserverCoversTaskRepairLifecycle(t *testing.T) {
	fixture := newTaskCycleTestRun(t, "review-dirty-repair")
	if err := fixture.run.State.PauseForAuth(
		nil,
		"Resume the observed task cycle.",
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.run.SaveState(); err != nil {
		t.Fatal(err)
	}

	processRunner := runner.New(runner.WithoutSignalHandling())
	defer processRunner.Close()
	collector := &eventCollector{}
	controller := NewController(
		processRunner,
		WithObserver(collector.observe),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := controller.Resume(ctx, fixture.run.RepoRoot, fixture.run.ID, nil)
	if err != nil {
		t.Fatalf("Controller.Resume() error = %v", err)
	}
	if status.Phase != state.PhaseDone {
		t.Fatalf("resumed phase = %s, want done", status.Phase)
	}

	events := collector.snapshot()
	if len(events) == 0 ||
		events[0].Kind != EventStateSnapshot ||
		events[0].Status == nil ||
		events[0].Status.Phase != state.PhasePausedForInput {
		t.Fatalf("opened-run snapshot = %#v", events)
	}
	requirePipelineEvent(
		t,
		events,
		EventStepStarted,
		StepImplementation,
		string(cli.RoleImplWriter),
		"codex",
	)
	requirePipelineEvent(
		t,
		events,
		EventStepStarted,
		StepGate,
		StepGate,
		fixture.run.Config.GateCommand[0],
	)
	requirePipelineEvent(
		t,
		events,
		EventStepStarted,
		StepImplementationReview,
		string(cli.RoleImplReviewer),
		"claude",
	)
	requirePipelineEvent(
		t,
		events,
		EventStepStarted,
		StepFix,
		string(cli.RoleFixer),
		"codex",
	)

	sawImplementing := false
	sawAttempt := false
	for _, event := range events {
		if event.Kind == EventStateSnapshot &&
			event.Status != nil &&
			event.Status.Phase == state.PhaseImplementing {
			sawImplementing = true
		}
		if event.Kind == EventAttemptFinished {
			sawAttempt = true
			if event.Attempt < 1 || event.Result == nil {
				t.Fatalf("attempt event lacks evidence: %#v", event)
			}
		}
	}
	if !sawImplementing || !sawAttempt {
		t.Fatalf(
			"events missing implementing snapshot or attempt evidence: %#v",
			events,
		)
	}
}

func requirePipelineEvent(
	t *testing.T,
	events []Event,
	kind EventKind,
	step string,
	role string,
	cliName string,
) {
	t.Helper()
	for _, event := range events {
		if event.Kind == kind &&
			event.Step == step &&
			event.Role == role &&
			event.CLI == cliName {
			return
		}
	}
	t.Fatalf(
		"missing event kind=%s step=%s role=%s cli=%s in %#v",
		kind,
		step,
		role,
		cliName,
		events,
	)
}
