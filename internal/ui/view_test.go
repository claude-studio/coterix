package ui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

func TestDashboardWideAndCompactLayoutBranches(t *testing.T) {
	current := populatedViewModel(t)

	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight
	wide := ansi.Strip(renderDashboard(current))
	for _, expected := range []string{
		wordmarkText,
		"orchestration",
		"coterix run",
		"PIPELINE FEED",
		"PIPELINE",
		"PLAN",
		"VERIFY",
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
	// Compact keeps the pre-card feed: no rounded card, no command header.
	if strings.Contains(compact, "╭") || strings.Contains(compact, "╰") ||
		strings.Contains(compact, "coterix run") {
		t.Fatalf("compact layout rendered the wide-only card:\n%s", compact)
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

func TestMainPaneRendersPlanDiffVerdictAndSplitFeed(t *testing.T) {
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
	// The lifecycle feed carries harness entries only; subprocess output lives
	// in the pinned activity tail below it (plan T13 W2).
	current.logs = []logEntry{{
		Role:    "impl_writer",
		CLI:     "codex",
		Attempt: 1,
		Icon:    logIconStart,
		Text:    "impl_writer · codex started",
	}}
	current.activity = []logEntry{{
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
		"impl_writer · codex started",
		"ACTIVITY",
		"CODEX",
		"impl_writer#1 · streamed output",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("main pane lacks %q:\n%s", expected, plain)
		}
	}
}

func TestMainHeaderKeepsRealTimeChipTextAcrossStates(t *testing.T) {
	current := populatedViewModel(t)

	working := renderMainHeader(current, 96)
	if !strings.Contains(ansi.Strip(working), "● real-time") {
		t.Fatalf("working header=%q", ansi.Strip(working))
	}
	workingChip := findStyledCell(t, working, "●")
	assertSameColor(
		t,
		workingChip.Style.Fg,
		lipgloss.Color(current.theme.tokens.Status.Busy.FG),
	)

	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	current.operation = ""
	idle := renderMainHeader(current, 96)
	plain := ansi.Strip(idle)
	if !strings.Contains(plain, "● real-time") ||
		strings.Contains(plain, "idle") {
		t.Fatalf("idle header=%q", plain)
	}
	idleChip := findStyledCell(t, idle, "●")
	assertSameColor(
		t,
		idleChip.Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.FGMostSubtle),
	)
}

func TestLogLinesRenderColumnarTimeTagAndIcon(t *testing.T) {
	current := populatedViewModel(t)
	at := time.Date(2026, 7, 24, 21, 42, 7, 0, time.UTC)

	line := ansi.Strip(renderLogLine(current.theme, logEntry{
		Role:    "impl_writer",
		CLI:     "codex",
		Attempt: 1,
		Text:    "writing files",
		At:      at,
	}, 80))
	for _, expected := range []string{
		"21:42:07",
		"CODEX",
		// The role separator `·` (not the stdout icon `·`): assert the
		// full meta prefix so the icon cannot satisfy this check.
		"impl_writer#1 · writing files",
	} {
		if !strings.Contains(line, expected) {
			t.Fatalf("columnar log line lacks %q: %q", expected, line)
		}
	}

	iconCases := []struct {
		entry logEntry
		glyph string
	}{
		{entry: logEntry{Icon: logIconStart}, glyph: "▸"},
		{entry: logEntry{Icon: logIconDone}, glyph: "✓"},
		{entry: logEntry{Icon: logIconFail}, glyph: "×"},
		{entry: logEntry{Stream: runner.StreamStderr}, glyph: "×"},
	}
	for _, iconCase := range iconCases {
		rendered := ansi.Strip(
			renderLogIcon(current.theme, iconCase.entry),
		)
		if rendered != iconCase.glyph {
			t.Fatalf("icon=%q, want %q", rendered, iconCase.glyph)
		}
	}

	// appendLog stamps arrival time from the injected clock.
	current.now = func() time.Time { return at }
	current.appendLog(logEntry{CLI: "claude", Text: "hello"})
	stamped := current.logs[len(current.logs)-1]
	if !stamped.At.Equal(at) {
		t.Fatalf("appendLog stamped %v, want %v", stamped.At, at)
	}
	if rendered := ansi.Strip(renderLogLine(
		current.theme,
		stamped,
		80,
	)); !strings.Contains(rendered, "21:42:07") ||
		!strings.Contains(rendered, "CLAUDE") {
		t.Fatalf("stamped log line=%q", rendered)
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

func TestTopBarShowsWordmarkActivityAndRun(t *testing.T) {
	current := populatedViewModel(t)
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight

	working := renderTopBar(current, wideBreakpointWidth, topBarHeight)
	plain := ansi.Strip(working)
	for _, expected := range []string{
		wordmarkText,
		"● orchestration active",
		"run: run-9",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("working top bar lacks %q:\n%s", expected, plain)
		}
	}
	lines := strings.Split(working, "\n")
	if len(lines) != topBarHeight {
		t.Fatalf("top bar rows=%d, want %d", len(lines), topBarHeight)
	}
	for index, line := range lines {
		if width := ansi.StringWidth(line); width > wideBreakpointWidth {
			t.Fatalf("top bar row %d width=%d exceeds %d", index, width, wideBreakpointWidth)
		}
	}
	// The second row is a full-width rule: gradient under the wordmark,
	// separator across the remaining width.
	underline := ansi.Strip(lines[1])
	contentWidth := wideBreakpointWidth - 4
	if !strings.Contains(underline, strings.Repeat("─", contentWidth)) {
		t.Fatalf("top bar underline does not span the content width:\n%s", underline)
	}
	separatorCell := styledCellAt(t, lines[1], contentWidth-1, 0)
	assertSameColor(
		t,
		separatorCell.Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.Separator),
	)

	activeCell := findStyledCell(t, working, "●")
	assertSameColor(
		t,
		activeCell.Style.Fg,
		lipgloss.Color(current.theme.tokens.Status.Busy.FG),
	)
	wordmarkCell := styledCellAt(t, lines[0], 1, 0)
	assertSameColor(
		t,
		wordmarkCell.Style.Fg,
		lipgloss.Color(current.theme.tokens.Gradient.BrandLeftToRight[0]),
	)

	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	current.operation = ""
	idle := ansi.Strip(renderTopBar(current, wideBreakpointWidth, topBarHeight))
	if !strings.Contains(idle, "● orchestration idle") ||
		strings.Contains(idle, "orchestration active") {
		t.Fatalf("idle top bar=%q", idle)
	}

	compact := ansi.Strip(renderCompactHeader(current, 80, 2))
	if !strings.Contains(compact, "COTERIX") {
		t.Fatalf("compact header lost the wordmark:\n%s", compact)
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
			// Columns: 8-cell time, one space, then the CLI tag.
			assertSameColor(
				t,
				styledCellAt(t, logLine, 9, 0).Style.Fg,
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

	// The wide sidebar budget at 120x30 after the top bar and status bar.
	sidebarBudget := wideBreakpointHeight - topBarHeight - 2

	t.Run("awaiting approval with last error", func(t *testing.T) {
		candidate := current
		candidate.status.Phase = state.PhaseAwaitingApproval
		candidate.status.PendingAction = nil
		candidate.status.LastError = pointerTo(
			"approval-error-marker " + strings.Repeat("failure context ", 3),
		)

		wide := renderSidebar(candidate, sidebarWidth, sidebarBudget)
		assertVisibleSignals(
			t,
			wide,
			sidebarWidth,
			"APPROVAL NEEDED",
			"approval-error-marker",
		)
		assertRenderedHeight(t, wide, sidebarBudget)

		dashboard := renderDashboard(candidate)
		assertVisibleSignals(
			t,
			dashboard,
			candidate.width,
			"APPROVAL NEEDED",
			"approval-error-marker",
		)

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

		wide := renderSidebar(candidate, sidebarWidth, sidebarBudget)
		assertVisibleSignals(
			t,
			wide,
			sidebarWidth,
			"task_cap",
			"retry or abort",
			"pending-error-marker",
		)
		assertRenderedHeight(t, wide, sidebarBudget)

		dashboard := renderDashboard(candidate)
		assertVisibleSignals(
			t,
			dashboard,
			candidate.width,
			"task_cap",
			"retry or abort",
			"pending-error-marker",
		)

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
