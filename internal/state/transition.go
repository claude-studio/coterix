package state

import (
	"fmt"
	"strings"
)

// ResumeResult contains the consumed action and any immediate one-shot cap
// override. The caller uses Action.Response as feedback for the resumed step.
type ResumeResult struct {
	Action   PendingAction
	Override *OneShotOverride
	Aborted  bool
}

// RejectResult carries human feedback and the one revision override granted by
// an explicit rejection.
type RejectResult struct {
	Feedback string
	Override *OneShotOverride
}

// TransitionPhase applies a direct phase transition. Pause, resume, and reject
// use their dedicated helpers so their pending-action and override invariants
// cannot be bypassed.
func (current *State) TransitionPhase(next Phase) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if !next.Valid() {
		return fmt.Errorf("state: invalid target phase %q", next)
	}
	switch {
	case next == PhasePausedForInput:
		return fmt.Errorf("state: use Pause to enter paused_for_input")
	case current.Phase == PhasePausedForInput && next != PhaseFailed:
		return fmt.Errorf("state: use ResumePending to leave paused_for_input")
	case current.Phase == PhaseAwaitingApproval && next == PhasePlanning:
		return fmt.Errorf("state: use RejectPlan to return to planning")
	case !phaseTransitionAllowed(current.Phase, next):
		return fmt.Errorf(
			"state: phase transition %s -> %s is not allowed",
			current.Phase,
			next,
		)
	}

	previousPhase := current.Phase
	previousPending := current.PendingAction
	current.Phase = next
	if next == PhaseFailed {
		current.PendingAction = nil
	}
	if err := current.Validate(); err != nil {
		current.Phase = previousPhase
		current.PendingAction = previousPending
		return err
	}
	return nil
}

// Fail moves any non-terminal run to failed and records a non-empty error.
func (current *State) Fail(message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("state: failure message is required")
	}
	if err := current.Validate(); err != nil {
		return err
	}
	if !phaseTransitionAllowed(current.Phase, PhaseFailed) {
		return fmt.Errorf("state: phase %s cannot transition to failed", current.Phase)
	}

	previousPhase := current.Phase
	previousPending := current.PendingAction
	previousError := current.LastError
	current.Phase = PhaseFailed
	current.PendingAction = nil
	current.LastError = stringPointer(message)
	if err := current.Validate(); err != nil {
		current.Phase = previousPhase
		current.PendingAction = previousPending
		current.LastError = previousError
		return err
	}
	return nil
}

// TransitionTask applies one harness-owned task status transition.
func (current *State) TransitionTask(taskID string, next TaskStatus) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if current.Phase != PhaseImplementing {
		return fmt.Errorf("state: tasks can transition only while implementing")
	}
	if current.CurrentTaskID == nil || *current.CurrentTaskID != taskID {
		return fmt.Errorf("state: task %q is not the current task", taskID)
	}
	if !next.Valid() {
		return fmt.Errorf("state: invalid target task status %q", next)
	}
	task, exists := current.Tasks[taskID]
	if !exists || task == nil {
		return fmt.Errorf("state: task %q does not exist", taskID)
	}
	if !taskTransitionAllowed(task.Status, next) {
		return fmt.Errorf(
			"state: task %q transition %s -> %s is not allowed",
			taskID,
			task.Status,
			next,
		)
	}

	previous := task.Status
	task.Status = next
	if err := current.Validate(); err != nil {
		task.Status = previous
		return err
	}
	return nil
}

// Pause enters paused_for_input with exactly one unresolved action.
func (current *State) Pause(action PendingAction) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if current.Phase != PhasePlanning && current.Phase != PhaseImplementing {
		return fmt.Errorf("state: phase %s cannot pause for input", current.Phase)
	}
	if action.Response != nil {
		return fmt.Errorf("state: a new pending_action response must be null")
	}
	if action.ResumePhase != current.Phase {
		return fmt.Errorf("state: pending_action resume_phase must match current phase")
	}
	if err := current.validatePendingAction(action); err != nil {
		return err
	}

	previousPhase := current.Phase
	current.Phase = PhasePausedForInput
	action = clonePendingAction(action)
	current.PendingAction = &action
	if err := current.Validate(); err != nil {
		current.Phase = previousPhase
		current.PendingAction = nil
		return err
	}
	return nil
}

// PauseForPlanQuestion pauses planning for planner-requested human input.
func (current *State) PauseForPlanQuestion(prompt string) error {
	return current.Pause(PendingAction{
		Kind:        PendingPlanQuestion,
		ResumePhase: PhasePlanning,
		Prompt:      prompt,
	})
}

// PauseForPlanCap pauses planning instead of accepting a plan automatically.
func (current *State) PauseForPlanCap(prompt string) error {
	return current.Pause(PendingAction{
		Kind:        PendingPlanCap,
		ResumePhase: PhasePlanning,
		Prompt:      prompt,
	})
}

// PauseForTaskCap pauses the current task instead of accepting it.
func (current *State) PauseForTaskCap(taskID, prompt string) error {
	return current.Pause(PendingAction{
		Kind:        PendingTaskCap,
		ResumePhase: PhaseImplementing,
		TaskID:      stringPointer(taskID),
		Prompt:      prompt,
	})
}

