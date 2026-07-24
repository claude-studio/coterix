package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

func TestRenderSnapshotTableKeepsArgvSelectedShape(t *testing.T) {
	statuses := []pipeline.RunStatus{
		snapshotTestStatus("run-table-1", state.PhaseImplementing),
		snapshotTestStatus("run-table-2", state.PhaseAwaitingApproval),
	}

	rendered, err := RenderSnapshot(
		statuses,
		180,
		SnapshotPresentationTable,
	)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(rendered)
	for _, expected := range []string{
		"run_id",
		"phase",
		"plan_round",
		"confirmed",
		"current_task",
		"task_status",
		"attempt",
		"gate",
		"review",
		"pending/approval",
		"run-table-1",
		"run-table-2",
		"1/2",
		"✓",
		"APPROVAL NEEDED",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("table snapshot lacks %q:\n%s", expected, plain)
		}
	}
	if strings.Contains(plain, "╱╱╱ RUN") {
		t.Fatalf("table presentation changed to detail by cardinality:\n%s", plain)
	}

	oneRun, err := RenderSnapshot(
		statuses[:1],
		180,
		SnapshotPresentationTable,
	)
	if err != nil {
		t.Fatal(err)
	}
	if plain := ansi.Strip(oneRun); !strings.Contains(plain, "run_id") ||
		strings.Contains(plain, "╱╱╱ RUN") {
		t.Fatalf("one-row table changed presentation:\n%s", plain)
	}

	empty, err := RenderSnapshot(
		nil,
		180,
		SnapshotPresentationTable,
	)
	if err != nil {
		t.Fatal(err)
	}
	if plain := ansi.Strip(empty); !strings.Contains(plain, "run_id") ||
		!strings.Contains(plain, "no runs") {
		t.Fatalf("empty status lacks table-shaped empty state:\n%s", plain)
	}
}

func TestRenderSnapshotDetailShowsStateWithoutActionKeys(t *testing.T) {
	lastError := "candidate verification failed"
	status := snapshotTestStatus("run-detail", state.PhaseAwaitingApproval)
	status.LastError = &lastError

	rendered, err := RenderSnapshot(
		[]pipeline.RunStatus{status},
		100,
		SnapshotPresentationDetail,
	)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(rendered)
	for _, expected := range []string{
		"╱╱╱ RUN",
		"run-detail",
		"awaiting_approval",
		"human_gate → operator",
		"current: T2",
		"status: confirmed",
		"attempt: 2",
		"1/2",
		"✓",
		"APPROVAL NEEDED",
		lastError,
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("detail snapshot lacks %q:\n%s", expected, plain)
		}
	}
	if strings.Contains(plain, "a approve") ||
		strings.Contains(plain, "r reject") {
		t.Fatalf("one-shot snapshot exposed action keys:\n%s", plain)
	}

	current := populatedViewModel(t)
	dashboard := ansi.Strip(renderSidebar(
		current,
		sidebarWidth,
		wideBreakpointHeight,
	))
	if !strings.Contains(dashboard, "a approve · r reject") {
		t.Fatalf("dashboard lost its interactive action hint:\n%s", dashboard)
	}
}

func TestRenderSnapshotDetailShowsPendingPrompt(t *testing.T) {
	status := snapshotTestStatus("run-pending", state.PhasePausedForInput)
	status.PendingAction = &state.PendingAction{
		Kind:        state.PendingTaskCap,
		ResumePhase: state.PhaseImplementing,
		TaskID:      pointerTo("T2"),
		Prompt:      "retry or abort",
	}

	rendered, err := RenderSnapshot(
		[]pipeline.RunStatus{status},
		100,
		SnapshotPresentationDetail,
	)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(rendered)
	for _, expected := range []string{
		"paused_for_input",
		"pending_action → operator",
		"task_cap",
		"retry or abort",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("pending snapshot lacks %q:\n%s", expected, plain)
		}
	}
}

func TestRenderSnapshotBannerFallsBackByCellWidth(t *testing.T) {
	currentTheme, err := loadTheme()
	if err != nil {
		t.Fatal(err)
	}

	wide := renderSnapshotBanner(currentTheme, 120)
	if plain := ansi.Strip(wide); !strings.Contains(
		plain,
		snapshotBannerRows[0],
	) {
		t.Fatalf("wide banner lacks block row:\n%s", plain)
	}
	firstRow := strings.Split(wide, "\n")[0]
	brand := currentTheme.tokens.Gradient.BrandLeftToRight
	assertSameColor(
		t,
		styledCellAt(t, firstRow, 0, 0).Style.Fg,
		lipgloss.Color(brand[0]),
	)
	assertSameColor(
		t,
		styledCellAt(
			t,
			firstRow,
			ansi.StringWidth(firstRow)-1,
			0,
		).Style.Fg,
		lipgloss.Color(brand[len(brand)-1]),
	)

	narrowWidth := ansi.StringWidth(snapshotBannerRows[0]) - 1
	narrow := renderSnapshotBanner(currentTheme, narrowWidth)
	if plain := ansi.Strip(narrow); plain != "COTERIX" {
		t.Fatalf("narrow banner=%q, want compact COTERIX", plain)
	}
	if ansi.StringWidth(narrow) > narrowWidth {
		t.Fatalf(
			"narrow banner width=%d exceeds %d",
			ansi.StringWidth(narrow),
			narrowWidth,
		)
	}
}

