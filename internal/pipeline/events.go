package pipeline

import (
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

// EventKind identifies one observable pipeline update.
type EventKind string

const (
	EventStateSnapshot   EventKind = "state_snapshot"
	EventStepStarted     EventKind = "step_started"
	EventStepLog         EventKind = "step_log"
	EventAttemptFinished EventKind = "attempt_finished"
	EventStepFinished    EventKind = "step_finished"
)

const (
	StepPlan                 = "plan"
	StepPlanReview           = "plan_review"
	StepImplementation       = "implementation"
	StepGate                 = "gate"
	StepImplementationReview = "implementation_review"
	StepFix                  = "fix"
)

// Event is the pipeline-owned boundary consumed by renderers such as the TUI.
// Fields that do not apply to a Kind are nil or empty. Status, Line, and Result
// are copies and may be retained or modified by an observer.
type Event struct {
	Kind     EventKind
	RepoRoot string
	RunID    string
	Step     string
	Role     string
	CLI      string
	Attempt  int
	Line     *runner.Line
	Result   *runner.RunResult
	Err      error
	Status   *RunStatus
}

// Observer receives pipeline events synchronously. It may be called from
// subprocess output goroutines or from different runs at the same time.
type Observer func(Event)

// ControllerOption configures a Controller without changing its control-plane
// method surface.
type ControllerOption func(*Controller)

// WithObserver connects one observer to state and subprocess lifecycle events.
func WithObserver(observer Observer) ControllerOption {
	return func(controller *Controller) {
		controller.observer = observer
	}
}

func (controller *Controller) observeRun(currentRun *Run) {
	if currentRun == nil {
		return
	}
	if controller != nil {
		currentRun.observer = controller.observer
	}
	currentRun.emitStateSnapshot()
}

func (controller *Controller) newPlanCycle() *PlanCycle {
	cycle := NewPlanCycle(controller.Executor)
	cycle.observer = controller.observer
	return cycle
}

func (controller *Controller) newTaskCycle() *TaskCycle {
	cycle := NewTaskCycle(controller.Executor)
	cycle.observer = controller.observer
	return cycle
}

func (run *Run) emitStateSnapshot() {
	if run == nil || run.State == nil || run.observer == nil {
		return
	}
	status := statusFromRun(run)
	emitEvent(run.observer, Event{
		Kind:     EventStateSnapshot,
		RepoRoot: run.RepoRoot,
		RunID:    run.ID,
		Status:   &status,
	})
}

func emitEvent(observer Observer, event Event) {
	if observer == nil {
		return
	}
	observer(cloneEvent(event))
}

func cloneEvent(event Event) Event {
	if event.Line != nil {
		line := *event.Line
		event.Line = &line
	}
	if event.Result != nil {
		result := *event.Result
		event.Result = &result
	}
	if event.Status != nil {
		status := cloneRunStatus(*event.Status)
		event.Status = &status
	}
	return event
}

func cloneRunStatus(status RunStatus) RunStatus {
	status.PlanHash = cloneString(status.PlanHash)
	status.ApprovedPlanHash = cloneString(status.ApprovedPlanHash)
	status.TaskOrder = append([]string(nil), status.TaskOrder...)
	status.CurrentTaskID = cloneString(status.CurrentTaskID)
	status.LastError = cloneString(status.LastError)

	if status.PendingAction != nil {
		pending := *status.PendingAction
		pending.TaskID = cloneString(pending.TaskID)
		pending.Response = cloneString(pending.Response)
		status.PendingAction = &pending
	}

	tasks := make(map[string]state.TaskState, len(status.Tasks))
	for taskID, task := range status.Tasks {
		task.BaseSHA = cloneString(task.BaseSHA)
		task.CandidateSHA = cloneString(task.CandidateSHA)
		task.GateResult = cloneString(task.GateResult)
		task.ReviewResult = cloneString(task.ReviewResult)
		tasks[taskID] = task
	}
	status.Tasks = tasks
	return status
}

func observeStep(
	observer Observer,
	currentRun *Run,
	step string,
	role string,
	cliName string,
	request *runner.RunRequest,
) func(runner.RunResult, error) {
	if observer == nil || currentRun == nil || request == nil {
		return func(runner.RunResult, error) {}
	}

	runID := currentRun.ID
	previousOnLine := request.OnLine
	request.OnLine = func(line runner.Line) {
		if previousOnLine != nil {
			previousOnLine(line)
		}
		emitEvent(observer, Event{
			Kind:     EventStepLog,
			RepoRoot: currentRun.RepoRoot,
			RunID:    runID,
			Step:     step,
			Role:     role,
			CLI:      cliName,
			Line:     &line,
		})
	}
	previousOnAttemptDone := request.OnAttemptDone
	request.OnAttemptDone = func(done runner.AttemptDone) {
		if previousOnAttemptDone != nil {
			previousOnAttemptDone(done)
		}
		result := done.Result
		emitEvent(observer, Event{
			Kind:     EventAttemptFinished,
			RepoRoot: currentRun.RepoRoot,
			RunID:    runID,
			Step:     step,
			Role:     role,
			CLI:      cliName,
			Attempt:  done.Attempt,
			Result:   &result,
			Err:      done.Err,
		})
	}
	emitEvent(observer, Event{
		Kind:     EventStepStarted,
		RepoRoot: currentRun.RepoRoot,
		RunID:    runID,
		Step:     step,
		Role:     role,
		CLI:      cliName,
	})
	return func(result runner.RunResult, err error) {
		emitEvent(observer, Event{
			Kind:     EventStepFinished,
			RepoRoot: currentRun.RepoRoot,
			RunID:    runID,
			Step:     step,
			Role:     role,
			CLI:      cliName,
			Result:   &result,
			Err:      err,
		})
	}
}
