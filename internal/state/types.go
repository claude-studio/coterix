package state

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SchemaVersion is the only state.json schema understood by this build.
const SchemaVersion = 1

// Phase is the top-level orchestration phase.
type Phase string

const (
	PhasePlanning         Phase = "planning"
	PhaseAwaitingApproval Phase = "awaiting_approval"
	PhasePausedForInput   Phase = "paused_for_input"
	PhaseImplementing     Phase = "implementing"
	PhaseDone             Phase = "done"
	PhaseFailed           Phase = "failed"
)

// TaskStatus is the harness-owned status for one approved-plan task.
type TaskStatus string

const (
	TaskOpen      TaskStatus = "open"
	TaskCandidate TaskStatus = "candidate"
	TaskRepairing TaskStatus = "repairing"
	TaskConfirmed TaskStatus = "confirmed"
	TaskFailed    TaskStatus = "failed"
)

// PendingKind identifies the single human action blocking a run.
type PendingKind string

const (
	PendingPlanQuestion PendingKind = "plan_question"
	PendingPlanCap      PendingKind = "plan_cap"
	PendingTaskCap      PendingKind = "task_cap"
	PendingAuth         PendingKind = "auth"
)

// State is the complete orchestration source of truth persisted as state.json.
type State struct {
	SchemaVersion    int                   `json:"schema_version"`
	Phase            Phase                 `json:"phase"`
	PlanHash         *string               `json:"plan_hash"`
	ApprovedPlanHash *string               `json:"approved_plan_hash"`
	PlanRound        int                   `json:"plan_round"`
	PendingAction    *PendingAction        `json:"pending_action"`
	TaskOrder        []string              `json:"task_order"`
	CurrentTaskID    *string               `json:"current_task_id"`
	Tasks            map[string]*TaskState `json:"tasks"`
	LastError        *string               `json:"last_error"`
}

// TaskState is the persisted state for one task.
type TaskState struct {
	Status       TaskStatus `json:"status"`
	Attempt      int        `json:"attempt"`
	BaseSHA      *string    `json:"base_sha"`
	CandidateSHA *string    `json:"candidate_sha"`
	GateResult   *string    `json:"gate_result"`
	ReviewResult *string    `json:"review_result"`
}

// PendingAction is the one human action that must be resolved before a run can
// continue. Response is initially nil.
type PendingAction struct {
	Kind        PendingKind `json:"kind"`
	ResumePhase Phase       `json:"resume_phase"`
	TaskID      *string     `json:"task_id"`
	Prompt      string      `json:"prompt"`
	Response    *string     `json:"response"`
}

// New returns an empty run state in its initial planning phase.
func New() *State {
	return &State{
		SchemaVersion: SchemaVersion,
		Phase:         PhasePlanning,
		TaskOrder:     make([]string, 0),
		Tasks:         make(map[string]*TaskState),
	}
}

// Valid reports whether phase is part of the fixed state contract.
func (phase Phase) Valid() bool {
	switch phase {
	case PhasePlanning,
		PhaseAwaitingApproval,
		PhasePausedForInput,
		PhaseImplementing,
		PhaseDone,
		PhaseFailed:
		return true
	default:
		return false
	}
}

// Valid reports whether status is part of the fixed task contract.
func (status TaskStatus) Valid() bool {
	switch status {
	case TaskOpen, TaskCandidate, TaskRepairing, TaskConfirmed, TaskFailed:
		return true
	default:
		return false
	}
}

// Valid reports whether kind is part of the fixed pending-action contract.
func (kind PendingKind) Valid() bool {
	switch kind {
	case PendingPlanQuestion, PendingPlanCap, PendingTaskCap, PendingAuth:
		return true
	default:
		return false
	}
}

// Validate checks the persisted state schema and its local invariants.
func (current *State) Validate() error {
	if current == nil {
		return fmt.Errorf("state: value is nil")
	}
	switch {
	case current.SchemaVersion != SchemaVersion:
		return fmt.Errorf("state: unsupported schema_version %d", current.SchemaVersion)
	case !current.Phase.Valid():
		return fmt.Errorf("state: invalid phase %q", current.Phase)
	case current.PlanRound < 0:
		return fmt.Errorf("state: plan_round must not be negative")
	case current.TaskOrder == nil:
		return fmt.Errorf("state: task_order must be an array, not null")
	case current.Tasks == nil:
		return fmt.Errorf("state: tasks must be an object, not null")
	}

	for name, value := range map[string]*string{
		"plan_hash":          current.PlanHash,
		"approved_plan_hash": current.ApprovedPlanHash,
		"current_task_id":    current.CurrentTaskID,
	} {
		if err := validateOptionalString(name, value); err != nil {
			return err
		}
	}
	if err := validateOptionalNonBlank("last_error", current.LastError); err != nil {
		return err
	}

	taskIDs := make(map[string]struct{}, len(current.TaskOrder))
	for index, taskID := range current.TaskOrder {
		if strings.TrimSpace(taskID) == "" || strings.TrimSpace(taskID) != taskID {
			return fmt.Errorf("state: task_order[%d] must be a non-empty task id", index)
		}
		if _, duplicate := taskIDs[taskID]; duplicate {
			return fmt.Errorf("state: duplicate task id %q in task_order", taskID)
		}
		taskIDs[taskID] = struct{}{}

		task, exists := current.Tasks[taskID]
		if !exists {
			return fmt.Errorf("state: task_order id %q is missing from tasks", taskID)
		}
		if err := task.validate(taskID); err != nil {
			return err
		}
	}
	for taskID, task := range current.Tasks {
		if _, ordered := taskIDs[taskID]; !ordered {
			return fmt.Errorf("state: task %q is missing from task_order", taskID)
		}
		if task == nil {
			return fmt.Errorf("state: task %q must be an object, not null", taskID)
		}
	}
	if current.Phase == PhaseDone {
		for _, taskID := range current.TaskOrder {
			if current.Tasks[taskID].Status != TaskConfirmed {
				return fmt.Errorf(
					"state: done phase requires task %q to be confirmed",
					taskID,
				)
			}
		}
	}
	if current.CurrentTaskID != nil {
		if _, exists := current.Tasks[*current.CurrentTaskID]; !exists {
			return fmt.Errorf(
				"state: current_task_id %q does not exist in tasks",
				*current.CurrentTaskID,
			)
		}
	}

	if current.Phase == PhasePausedForInput {
		if current.PendingAction == nil {
			return fmt.Errorf("state: paused_for_input requires pending_action")
		}
		if err := current.validatePendingAction(*current.PendingAction); err != nil {
			return err
		}
	} else if current.PendingAction != nil {
		return fmt.Errorf("state: pending_action requires paused_for_input phase")
	}

	return nil
}

