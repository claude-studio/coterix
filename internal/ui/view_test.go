package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
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
		"APPROVAL NEEDED",
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
	if !strings.Contains(approval, "APPROVAL NEEDED") ||
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
	if !strings.Contains(pending, "PENDING") ||
		!strings.Contains(pending, "task_cap") ||
		strings.Contains(pending, "APPROVAL NEEDED") {
		t.Fatalf("pending status bar=%q", pending)
	}
}

func TestWordmarkExpandsOnlyWithWideHeightBudget(t *testing.T) {
	current := populatedViewModel(t)
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight

	minimum := renderSidebar(
		current,
		sidebarWidth,
		wideBreakpointHeight-2,
	)
	if plain := ansi.Strip(minimum); !strings.Contains(plain, "COTERIX") ||
		strings.Contains(plain, expandedWordmarkRows[0]) {
		t.Fatalf("120x30 sidebar wordmark expanded:\n%s", plain)
	}

	current.height = wideBreakpointHeight + 2
	expanded := renderWordmark(current.theme, true)
	lines := strings.Split(expanded, "\n")
	if len(lines) < 2 || len(lines) > 3 {
		t.Fatalf("expanded wordmark rows=%d, want 2-3", len(lines))
	}
	for index, line := range lines {
		if width := ansi.StringWidth(line); width > 27 {
			t.Fatalf("expanded wordmark row %d width=%d, want <=27", index, width)
		}
	}
	if plain := ansi.Strip(
		renderSidebar(
			current,
			sidebarWidth,
			expandedWordmarkMinSidebarHeight,
		),
	); !strings.Contains(plain, expandedWordmarkRows[0]) ||
		strings.Contains(plain, "\n COTERIX") {
		t.Fatalf("height-rich sidebar lacks expanded wordmark:\n%s", plain)
	}
	current.prompt = promptReject
	if plain := ansi.Strip(renderSidebar(
		current,
		sidebarWidth,
		wideBreakpointHeight-2,
	)); !strings.Contains(plain, "COTERIX") ||
		strings.Contains(plain, expandedWordmarkRows[0]) {
		t.Fatalf("prompt-constrained sidebar wordmark expanded:\n%s", plain)
	}
	current.prompt = promptNone

	first := styledCellAt(t, lines[0], 0, 0)
	last := styledCellAt(t, lines[0], ansi.StringWidth(lines[0])-1, 0)
	assertSameColor(
		t,
		first.Style.Fg,
		lipgloss.Color(current.theme.tokens.Gradient.BrandLeftToRight[0]),
	)
	brand := current.theme.tokens.Gradient.BrandLeftToRight
	assertSameColor(
		t,
		last.Style.Fg,
		lipgloss.Color(brand[len(brand)-1]),
	)

	compact := ansi.Strip(renderCompactHeader(current, 80, 2))
	if !strings.Contains(compact, "COTERIX") ||
		strings.Contains(compact, expandedWordmarkRows[0]) {
		t.Fatalf("compact header wordmark expanded:\n%s", compact)
	}
}

func TestSidebarSectionAndRoleColorsUseInjectedTokens(t *testing.T) {
	current := populatedViewModel(t)
	title := renderSidebarSectionTitle(current.theme, "RUN", 26)
	if plain := ansi.Strip(title); !strings.HasPrefix(plain, "╱╱╱ RUN ─") ||
		ansi.StringWidth(title) != 26 {
		t.Fatalf("section title=%q", plain)
	}
	brand := current.theme.tokens.Gradient.BrandLeftToRight
	assertSameColor(
		t,
		styledCellAt(t, title, 0, 0).Style.Fg,
		lipgloss.Color(brand[0]),
	)
	assertSameColor(
		t,
		styledCellAt(t, title, 2, 0).Style.Fg,
		lipgloss.Color(brand[len(brand)-1]),
	)
	assertSameColor(
		t,
		styledCellAt(t, title, 4, 0).Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.Accent),
	)
	assertSameColor(
		t,
		styledCellAt(t, title, 8, 0).Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.Separator),
	)

	var grouped strings.Builder
	writeSidebarField(&grouped, current.theme, "run", "run-9", 26)
	groupCell := styledCellAt(t, grouped.String(), 0, 0)
	if groupCell.Content != "▌" {
		t.Fatalf("sidebar group prefix=%q", groupCell.Content)
	}
	assertSameColor(
		t,
		groupCell.Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.Accent),
	)

	roleCases := []struct {
		cli   string
		token string
	}{
		{
			cli:   "claude",
			token: current.theme.tokens.Palette.Claude.Shade400,
		},
		{
			cli:   "codex",
			token: current.theme.tokens.Palette.Codex.Shade400,
		},
	}
	for _, roleCase := range roleCases {
		t.Run(roleCase.cli, func(t *testing.T) {
			var route strings.Builder
			writeSidebarStyledField(
				&route,
				current.theme,
				"",
				cliRoleStyle(
					current.theme,
					roleCase.cli,
					current.theme.styles.Value,
				).Render("role → "+roleCase.cli),
				26,
			)
			assertSameColor(
				t,
				styledCellAt(t, route.String(), 2, 0).Style.Fg,
				lipgloss.Color(roleCase.token),
			)

			logLine := renderLogLine(current.theme, logEntry{
				Role: "role",
				CLI:  roleCase.cli,
				Text: "output",
			}, 60)
			assertSameColor(
				t,
				styledCellAt(t, logLine, 0, 0).Style.Fg,
				lipgloss.Color(roleCase.token),
			)
		})
	}
}

