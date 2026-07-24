package state

import (
	"reflect"
	"testing"
)

func TestPhaseTransitionsAndRejectOverride(t *testing.T) {
	current := New()
	if err := current.TransitionPhase(PhaseDone); err == nil {
		t.Fatal("TransitionPhase() allowed planning -> done")
	}
	if current.Phase != PhasePlanning {
		t.Fatalf("invalid transition changed phase to %s", current.Phase)
	}

	current.PlanRound = 1
	if err := current.TransitionPhase(PhaseAwaitingApproval); err != nil {
		t.Fatalf("planning -> awaiting approval: %v", err)
	}
	if err := current.TransitionPhase(PhasePlanning); err == nil {
		t.Fatal("TransitionPhase() bypassed RejectPlan")
	}
	rejection, err := current.RejectPlan("revise the acceptance criteria")
	if err != nil {
		t.Fatalf("RejectPlan() error = %v", err)
	}
	if current.Phase != PhasePlanning || current.PlanRound != 1 {
		t.Fatalf("rejected state = %#v", current)
	}
	if rejection.Feedback != "revise the acceptance criteria" ||
		rejection.Override == nil ||
		rejection.Override.Reason() != OverridePlanReject {
		t.Fatalf("RejectPlan() = %#v", rejection)
	}
	if err := current.BeginPlanRound(1, rejection.Override); err != nil {
		t.Fatalf("rejected revision BeginPlanRound() error = %v", err)
	}
	if current.PlanRound != 2 || !rejection.Override.Consumed() {
		t.Fatalf(
			"rejected revision round=%d consumed=%v",
			current.PlanRound,
			rejection.Override.Consumed(),
		)
	}

	if err := current.TransitionPhase(PhaseAwaitingApproval); err != nil {
		t.Fatalf("planning -> awaiting approval: %v", err)
	}
	if err := current.TransitionPhase(PhaseImplementing); err != nil {
		t.Fatalf("awaiting approval -> implementing: %v", err)
	}
	if err := current.TransitionPhase(PhaseDone); err != nil {
		t.Fatalf("implementing -> done: %v", err)
	}
	if err := current.TransitionPhase(PhaseFailed); err == nil {
		t.Fatal("TransitionPhase() allowed a terminal transition")
	}
}

func TestPauseAndResumePlanQuestion(t *testing.T) {
	current := New()
	if err := current.PauseForPlanQuestion("Which target should the plan use?"); err != nil {
		t.Fatalf("PauseForPlanQuestion() error = %v", err)
	}
	if current.Phase != PhasePausedForInput ||
		current.PendingAction == nil ||
		current.PendingAction.Kind != PendingPlanQuestion {
		t.Fatalf("paused state = %#v", current)
	}
	if _, err := current.ResumePending(nil); err == nil {
		t.Fatal("ResumePending() accepted a missing response")
	}
	if current.Phase != PhasePausedForInput || current.PendingAction == nil {
		t.Fatal("failed resume mutated paused state")
	}

	response := "Use the repository root.\n"
	result, err := current.ResumePending(&response)
	if err != nil {
		t.Fatalf("ResumePending() error = %v", err)
	}
	if current.Phase != PhasePlanning || current.PendingAction != nil {
		t.Fatalf("resumed state = %#v", current)
	}
	if result.Action.Response == nil || *result.Action.Response != response {
		t.Fatalf("resume response = %#v", result.Action.Response)
	}
	if result.Override != nil || result.Aborted {
		t.Fatalf("plan question result = %#v", result)
	}
}

func TestAuthResumeForbidsResponse(t *testing.T) {
	current := New()
	if err := current.PauseForAuth(nil, "Log in to Claude"); err != nil {
		t.Fatalf("PauseForAuth() error = %v", err)
	}
	response := ""
	if _, err := current.ResumePending(&response); err == nil {
		t.Fatal("ResumePending() accepted an auth response")
	}
	if current.Phase != PhasePausedForInput || current.PendingAction == nil {
		t.Fatal("failed auth resume mutated state")
	}

	result, err := current.ResumePending(nil)
	if err != nil {
		t.Fatalf("response-free ResumePending() error = %v", err)
	}
	if current.Phase != PhasePlanning ||
		current.PendingAction != nil ||
		result.Action.Response != nil ||
		result.Override != nil {
		t.Fatalf("auth resume result=%#v state=%#v", result, current)
	}
}