func (task *TaskState) validate(taskID string) error {
	if task == nil {
		return fmt.Errorf("state: task %q must be an object, not null", taskID)
	}
	switch {
	case !task.Status.Valid():
		return fmt.Errorf("state: task %q has invalid status %q", taskID, task.Status)
	case task.Attempt < 0:
		return fmt.Errorf("state: task %q attempt must not be negative", taskID)
	}
	for name, value := range map[string]*string{
		"base_sha":      task.BaseSHA,
		"candidate_sha": task.CandidateSHA,
	} {
		if err := validateOptionalString("task "+taskID+" "+name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]*string{
		"gate_result":   task.GateResult,
		"review_result": task.ReviewResult,
	} {
		if err := validateOptionalRelativePath("task "+taskID+" "+name, value); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the standalone pending-action field contract.
func (action PendingAction) Validate() error {
	switch {
	case !action.Kind.Valid():
		return fmt.Errorf("state: invalid pending_action kind %q", action.Kind)
	case action.ResumePhase != PhasePlanning && action.ResumePhase != PhaseImplementing:
		return fmt.Errorf(
			"state: pending_action resume_phase must be planning or implementing",
		)
	case strings.TrimSpace(action.Prompt) == "":
		return fmt.Errorf("state: pending_action prompt must not be empty")
	}
	if err := validateOptionalString("pending_action task_id", action.TaskID); err != nil {
		return err
	}
	if err := validateOptionalNonBlank("pending_action response", action.Response); err != nil {
		return err
	}

	switch action.Kind {
	case PendingPlanQuestion, PendingPlanCap:
		if action.ResumePhase != PhasePlanning || action.TaskID != nil {
			return fmt.Errorf(
				"state: %s must resume planning without a task_id",
				action.Kind,
			)
		}
	case PendingTaskCap:
		if action.ResumePhase != PhaseImplementing || action.TaskID == nil {
			return fmt.Errorf(
				"state: task_cap must resume implementing with a task_id",
			)
		}
		if action.Response != nil &&
			*action.Response != "retry" &&
			*action.Response != "abort" {
			return fmt.Errorf("state: task_cap response must be exactly retry or abort")
		}
	case PendingAuth:
		if action.Response != nil {
			return fmt.Errorf("state: auth pending_action cannot contain a response")
		}
	}
	return nil
}

func (current *State) validatePendingAction(action PendingAction) error {
	if err := action.Validate(); err != nil {
		return err
	}
	switch action.Kind {
	case PendingTaskCap:
		if current.CurrentTaskID == nil || *current.CurrentTaskID != *action.TaskID {
			return fmt.Errorf("state: task_cap task_id must match current_task_id")
		}
		task := current.Tasks[*action.TaskID]
		if task == nil || (task.Status != TaskOpen && task.Status != TaskRepairing) {
			return fmt.Errorf("state: task_cap requires a current open or repairing task")
		}
	case PendingAuth:
		if action.TaskID != nil {
			if current.CurrentTaskID == nil || *current.CurrentTaskID != *action.TaskID {
				return fmt.Errorf("state: auth task_id must match current_task_id")
			}
		}
	}
	return nil
}

func validateOptionalString(name string, value *string) error {
	if value != nil && (strings.TrimSpace(*value) == "" || strings.TrimSpace(*value) != *value) {
		return fmt.Errorf("state: %s must be null or a non-empty trimmed string", name)
	}
	return nil
}

func validateOptionalNonBlank(name string, value *string) error {
	if value != nil && strings.TrimSpace(*value) == "" {
		return fmt.Errorf("state: %s must be null or a non-empty string", name)
	}
	return nil
}

func validateOptionalRelativePath(name string, value *string) error {
	if err := validateOptionalString(name, value); err != nil || value == nil {
		return err
	}
	cleaned := filepath.Clean(*value)
	if filepath.IsAbs(*value) || cleaned != *value || cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("state: %s must be a clean run-relative path", name)
	}
	return nil
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func clonePendingAction(action PendingAction) PendingAction {
	action.TaskID = cloneString(action.TaskID)
	action.Response = cloneString(action.Response)
	return action
}