func TestPhaseDotsAndStatusChipsUseSemanticStates(t *testing.T) {
	current := populatedViewModel(t)
	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	current.operation = ""

	phaseCases := []struct {
		phase state.Phase
		fg    string
		bg    string
		label string
	}{
		{
			phase: state.PhasePlanning,
			fg:    current.theme.tokens.Status.Info.FG,
			bg:    current.theme.tokens.Status.Info.BG,
			label: "PLANNING",
		},
		{
			phase: state.PhaseAwaitingApproval,
			fg:    current.theme.tokens.Status.Warning.FG,
			bg:    current.theme.tokens.Status.Warning.BG,
			label: "APPROVAL NEEDED",
		},
		{
			phase: state.PhasePausedForInput,
			fg:    current.theme.tokens.Status.Warning.FG,
			bg:    current.theme.tokens.Status.Warning.BG,
			label: "PAUSED FOR INPUT",
		},
		{
			phase: state.PhaseImplementing,
			fg:    current.theme.tokens.Status.Info.FG,
			bg:    current.theme.tokens.Status.Info.BG,
			label: "IMPLEMENTING",
		},
		{
			phase: state.PhaseDone,
			fg:    current.theme.tokens.Status.Success.FG,
			bg:    current.theme.tokens.Status.Success.BG,
			label: "DONE",
		},
		{
			phase: state.PhaseFailed,
			fg:    current.theme.tokens.Status.Error.FG,
			bg:    current.theme.tokens.Status.Error.BG,
			label: "FAILED",
		},
	}
	for _, phaseCase := range phaseCases {
		t.Run(string(phaseCase.phase), func(t *testing.T) {
			current.status.Phase = phaseCase.phase
			current.status.PendingAction = nil
			dot := styledCellAt(t, phaseValue(current.theme, phaseCase.phase), 0, 0)
			assertSameColor(t, dot.Style.Fg, lipgloss.Color(phaseCase.fg))

			chip := statusSignal(current)
			if !strings.Contains(ansi.Strip(chip), phaseCase.label) {
				t.Fatalf("phase chip=%q", ansi.Strip(chip))
			}
			chipCell := firstVisibleStyledCell(t, chip)
			assertSameColor(t, chipCell.Style.Fg, lipgloss.Color(phaseCase.fg))
			assertSameColor(t, chipCell.Style.Bg, lipgloss.Color(phaseCase.bg))
		})
	}

	pendingCases := []state.PendingKind{
		state.PendingPlanQuestion,
		state.PendingPlanCap,
		state.PendingTaskCap,
		state.PendingAuth,
	}
	for _, kind := range pendingCases {
		t.Run(string(kind), func(t *testing.T) {
			current.status.Phase = state.PhasePausedForInput
			current.status.PendingAction = &state.PendingAction{
				Kind:        kind,
				ResumePhase: state.PhasePlanning,
				Prompt:      "human response needed",
			}
			chip := statusSignal(current)
			plain := ansi.Strip(chip)
			if !strings.Contains(plain, string(kind)) ||
				!strings.Contains(plain, strings.Fields(pendingChipText(kind))[0]) {
				t.Fatalf("pending chip=%q", plain)
			}
			cell := firstVisibleStyledCell(t, chip)
			assertSameColor(
				t,
				cell.Style.Fg,
				lipgloss.Color(current.theme.tokens.Status.Warning.FG),
			)
			assertSameColor(
				t,
				cell.Style.Bg,
				lipgloss.Color(current.theme.tokens.Status.Warning.BG),
			)
		})
	}

	current.status.PendingAction = nil
	current.status.Phase = state.PhasePlanning
	bar := renderStatusBar(current, 100, 2)
	hint := findStyledCell(t, bar, "j")
	assertSameColor(
		t,
		hint.Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.FGSubtle),
	)

	current.activeStep = pipeline.StepImplementation
	current.activeRole = "impl_writer"
	working := statusSignal(current)
	workingCell := firstVisibleStyledCell(t, working)
	assertSameColor(
		t,
		workingCell.Style.Bg,
		lipgloss.Color(current.theme.tokens.Status.Busy.BG),
	)
	workingLabel := findStyledCell(t, working, "W")
	assertSameColor(
		t,
		workingLabel.Style.Bg,
		lipgloss.Color(current.theme.tokens.Status.Busy.BG),
	)
	workingTail := styledCellAt(t, working, ansi.StringWidth(working)-1, 0)
	assertSameColor(
		t,
		workingTail.Style.Bg,
		lipgloss.Color(current.theme.tokens.Status.Busy.BG),
	)
}