func TestRenderSnapshotTableConstrainsLongFieldsToWidth(t *testing.T) {
	const width = 64
	status := snapshotTestStatus(
		"run-"+strings.Repeat("very-long-identifier-", 8),
		state.PhasePausedForInput,
	)
	currentTaskID := "task-" + strings.Repeat("wide-value-", 8)
	status.CurrentTaskID = &currentTaskID
	status.Tasks[currentTaskID] = state.TaskState{
		Status:  state.TaskRepairing,
		Attempt: 12,
	}
	status.PendingAction = &state.PendingAction{
		Kind:        state.PendingTaskCap,
		ResumePhase: state.PhaseImplementing,
		TaskID:      &currentTaskID,
		Prompt: "retry after reviewing " + strings.Repeat("evidence ", 16) +
			"\nwithout adding a physical table row",
	}

	rendered, err := RenderSnapshot(
		[]pipeline.RunStatus{status},
		width,
		SnapshotPresentationTable,
	)
	if err != nil {
		t.Fatal(err)
	}
	for index, line := range strings.Split(rendered, "\n") {
		if lineWidth := ansi.StringWidth(line); lineWidth > width {
			t.Fatalf(
				"snapshot line %d width=%d exceeds %d:\n%s",
				index,
				lineWidth,
				width,
				ansi.Strip(rendered),
			)
		}
	}
	if !strings.Contains(ansi.Strip(rendered), "…") {
		t.Fatalf("long table fields were not truncated:\n%s", ansi.Strip(rendered))
	}
	if got := snapshotCell("first\nsecond\tthird"); got != "first second third" {
		t.Fatalf("snapshotCell()=%q", got)
	}

	currentTheme, err := loadTheme()
	if err != nil {
		t.Fatal(err)
	}
	tableOnly := renderSnapshotTable(
		currentTheme,
		[]pipeline.RunStatus{status},
		width,
	)
	tableLines := strings.Split(tableOnly, "\n")
	wantEndings := []string{"┐", "│", "┤", "│", "┘"}
	if len(tableLines) != len(wantEndings) {
		t.Fatalf(
			"table physical lines=%d, want %d:\n%s",
			len(tableLines),
			len(wantEndings),
			ansi.Strip(tableOnly),
		)
	}
	for index, ending := range wantEndings {
		plainLine := ansi.Strip(tableLines[index])
		if ansi.StringWidth(tableLines[index]) != width ||
			!strings.HasSuffix(plainLine, ending) {
			t.Fatalf(
				"table line %d lost Width/Wrap border: width=%d ending=%q:\n%s",
				index,
				ansi.StringWidth(tableLines[index]),
				ending,
				ansi.Strip(tableOnly),
			)
		}
	}
}

func TestDeriveStatusFieldsUsesStateOnlyEvidence(t *testing.T) {
	confirmed := deriveStatusFields(
		snapshotTestStatus("run-confirmed", state.PhaseDone),
	)
	if confirmed.RunID != "run-confirmed" ||
		confirmed.TaskID != "T2" ||
		confirmed.TaskStatus != state.TaskConfirmed ||
		confirmed.Attempt != 2 ||
		confirmed.Confirmed != 1 ||
		confirmed.Total != 2 ||
		confirmed.Gate != evidencePass ||
		confirmed.Review != evidencePass {
		t.Fatalf("confirmed status fields=%#v", confirmed)
	}

	candidateStatus := snapshotTestStatus(
		"run-candidate",
		state.PhaseImplementing,
	)
	candidateStatus.Tasks["T2"] = state.TaskState{
		Status:       state.TaskCandidate,
		Attempt:      2,
		GateResult:   pointerTo("runs/run-candidate/tasks/T2/gate.json"),
		ReviewResult: pointerTo("runs/run-candidate/tasks/T2/review.json"),
	}
	candidate := deriveStatusFields(candidateStatus)
	if candidate.Gate != evidenceUnknown ||
		candidate.Review != evidenceUnknown {
		t.Fatalf("candidate evidence was inferred: %#v", candidate)
	}
}

func snapshotTestStatus(
	runID string,
	phase state.Phase,
) pipeline.RunStatus {
	currentTaskID := "T2"
	return pipeline.RunStatus{
		RunID:         runID,
		Phase:         phase,
		PlanRound:     3,
		TaskOrder:     []string{"T1", "T2"},
		CurrentTaskID: &currentTaskID,
		Tasks: map[string]state.TaskState{
			"T1": {
				Status:  state.TaskOpen,
				Attempt: 0,
			},
			"T2": {
				Status:  state.TaskConfirmed,
				Attempt: 2,
			},
		},
	}
}
