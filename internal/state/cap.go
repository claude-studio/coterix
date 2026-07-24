package state

import (
	"fmt"
	"math"
)

// CapScope identifies which persisted counter reached its configured limit.
type CapScope string

const (
	CapPlan CapScope = "plan"
	CapTask CapScope = "task"
)

// CapError reports a pre-start cap check without mutating the counter.
type CapError struct {
	Scope   CapScope
	TaskID  string
	Current int
	Maximum int
}

func (failure *CapError) Error() string {
	if failure.Scope == CapTask {
		return fmt.Sprintf(
			"state: task %s attempt cap reached (%d >= %d)",
			failure.TaskID,
			failure.Current,
			failure.Maximum,
		)
	}
	return fmt.Sprintf(
		"state: plan round cap reached (%d >= %d)",
		failure.Current,
		failure.Maximum,
	)
}

// OverrideReason identifies the human action that granted a one-shot start.
type OverrideReason string

const (
	OverridePlanCap    OverrideReason = "plan_cap"
	OverridePlanReject OverrideReason = "plan_reject"
	OverrideTaskRetry  OverrideReason = "task_cap_retry"
)

type overrideScope uint8

const (
	overrideScopePlan overrideScope = iota + 1
	overrideScopeTask
)

// OneShotOverride is an opaque, scope-bound permission for the immediately
// following start. It is deliberately not part of state.json.
type OneShotOverride struct {
	scope           overrideScope
	reason          OverrideReason
	taskID          string
	owner           *State
	expectedCounter int
	grant           *overrideGrant
}

type overrideGrant struct {
	consumed bool
}

// Reason reports which human action granted the override.
func (override *OneShotOverride) Reason() OverrideReason {
	if override == nil {
		return ""
	}
	return override.reason
}

// Consumed reports whether a Begin call has already used the override.
func (override *OneShotOverride) Consumed() bool {
	return override != nil && override.grant != nil && override.grant.consumed
}

// BeginPlanRound performs the sole plan cap check immediately before a new
// round and increments the counter only when the round may start.
func (current *State) BeginPlanRound(
	maxPlanRounds int,
	override *OneShotOverride,
) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if current.Phase != PhasePlanning {
		return fmt.Errorf("state: a plan round can start only while planning")
	}
	if maxPlanRounds <= 0 {
		return fmt.Errorf("state: max plan rounds must be positive")
	}

	hasOverride := override != nil
	if hasOverride {
		if err := override.validatePlan(current); err != nil {
			return err
		}
	}
	if current.PlanRound >= maxPlanRounds && !hasOverride {
		return &CapError{
			Scope:   CapPlan,
			Current: current.PlanRound,
			Maximum: maxPlanRounds,
		}
	}
	if current.PlanRound == math.MaxInt {
		return fmt.Errorf("state: plan_round cannot be incremented")
	}
	if hasOverride {
		override.consume()
	}
	current.PlanRound++
	return nil
}

// BeginTaskAttempt performs the sole task cap check immediately before a new
// impl/fix attempt and increments the counter only when it may start.
func (current *State) BeginTaskAttempt(
	taskID string,
	maxTaskAttempts int,
	override *OneShotOverride,
) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if current.Phase != PhaseImplementing {
		return fmt.Errorf("state: a task attempt can start only while implementing")
	}
	if current.CurrentTaskID == nil || *current.CurrentTaskID != taskID {
		return fmt.Errorf("state: task %q is not the current task", taskID)
	}
	if maxTaskAttempts <= 0 {
		return fmt.Errorf("state: max task attempts must be positive")
	}
	task, exists := current.Tasks[taskID]
	if !exists || task == nil {
		return fmt.Errorf("state: task %q does not exist", taskID)
	}
	if task.Status != TaskOpen && task.Status != TaskRepairing {
		return fmt.Errorf(
			"state: task %q cannot start an attempt from status %s",
			taskID,
			task.Status,
		)
	}

	hasOverride := override != nil
	if hasOverride {
		if err := override.validateTask(current, taskID, task.Attempt); err != nil {
			return err
		}
	}
	if task.Attempt >= maxTaskAttempts && !hasOverride {
		return &CapError{
			Scope:   CapTask,
			TaskID:  taskID,
			Current: task.Attempt,
			Maximum: maxTaskAttempts,
		}
	}
	if task.Attempt == math.MaxInt {
		return fmt.Errorf("state: task %q attempt cannot be incremented", taskID)
	}
	if hasOverride {
		override.consume()
	}
	task.Attempt++
	return nil
}

func newPlanOverride(owner *State, reason OverrideReason) *OneShotOverride {
	return &OneShotOverride{
		scope:           overrideScopePlan,
		reason:          reason,
		owner:           owner,
		expectedCounter: owner.PlanRound,
		grant:           &overrideGrant{},
	}
}

func newTaskOverride(owner *State, taskID string) *OneShotOverride {
	return &OneShotOverride{
		scope:           overrideScopeTask,
		reason:          OverrideTaskRetry,
		taskID:          taskID,
		owner:           owner,
		expectedCounter: owner.Tasks[taskID].Attempt,
		grant:           &overrideGrant{},
	}
}

func (override *OneShotOverride) validatePlan(owner *State) error {
	switch {
	case override.grant == nil:
		return fmt.Errorf("state: invalid one-shot override")
	case override.grant.consumed:
		return fmt.Errorf("state: one-shot override was already consumed")
	case override.scope != overrideScopePlan:
		return fmt.Errorf("state: task override cannot start a plan round")
	case override.reason != OverridePlanCap && override.reason != OverridePlanReject:
		return fmt.Errorf("state: invalid plan override reason %q", override.reason)
	case override.owner != owner:
		return fmt.Errorf("state: one-shot override belongs to another run state")
	case override.expectedCounter != owner.PlanRound:
		return fmt.Errorf(
			"state: plan override is stale (expected round %d, got %d)",
			override.expectedCounter,
			owner.PlanRound,
		)
	default:
		return nil
	}
}

func (override *OneShotOverride) validateTask(
	owner *State,
	taskID string,
	attempt int,
) error {
	switch {
	case override.grant == nil:
		return fmt.Errorf("state: invalid one-shot override")
	case override.grant.consumed:
		return fmt.Errorf("state: one-shot override was already consumed")
	case override.scope != overrideScopeTask:
		return fmt.Errorf("state: plan override cannot start a task attempt")
	case override.reason != OverrideTaskRetry:
		return fmt.Errorf("state: invalid task override reason %q", override.reason)
	case override.owner != owner:
		return fmt.Errorf("state: one-shot override belongs to another run state")
	case override.taskID != taskID:
		return fmt.Errorf(
			"state: task override for %q cannot start task %q",
			override.taskID,
			taskID,
		)
	case override.expectedCounter != attempt:
		return fmt.Errorf(
			"state: task override is stale (expected attempt %d, got %d)",
			override.expectedCounter,
			attempt,
		)
	default:
		return nil
	}
}

func (override *OneShotOverride) consume() {
	override.grant.consumed = true
}