func TestPendingActionRejectsInvalidKindCombinations(t *testing.T) {
	tests := []struct {
		name   string
		action PendingAction
	}{
		{
			name: "plan question resumes implementation",
			action: PendingAction{
				Kind:        PendingPlanQuestion,
				ResumePhase: PhaseImplementing,
				Prompt:      "question",
			},
		},
		{
			name: "plan cap has task",
			action: PendingAction{
				Kind:        PendingPlanCap,
				ResumePhase: PhasePlanning,
				TaskID:      testString("T1"),
				Prompt:      "cap",
			},
		},
		{
			name: "task cap lacks task",
			action: PendingAction{
				Kind:        PendingTaskCap,
				ResumePhase: PhaseImplementing,
				Prompt:      "cap",
			},
		},
		{
			name: "auth contains response",
			action: PendingAction{
				Kind:        PendingAuth,
				ResumePhase: PhasePlanning,
				Prompt:      "log in",
				Response:    testString("done"),
			},
		},
		{
			name: "empty prompt",
			action: PendingAction{
				Kind:        PendingAuth,
				ResumePhase: PhasePlanning,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.action.Validate(); err == nil {
				t.Fatal("PendingAction.Validate() accepted an invalid action")
			}
		})
	}
}

func TestTaskCapPauseRequiresAttemptableTaskStatus(t *testing.T) {
	for _, status := range []TaskStatus{TaskOpen, TaskRepairing} {
		t.Run(string(status), func(t *testing.T) {
			current := newImplementingTestState("T1", status, 1)
			if err := current.PauseForTaskCap("T1", "retry or abort"); err != nil {
				t.Fatalf("PauseForTaskCap() error = %v", err)
			}
		})
	}

	candidate := newImplementingTestState("T1", TaskCandidate, 1)
	if err := candidate.PauseForTaskCap("T1", "retry or abort"); err == nil {
		t.Fatal("PauseForTaskCap() accepted candidate status")
	}
	if candidate.Phase != PhaseImplementing || candidate.PendingAction != nil {
		t.Fatal("rejected task cap pause mutated state")
	}
}

func TestTaskStatusTransitions(t *testing.T) {
	current := newImplementingTestState("T1", TaskOpen, 0)
	sequence := []TaskStatus{
		TaskCandidate,
		TaskRepairing,
		TaskCandidate,
		TaskConfirmed,
	}
	for _, status := range sequence {
		if err := current.TransitionTask("T1", status); err != nil {
			t.Fatalf("TransitionTask(%s) error = %v", status, err)
		}
	}
	if current.Tasks["T1"].Status != TaskConfirmed {
		t.Fatalf("task status = %s", current.Tasks["T1"].Status)
	}
	if err := current.TransitionTask("T1", TaskFailed); err == nil {
		t.Fatal("TransitionTask() allowed confirmed -> failed")
	}
	if current.Tasks["T1"].Status != TaskConfirmed {
		t.Fatal("invalid transition mutated task status")
	}

	failed := newImplementingTestState("T1", TaskOpen, 0)
	if err := failed.TransitionTask("T1", TaskFailed); err != nil {
		t.Fatalf("open -> failed: %v", err)
	}
	if err := failed.TransitionTask("T1", TaskCandidate); err == nil {
		t.Fatal("TransitionTask() allowed transition from failed")
	}
}

func TestDoneRequiresEveryTaskConfirmed(t *testing.T) {
	current := newImplementingTestState("T1", TaskConfirmed, 1)
	addTestTask(current, "T2", TaskOpen, 0)
	current.CurrentTaskID = testString("T2")
	if err := current.TransitionPhase(PhaseDone); err == nil {
		t.Fatal("TransitionPhase() allowed done with an open task")
	}
	if current.Phase != PhaseImplementing {
		t.Fatal("rejected done transition changed phase")
	}

	current.Tasks["T2"].Status = TaskConfirmed
	if err := current.TransitionPhase(PhaseDone); err != nil {
		t.Fatalf("TransitionPhase(done) error = %v", err)
	}
	if err := current.Validate(); err != nil {
		t.Fatalf("done state validation error = %v", err)
	}

	current.Tasks["T1"].Status = TaskFailed
	if err := current.Validate(); err == nil {
		t.Fatal("Validate() accepted a loaded done state with a failed task")
	}
}

func TestFailClearsPendingActionAndRecordsError(t *testing.T) {
	current := New()
	if err := current.PauseForPlanCap("planning stopped"); err != nil {
		t.Fatal(err)
	}
	if err := current.Fail("unrecoverable parse failure"); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	if current.Phase != PhaseFailed ||
		current.PendingAction != nil ||
		current.LastError == nil ||
		*current.LastError != "unrecoverable parse failure" {
		t.Fatalf("failed state = %#v", current)
	}
	before := *current
	if err := current.Fail("again"); err == nil {
		t.Fatal("Fail() allowed transition from failed")
	}
	if !reflect.DeepEqual(*current, before) {
		t.Fatal("invalid Fail() mutated terminal state")
	}
}

func newImplementingTestState(
	taskID string,
	status TaskStatus,
	attempt int,
) *State {
	current := New()
	current.Phase = PhaseImplementing
	addTestTask(current, taskID, status, attempt)
	return current
}