func TestCanonicalHumanSignalsRemainVisibleAtMinimumLayouts(t *testing.T) {
	current := populatedViewModel(t)
	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	current.operation = ""
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight

	t.Run("awaiting approval with last error", func(t *testing.T) {
		candidate := current
		candidate.status.Phase = state.PhaseAwaitingApproval
		candidate.status.PendingAction = nil
		candidate.status.LastError = pointerTo(
			"approval-error-marker " + strings.Repeat("failure context ", 3),
		)

		wide := renderSidebar(
			candidate,
			sidebarWidth,
			wideBreakpointHeight-2,
		)
		assertVisibleSignals(
			t,
			wide,
			sidebarWidth,
			"APPROVAL NEEDED",
			"approval-error-marker",
		)
		assertRenderedHeight(t, wide, wideBreakpointHeight-2)

		candidate.width = 80
		candidate.height = 24
		compact := renderDashboard(candidate)
		assertVisibleSignals(
			t,
			compact,
			candidate.width,
			"APPROVAL NEEDED",
			"approval-error-marker",
		)
	})

	t.Run("paused pending with last error", func(t *testing.T) {
		candidate := current
		candidate.status.Phase = state.PhasePausedForInput
		candidate.status.PendingAction = &state.PendingAction{
			Kind:        state.PendingTaskCap,
			ResumePhase: state.PhaseImplementing,
			TaskID:      pointerTo("T2"),
			Prompt: "retry or abort after reviewing the pending task " +
				"and its latest evidence",
		}
		candidate.status.LastError = pointerTo(
			"pending-error-marker " + strings.Repeat("failure context ", 3),
		)

		wide := renderSidebar(
			candidate,
			sidebarWidth,
			wideBreakpointHeight-2,
		)
		assertVisibleSignals(
			t,
			wide,
			sidebarWidth,
			"task_cap",
			"retry or abort",
			"pending-error-marker",
		)
		assertRenderedHeight(t, wide, wideBreakpointHeight-2)

		candidate.width = 80
		candidate.height = 24
		compact := renderDashboard(candidate)
		assertVisibleSignals(
			t,
			compact,
			candidate.width,
			"task_cap",
			"retry or abort",
			"pending-error-marker",
		)
	})
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

func styledCellAt(t *testing.T, rendered string, x, y int) *uv.Cell {
	t.Helper()
	width := 1
	for _, line := range strings.Split(rendered, "\n") {
		width = max(width, ansi.StringWidth(line))
	}
	height := max(1, strings.Count(rendered, "\n")+1)
	screen := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(rendered).Draw(screen, screen.Bounds())
	cell := screen.CellAt(x, y)
	if cell == nil {
		t.Fatalf("styled output has no cell at %d,%d", x, y)
	}
	return cell
}

func findStyledCell(t *testing.T, rendered, content string) *uv.Cell {
	t.Helper()
	width := 1
	for _, line := range strings.Split(rendered, "\n") {
		width = max(width, ansi.StringWidth(line))
	}
	height := max(1, strings.Count(rendered, "\n")+1)
	screen := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(rendered).Draw(screen, screen.Bounds())
	for y := screen.Bounds().Min.Y; y < screen.Bounds().Max.Y; y++ {
		for x := screen.Bounds().Min.X; x < screen.Bounds().Max.X; x++ {
			cell := screen.CellAt(x, y)
			if cell != nil && cell.Content == content {
				return cell
			}
		}
	}
	t.Fatalf("styled output lacks cell content %q", content)
	return nil
}

func firstVisibleStyledCell(t *testing.T, rendered string) *uv.Cell {
	t.Helper()
	width := max(1, ansi.StringWidth(rendered))
	height := max(1, strings.Count(rendered, "\n")+1)
	screen := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(rendered).Draw(screen, screen.Bounds())
	for y := screen.Bounds().Min.Y; y < screen.Bounds().Max.Y; y++ {
		for x := screen.Bounds().Min.X; x < screen.Bounds().Max.X; x++ {
			cell := screen.CellAt(x, y)
			if cell != nil && strings.TrimSpace(cell.Content) != "" {
				return cell
			}
		}
	}
	t.Fatal("styled output has no visible cell")
	return nil
}

func assertVisibleSignals(
	t *testing.T,
	rendered string,
	width int,
	signals ...string,
) {
	t.Helper()
	plain := ansi.Strip(rendered)
	for _, signal := range signals {
		if !strings.Contains(plain, signal) {
			t.Fatalf("rendered layout lacks %q:\n%s", signal, plain)
		}
	}
	for index, line := range strings.Split(rendered, "\n") {
		if lineWidth := ansi.StringWidth(line); lineWidth > width {
			t.Fatalf(
				"rendered line %d width=%d exceeds %d:\n%s",
				index,
				lineWidth,
				width,
				plain,
			)
		}
	}
}

func assertRenderedHeight(t *testing.T, rendered string, height int) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(lines) > height {
		t.Fatalf(
			"rendered height=%d exceeds %d:\n%s",
			len(lines),
			height,
			ansi.Strip(rendered),
		)
	}
}