// PauseForAuth pauses an active planning or implementation phase while the
// user repairs provider credentials outside Coterix.
func (current *State) PauseForAuth(taskID *string, prompt string) error {
	return current.Pause(PendingAction{
		Kind:        PendingAuth,
		ResumePhase: current.Phase,
		TaskID:      cloneString(taskID),
		Prompt:      prompt,
	})
}

// ResumePending validates the kind-specific response, clears pending_action,
// and restores its resume phase. plan_cap and task_cap retry return an override
// that must be consumed by the immediately following Begin call.
func (current *State) ResumePending(response *string) (ResumeResult, error) {
	if err := current.Validate(); err != nil {
		return ResumeResult{}, err
	}
	if current.Phase != PhasePausedForInput || current.PendingAction == nil {
		return ResumeResult{}, fmt.Errorf("state: run is not paused for input")
	}

	action := clonePendingAction(*current.PendingAction)
	if response != nil {
		if action.Response != nil {
			return ResumeResult{}, fmt.Errorf("state: pending_action already has a response")
		}
		action.Response = cloneString(response)
	}
	if err := validateResumeResponse(action); err != nil {
		return ResumeResult{}, err
	}

	result := ResumeResult{Action: action}
	previousPhase := current.Phase
	previousPending := current.PendingAction
	previousError := current.LastError
	var previousTaskStatus TaskStatus
	var changedTask *TaskState

	current.PendingAction = nil
	switch action.Kind {
	case PendingPlanQuestion:
		current.Phase = PhasePlanning
	case PendingPlanCap:
		current.Phase = PhasePlanning
		result.Override = newPlanOverride(current, OverridePlanCap)
	case PendingTaskCap:
		task := current.Tasks[*action.TaskID]
		if *action.Response == "abort" {
			changedTask = task
			previousTaskStatus = task.Status
			task.Status = TaskFailed
			current.Phase = PhaseFailed
			current.LastError = stringPointer(
				fmt.Sprintf("task %s aborted after reaching attempt cap", *action.TaskID),
			)
			result.Aborted = true
		} else {
			current.Phase = PhaseImplementing
			result.Override = newTaskOverride(current, *action.TaskID)
		}
	case PendingAuth:
		current.Phase = action.ResumePhase
	}

	if err := current.Validate(); err != nil {
		current.Phase = previousPhase
		current.PendingAction = previousPending
		current.LastError = previousError
		if changedTask != nil {
			changedTask.Status = previousTaskStatus
		}
		return ResumeResult{}, err
	}
	return result, nil
}

// RejectPlan returns from awaiting approval to planning without resetting the
// round counter and grants exactly one immediate revision override.
func (current *State) RejectPlan(response string) (RejectResult, error) {
	if err := current.Validate(); err != nil {
		return RejectResult{}, err
	}
	if current.Phase != PhaseAwaitingApproval {
		return RejectResult{}, fmt.Errorf("state: plan can be rejected only while awaiting approval")
	}
	if strings.TrimSpace(response) == "" {
		return RejectResult{}, fmt.Errorf("state: plan rejection response is required")
	}

	current.Phase = PhasePlanning
	if err := current.Validate(); err != nil {
		current.Phase = PhaseAwaitingApproval
		return RejectResult{}, err
	}
	return RejectResult{
		Feedback: response,
		Override: newPlanOverride(current, OverridePlanReject),
	}, nil
}

func validateResumeResponse(action PendingAction) error {
	switch action.Kind {
	case PendingPlanQuestion, PendingPlanCap:
		if action.Response == nil || strings.TrimSpace(*action.Response) == "" {
			return fmt.Errorf("state: %s resume requires a response", action.Kind)
		}
	case PendingTaskCap:
		if action.Response == nil ||
			(*action.Response != "retry" && *action.Response != "abort") {
			return fmt.Errorf("state: task_cap response must be exactly retry or abort")
		}
	case PendingAuth:
		if action.Response != nil {
			return fmt.Errorf("state: auth resume forbids a response")
		}
	default:
		return fmt.Errorf("state: invalid pending_action kind %q", action.Kind)
	}
	return nil
}

func phaseTransitionAllowed(from, to Phase) bool {
	switch from {
	case PhasePlanning:
		return to == PhaseAwaitingApproval || to == PhasePausedForInput || to == PhaseFailed
	case PhaseAwaitingApproval:
		return to == PhasePlanning || to == PhaseImplementing || to == PhaseFailed
	case PhasePausedForInput:
		return to == PhasePlanning || to == PhaseImplementing || to == PhaseFailed
	case PhaseImplementing:
		return to == PhasePausedForInput || to == PhaseDone || to == PhaseFailed
	default:
		return false
	}
}

func taskTransitionAllowed(from, to TaskStatus) bool {
	switch from {
	case TaskOpen:
		return to == TaskCandidate || to == TaskFailed
	case TaskCandidate:
		return to == TaskRepairing || to == TaskConfirmed || to == TaskFailed
	case TaskRepairing:
		return to == TaskCandidate || to == TaskFailed
	default:
		return false
	}
}

func stringPointer(value string) *string {
	return &value
}
