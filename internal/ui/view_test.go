package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

func TestDashboardWideAndCompactLayoutBranches(t *testing.T) {
	current := populatedViewModel(t)

	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight
	wide := ansi.Strip(renderDashboard(current))
	for _, expected := range []string{
		"COTERIX",
		"PIPELINE FEED",
		"ROUTING",
		"plan_writer",
		"claude",
		"APPROVAL GATE",
	} {
		if !strings.Contains(wide, expected) {
			t.Fatalf("wide layout lacks %q:\n%s", expected, wide)
		}
	}

	current.width = wideBreakpointWidth - 1
	compact := ansi.Strip(renderDashboard(current))
	for _, expected := range []string{
		"COTERIX",
		"PIPELINE FEED",
		"gate",
		"review",
	} {
		if !strings.Contains(compact, expected) {
			t.Fatalf("compact layout lacks %q:\n%s", expected, compact)
		}
	}
	if strings.Contains(compact, "ROUTING") {
		t.Fatalf("compact layout did not fold the sidebar:\n%s", compact)
	}
	if strings.Contains(strings.ToLower(compact), "session-details") {
		t.Fatal("compact layout exposed the rejected session-details overlay")
	}
	if !isWide(120, 30) || isWide(119, 30) || isWide(120, 29) {
		t.Fatal("wide breakpoint does not match >=120x30")
	}
}

func TestSidebarDerivesStateSnapshotFields(t *testing.T) {
	current := populatedViewModel(t)
	data := deriveSidebar(current)

	if data.RunID != "run-9" ||
		data.Phase != state.PhaseAwaitingApproval ||
		data.Role != "plan_writer" ||
		data.CLI != "claude" ||
		data.TaskID != "T2" ||
		data.TaskStatus != state.TaskCandidate ||
		data.Attempt != 2 ||
		data.Gate != evidencePass ||
		data.Review != evidenceFail ||
		data.PlanRound != 3 ||
		data.Confirmed != 1 ||
		data.Total != 3 ||
		!data.AwaitingApproval ||
		data.PendingKind != "" {
		t.Fatalf("derived sidebar=%#v", data)
	}

	current.activeRole = ""
	current.activeStep = ""
	current.activeCLI = ""
	current.status.Phase = state.PhasePausedForInput
	current.status.PendingAction = &state.PendingAction{
		Kind:        state.PendingPlanQuestion,
		ResumePhase: state.PhasePlanning,
		Prompt:      "Which package?",
	}
	data = deriveSidebar(current)
	if data.AwaitingApproval ||
		data.PendingKind != state.PendingPlanQuestion ||
		data.PendingPrompt != "Which package?" ||
		data.Role != "pending_action" {
		t.Fatalf("pending action was not distinct from approval: %#v", data)
	}
}

func TestMainPaneRendersPlanDiffVerdictAndStreamingLogs(t *testing.T) {
	current := populatedViewModel(t)
	diff := "@@ file.go @@\n-old\n+new\n"
	current.artifacts = artifactData{
		PlanMarkdown: "# Plan\n\n- implement dashboard\n",
		DiffContent:  &diff,
		Verdicts: []namedVerdict{{
			Name: "review.json",
			JSON: `{"clean":true}`,
		}},
	}
	current.logs = []logEntry{{
		Role:    "impl_writer",
		CLI:     "codex",
		Attempt: 1,
		Text:    "streamed output",
	}}
	current.refreshArtifactRender()

	plain := ansi.Strip(renderMain(current, 100, 80))
	for _, expected := range []string{
		"Plan",
		"implement dashboard",
		"Current diff",
		"-old",
		"+new",
		"Verdict",
		"clean",
		"LIVE OUTPUT",
		"impl_writer→codex#1",
		"streamed output",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("main pane lacks %q:\n%s", expected, plain)
		}
	}
}

func TestStatusBarDistinguishesApprovalAndPendingAction(t *testing.T) {
	current := populatedViewModel(t)
	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	approval := ansi.Strip(renderStatusBar(current, 100, 2))
	if !strings.Contains(approval, "Approval required") ||
		!strings.Contains(approval, "a approve") {
		t.Fatalf("approval status bar=%q", approval)
	}

	current.status.Phase = state.PhasePausedForInput
	current.status.PendingAction = &state.PendingAction{
		Kind:        state.PendingTaskCap,
		ResumePhase: state.PhaseImplementing,
		TaskID:      pointerTo("T2"),
		Prompt:      "retry or abort",
	}
	pending := ansi.Strip(renderStatusBar(current, 100, 2))
	if !strings.Contains(pending, "Pending action") ||
		!strings.Contains(pending, "task_cap") ||
		strings.Contains(pending, "Approval required") {
		t.Fatalf("pending status bar=%q", pending)
	}
}

func populatedViewModel(t *testing.T) model {
	t.Helper()
	current := testModel(t, &fakeUIControl{})
	current.hasStatus = true
	current.activeStep = pipeline.StepPlan
	current.activeRole = "plan_writer"
	current.activeCLI = "claude"
	current.status = pipeline.RunStatus{
		RunID:         "run-9",
		Phase:         state.PhaseAwaitingApproval,
		PlanRound:     3,
		TaskOrder:     []string{"T1", "T2", "T3"},
		CurrentTaskID: pointerTo("T2"),
		Tasks: map[string]state.TaskState{
			"T1": {Status: state.TaskConfirmed, Attempt: 1},
			"T2": {Status: state.TaskCandidate, Attempt: 2},
			"T3": {Status: state.TaskOpen},
		},
	}
	current.artifacts.GateOutcome = evidencePass
	current.artifacts.ReviewOutcome = evidenceFail
	return current
}
