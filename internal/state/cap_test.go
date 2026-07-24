package state

import (
	"errors"
	"testing"
)

func TestPlanCapIsCheckedOnlyBeforeStartingRound(t *testing.T) {
	current := New()
	current.PlanRound = 1
	if err := current.BeginPlanRound(2, nil); err != nil {
		t.Fatalf("last allowed BeginPlanRound() error = %v", err)
	}
	if current.PlanRound != 2 {
		t.Fatalf("plan_round = %d, want 2", current.PlanRound)
	}

	// The result of the last allowed round is evaluated directly. There is no
	// post-result cap check that can steal this successful transition.
	if err := current.TransitionPhase(PhaseAwaitingApproval); err != nil {
		t.Fatalf("last allowed round could not succeed: %v", err)
	}

	capped := New()
	capped.PlanRound = 2
	err := capped.BeginPlanRound(2, nil)
	var capErr *CapError
	if !errors.As(err, &capErr) {
		t.Fatalf("BeginPlanRound() error = %T %v, want *CapError", err, err)
	}
	if capErr.Scope != CapPlan || capErr.Current != 2 || capErr.Maximum != 2 {
		t.Fatalf("CapError = %#v", capErr)
	}
	if capped.PlanRound != 2 {
		t.Fatal("cap failure changed plan_round")
	}
}

func TestPlanCapResumeOverrideIsConsumedOnceWithoutReset(t *testing.T) {
	current := New()
	current.PlanRound = 2
	if err := current.PauseForPlanCap("Provide feedback to retry once"); err != nil {
		t.Fatalf("PauseForPlanCap() error = %v", err)
	}
	response := "Try a smaller task split."
	resumed, err := current.ResumePending(&response)
	if err != nil {
		t.Fatalf("ResumePending() error = %v", err)
	}
	if resumed.Override == nil || resumed.Override.Reason() != OverridePlanCap {
		t.Fatalf("resume override = %#v", resumed.Override)
	}
	if current.PlanRound != 2 {
		t.Fatal("resume reset plan_round")
	}

	if err := current.BeginPlanRound(2, resumed.Override); err != nil {
		t.Fatalf("over-cap BeginPlanRound() error = %v", err)
	}
	if current.PlanRound != 3 || !resumed.Override.Consumed() {
		t.Fatalf(
			"plan_round=%d consumed=%v",
			current.PlanRound,
			resumed.Override.Consumed(),
		)
	}
	if err := current.BeginPlanRound(2, resumed.Override); err == nil {
		t.Fatal("BeginPlanRound() reused a one-shot override")
	}
	if current.PlanRound != 3 {
		t.Fatal("override reuse changed plan_round")
	}
}

func TestRejectOverrideIsConsumedByImmediateBelowCapRound(t *testing.T) {
	current := New()
	if err := current.TransitionPhase(PhaseAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	rejected, err := current.RejectPlan("revise wording")
	if err != nil {
		t.Fatalf("RejectPlan() error = %v", err)
	}
	if err := current.BeginPlanRound(5, rejected.Override); err != nil {
		t.Fatalf("BeginPlanRound() error = %v", err)
	}
	if current.PlanRound != 1 || !rejected.Override.Consumed() {
		t.Fatalf(
			"plan_round=%d consumed=%v",
			current.PlanRound,
			rejected.Override.Consumed(),
		)
	}
	if err := current.BeginPlanRound(5, rejected.Override); err == nil {
		t.Fatal("below-cap reject override was saved for later reuse")
	}
	if current.PlanRound != 1 {
		t.Fatal("reused reject override changed plan_round")
	}
}

func TestPlanOverrideIsBoundToOwnerAndImmediateCounter(t *testing.T) {
	owner := New()
	if err := owner.TransitionPhase(PhaseAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	rejected, err := owner.RejectPlan("revise")
	if err != nil {
		t.Fatal(err)
	}

	other := New()
	if err := other.BeginPlanRound(1, rejected.Override); err == nil {
		t.Fatal("BeginPlanRound() accepted another state's override")
	}
	if other.PlanRound != 0 || rejected.Override.Consumed() {
		t.Fatal("cross-state use changed counter or consumed override")
	}

	if err := owner.BeginPlanRound(2, nil); err != nil {
		t.Fatal(err)
	}
	if err := owner.BeginPlanRound(2, rejected.Override); err == nil {
		t.Fatal("BeginPlanRound() accepted a banked stale override")
	}
	if owner.PlanRound != 1 || rejected.Override.Consumed() {
		t.Fatal("stale use changed counter or consumed override")
	}
}

func TestTaskCapIsCheckedOnlyBeforeStartingAttempt(t *testing.T) {
	current := newImplementingTestState("T1", TaskOpen, 1)
	if err := current.BeginTaskAttempt("T1", 2, nil); err != nil {
		t.Fatalf("last allowed BeginTaskAttempt() error = %v", err)
	}
	if current.Tasks["T1"].Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", current.Tasks["T1"].Attempt)
	}
	if err := current.TransitionTask("T1", TaskCandidate); err != nil {
		t.Fatal(err)
	}
	if err := current.TransitionTask("T1", TaskConfirmed); err != nil {
		t.Fatalf("last allowed attempt could not be confirmed: %v", err)
	}

	capped := newImplementingTestState("T1", TaskRepairing, 2)
	err := capped.BeginTaskAttempt("T1", 2, nil)
	var capErr *CapError
	if !errors.As(err, &capErr) {
		t.Fatalf("BeginTaskAttempt() error = %T %v, want *CapError", err, err)
	}
	if capErr.Scope != CapTask ||
		capErr.TaskID != "T1" ||
		capErr.Current != 2 ||
		capErr.Maximum != 2 {
		t.Fatalf("CapError = %#v", capErr)
	}
	if capped.Tasks["T1"].Attempt != 2 {
		t.Fatal("cap failure changed task attempt")
	}
}

func TestTaskCapRetryOverrideIsTaskBoundAndConsumedOnce(t *testing.T) {
	current := newImplementingTestState("T1", TaskRepairing, 2)
	addTestTask(current, "T2", TaskRepairing, 2)
	current.CurrentTaskID = testString("T1")
	if err := current.PauseForTaskCap("T1", "retry or abort"); err != nil {
		t.Fatalf("PauseForTaskCap() error = %v", err)
	}
	response := "retry"
	resumed, err := current.ResumePending(&response)
	if err != nil {
		t.Fatalf("ResumePending() error = %v", err)
	}
	if resumed.Override == nil || resumed.Override.Reason() != OverrideTaskRetry {
		t.Fatalf("task retry override = %#v", resumed.Override)
	}
	if current.Tasks["T1"].Attempt != 2 {
		t.Fatal("task retry reset attempt counter")
	}

	current.CurrentTaskID = testString("T2")
	if err := current.BeginTaskAttempt("T2", 2, resumed.Override); err == nil {
		t.Fatal("BeginTaskAttempt() accepted an override for another task")
	}
	if resumed.Override.Consumed() || current.Tasks["T2"].Attempt != 2 {
		t.Fatal("wrong-task use consumed override or changed counter")
	}

	current.CurrentTaskID = testString("T1")
	if err := current.BeginTaskAttempt("T1", 2, resumed.Override); err != nil {
		t.Fatalf("over-cap BeginTaskAttempt() error = %v", err)
	}
	if current.Tasks["T1"].Attempt != 3 || !resumed.Override.Consumed() {
		t.Fatalf(
			"attempt=%d consumed=%v",
			current.Tasks["T1"].Attempt,
			resumed.Override.Consumed(),
		)
	}
	if err := current.BeginTaskAttempt("T1", 2, resumed.Override); err == nil {
		t.Fatal("BeginTaskAttempt() reused a one-shot override")
	}
	if current.Tasks["T1"].Attempt != 3 {
		t.Fatal("override reuse changed attempt")
	}
}

func TestTaskOverrideIsBoundToOwnerAndImmediateAttempt(t *testing.T) {
	owner := newImplementingTestState("T1", TaskRepairing, 1)
	if err := owner.PauseForTaskCap("T1", "retry or abort"); err != nil {
		t.Fatal(err)
	}
	retry := "retry"
	resumed, err := owner.ResumePending(&retry)
	if err != nil {
		t.Fatal(err)
	}

	other := newImplementingTestState("T1", TaskRepairing, 1)
	if err := other.BeginTaskAttempt("T1", 1, resumed.Override); err == nil {
		t.Fatal("BeginTaskAttempt() accepted another state's override")
	}
	if other.Tasks["T1"].Attempt != 1 || resumed.Override.Consumed() {
		t.Fatal("cross-state use changed counter or consumed override")
	}

	if err := owner.BeginTaskAttempt("T1", 2, nil); err != nil {
		t.Fatal(err)
	}
	if err := owner.BeginTaskAttempt("T1", 2, resumed.Override); err == nil {
		t.Fatal("BeginTaskAttempt() accepted a banked stale override")
	}
	if owner.Tasks["T1"].Attempt != 2 || resumed.Override.Consumed() {
		t.Fatal("stale use changed counter or consumed override")
	}
}

func TestTaskCapAbortFailsTaskWithoutIncrement(t *testing.T) {
	current := newImplementingTestState("T1", TaskRepairing, 2)
	if err := current.PauseForTaskCap("T1", "retry or abort"); err != nil {
		t.Fatal(err)
	}
	badResponse := "Retry"
	if _, err := current.ResumePending(&badResponse); err == nil {
		t.Fatal("ResumePending() accepted a non-exact task cap response")
	}
	if current.Phase != PhasePausedForInput ||
		current.Tasks["T1"].Status != TaskRepairing {
		t.Fatal("invalid task cap response mutated state")
	}

	response := "abort"
	result, err := current.ResumePending(&response)
	if err != nil {
		t.Fatalf("ResumePending(abort) error = %v", err)
	}
	if !result.Aborted || result.Override != nil {
		t.Fatalf("abort result = %#v", result)
	}
	if current.Phase != PhaseFailed ||
		current.Tasks["T1"].Status != TaskFailed ||
		current.Tasks["T1"].Attempt != 2 ||
		current.PendingAction != nil {
		t.Fatalf("aborted state = %#v", current)
	}
}

func TestOverridesRejectWrongScopeWithoutMutation(t *testing.T) {
	taskState := newImplementingTestState("T1", TaskRepairing, 1)
	if err := taskState.PauseForTaskCap("T1", "retry or abort"); err != nil {
		t.Fatal(err)
	}
	retry := "retry"
	taskResume, err := taskState.ResumePending(&retry)
	if err != nil {
		t.Fatal(err)
	}

	planState := New()
	if err := planState.BeginPlanRound(2, taskResume.Override); err == nil {
		t.Fatal("BeginPlanRound() accepted a task override")
	}
	if planState.PlanRound != 0 || taskResume.Override.Consumed() {
		t.Fatal("wrong-scope plan use mutated state or override")
	}

	if err := planState.TransitionPhase(PhaseAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	rejected, err := planState.RejectPlan("try again")
	if err != nil {
		t.Fatal(err)
	}
	if err := taskState.BeginTaskAttempt("T1", 2, rejected.Override); err == nil {
		t.Fatal("BeginTaskAttempt() accepted a plan override")
	}
	if taskState.Tasks["T1"].Attempt != 1 || rejected.Override.Consumed() {
		t.Fatal("wrong-scope task use mutated state or override")
	}
}
