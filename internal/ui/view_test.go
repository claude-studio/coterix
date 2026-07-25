package ui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	// One artifact at a time now: the tab strip names all three, the body shows
	// the selected one (T14 W3).
	for _, expected := range []string{
		"1 Plan",
		"2 Diff",
		"3 Verdict",
		"implement dashboard",
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
	for _, unexpected := range []string{"-old", "+new", "clean"} {
		if strings.Contains(plain, unexpected) {
			t.Fatalf(
				"the Plan tab showed another tab's artifact %q:\n%s",
				unexpected,
				plain,
			)
		}
	}
}

// The FEED box shows one artifact at a time: `1/2/3` select and `[/]` cycle, the
// strip lives in the box heading, and switching returns the box to the live edge
// because the offset now measures into a different document (T14 W3).
func TestArtifactTabsSelectCycleAndResetScroll(t *testing.T) {
	current := tabbedViewModel(t)
	body := func(model model) string {
		return ansi.Strip(mainBoxBody(model, boxFeed, 90, 20))
	}
	if got := body(current); !strings.Contains(got, "implement dashboard") {
		t.Fatalf("the default tab is not Plan:\n%s", got)
	}

	// The strip belongs to the heading, so it must survive into the box chrome and
	// not cost a body row.
	heading := ansi.Strip(strings.SplitN(
		renderBoxCard(
			current.theme,
			mainBoxTitle(current, boxFeed),
			mainBoxTitleSuffix(current, boxFeed, false),
			"body",
			90,
			false,
		),
		"\n",
		2,
	)[0])
	if !containsAll(heading, "PIPELINE FEED", "1 Plan", "2 Diff", "3 Verdict") {
		t.Fatalf("the tab strip is not in the box heading: %q", heading)
	}

	for _, testCase := range []struct {
		key  rune
		want string
	}{
		{'2', "+new"},
		{'3', "clean"},
		{'1', "implement dashboard"},
	} {
		updated, _ := current.Update(printableKey(testCase.key))
		current = updated.(model)
		if got := body(current); !strings.Contains(got, testCase.want) {
			t.Fatalf("key %q did not select its artifact:\n%s", testCase.key, got)
		}
	}

	// `]` cycles forward and wraps; `[` cycles back.
	for _, want := range []artifactTab{tabDiff, tabVerdict, tabPlan} {
		updated, _ := current.Update(printableKey(']'))
		current = updated.(model)
		if current.artifactTab != want {
			t.Fatalf("] moved to tab %d want %d", current.artifactTab, want)
		}
	}
	for _, want := range []artifactTab{tabVerdict, tabDiff, tabPlan} {
		updated, _ := current.Update(printableKey('['))
		current = updated.(model)
		if current.artifactTab != want {
			t.Fatalf("[ moved to tab %d want %d", current.artifactTab, want)
		}
	}

	current.boxScroll[boxFeed] = 6
	updated, _ := current.Update(printableKey('2'))
	current = updated.(model)
	if current.boxScroll[boxFeed] != 0 {
		t.Fatalf(
			"switching tabs left the box at offset %d in a different document",
			current.boxScroll[boxFeed],
		)
	}
}

// A heading too narrow for both drops the tab strip whole and keeps the title.
// Truncating into the strip would cut between a colour and its reset, and the row
// must still be exactly `width` cells so the border closes (T14 W3).
func TestNarrowBoxHeadingDropsTheTabStripNotTheTitle(t *testing.T) {
	current := tabbedViewModel(t)
	suffix := mainBoxTitleSuffix(current, boxFeed, false)

	for _, width := range []int{24, 30, 34, 90} {
		heading := strings.SplitN(renderBoxCard(
			current.theme,
			mainBoxTitle(current, boxFeed),
			suffix,
			"body",
			width,
			false,
		), "\n", 2)[0]
		plain := ansi.Strip(heading)
		if got := ansi.StringWidth(plain); got != width {
			t.Fatalf("width=%d heading spans %d cells: %q", width, got, plain)
		}
		// Partial tabs are the failure mode: either the whole strip is there or
		// none of it.
		labels := 0
		for _, label := range []string{"1 Plan", "2 Diff", "3 Verdict"} {
			if strings.Contains(plain, label) {
				labels++
			}
		}
		if labels != 0 && labels != 3 {
			t.Fatalf("width=%d kept %d of 3 tabs: %q", width, labels, plain)
		}
		if labels == 0 && !strings.Contains(plain, "PIPELINE") {
			t.Fatalf("width=%d dropped the title instead of the strip: %q", width, plain)
		}
	}
}

// An empty tab stays selectable and says what is missing; the strip marks it
// without relying on colour (color-system.md) — T14 W3.
func TestEmptyArtifactTabStaysSelectableAndNamesWhatIsMissing(t *testing.T) {
	current := populatedViewModel(t)
	current.artifacts = artifactData{PlanMarkdown: "# Plan\n\n- only a plan\n"}
	current.refreshArtifactRender()

	strip := ansi.Strip(renderArtifactTabs(current))
	if !containsAll(strip, "(2 Diff)", "(3 Verdict)") {
		t.Fatalf("empty tabs carry no non-color cue: %q", strip)
	}
	if strings.Contains(strip, "(1 Plan)") {
		t.Fatalf("the populated tab was marked empty: %q", strip)
	}

	updated, _ := current.Update(printableKey('2'))
	current = updated.(model)
	if current.artifactTab != tabDiff {
		t.Fatal("an empty tab refused selection")
	}
	if got := ansi.Strip(mainBoxBody(current, boxFeed, 90, 20)); !strings.Contains(
		got,
		"No diff yet",
	) {
		t.Fatalf("the empty tab does not name what is missing: %q", got)
	}
}

// The active tab carries Primary *and* bold, so the selection survives a terminal
// that drops colour (color-system.md: never state by colour alone) — T14 W3.
func TestActiveArtifactTabCarriesColorAndWeight(t *testing.T) {
	current := tabbedViewModel(t)
	strip := renderArtifactTabs(current)

	// Lowercase letters are unique to the strip: the box heading around it is all
	// caps, so "l" can only come from "Plan" and "i" from "Diff".
	active := findStyledCell(t, strip, "l")
	assertSameColor(
		t,
		active.Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.Primary),
	)
	if active.Style.Attrs&uv.AttrBold == 0 {
		t.Fatal("the active tab is signalled by colour alone")
	}

	inactive := findStyledCell(t, strip, "i")
	assertSameColor(
		t,
		inactive.Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.FGMostSubtle),
	)
	if inactive.Style.Attrs&uv.AttrBold != 0 {
		t.Fatal("an inactive tab is bold, so weight no longer marks the active one")
	}

	// The same has to hold *inside the box heading*: the strip is multi-colour, so
	// putting it through the heading's single style would flatten it.
	heading := strings.SplitN(renderBoxCard(
		current.theme,
		mainBoxTitle(current, boxFeed),
		mainBoxTitleSuffix(current, boxFeed, false),
		"body",
		90,
		false,
	), "\n", 2)[0]
	assertSameColor(
		t,
		findStyledCell(t, heading, "l").Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.Primary),
	)
	assertSameColor(
		t,
		findStyledCell(t, heading, "i").Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.FGMostSubtle),
	)
}

func tabbedViewModel(t *testing.T) model {
	t.Helper()
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
	current.refreshArtifactRender()
	return current
}

// The header's right slot names what is running rather than showing a fixed
// chip. The old `● real-time` text never changed during a multi-minute step, so
// nothing in the content pane answered "what is it doing right now" — the only
// motion was the sidebar clock and the status bar (live-smoke finding).
func TestMainHeaderNamesActiveStepAndIdleState(t *testing.T) {
	current := populatedViewModel(t)

	working := renderMainHeader(current, 96)
	plain := ansi.Strip(working)
	if !strings.Contains(plain, "plan_writer · claude") {
		t.Fatalf("working header must name the active role and CLI: %q", plain)
	}
	if strings.Contains(plain, "real-time") {
		t.Fatalf("fixed chip text survived: %q", plain)
	}
	workingCell := findStyledCell(t, working, "p")
	assertSameColor(
		t,
		workingCell.Style.Fg,
		lipgloss.Color(current.theme.tokens.Status.Busy.FG),
	)

	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	current.operation = ""
	idle := renderMainHeader(current, 96)
	plain = ansi.Strip(idle)
	if !strings.Contains(plain, "● idle") {
		t.Fatalf("idle header=%q", plain)
	}
	idleChip := findStyledCell(t, idle, "●")
	assertSameColor(
		t,
		idleChip.Style.Fg,
		lipgloss.Color(current.theme.tokens.Theme.FGMostSubtle),
	)
}

// The typed response must actually appear. The original defect was a misused
// TruncateLeftWc that erased every response shorter than the budget; the editor
// replaced that path, so the guard now runs through the real key/paste route
// (live-smoke finding, 2026-07-25 · T14 W5).
func TestPromptRendersTypedResponse(t *testing.T) {
	for _, promptCase := range []struct {
		name  string
		value string
	}{
		{"wide runes", "한국어 답변입니다"},
		{"ascii", "retry please"},
		{"long enough to scroll", strings.Repeat("a", 40) + "TAIL"},
	} {
		t.Run(promptCase.name, func(t *testing.T) {
			current := populatedViewModel(t)
			current.status.Phase = state.PhasePausedForInput
			current.status.PendingAction = &state.PendingAction{
				Kind:   state.PendingPlanQuestion,
				Prompt: "질문",
			}
			updated, _ := current.Update(specialKey(tea.KeyEnter))
			current = updated.(model)
			if !current.usesTextarea() {
				t.Fatal("a free-text response did not open the editor")
			}
			updated, _ = current.Update(tea.PasteMsg{Content: promptCase.value})
			current = updated.(model)

			if got := current.promptResponse(); got != promptCase.value {
				t.Fatalf("editor holds %q, want %q", got, promptCase.value)
			}
			plain := ansi.Strip(renderStatusBar(current, 120, promptRowsArea))
			// The editor may scroll a long value, so assert on its tail — the part
			// being typed — rather than the whole string.
			tail := promptCase.value
			if len(tail) > 12 {
				tail = tail[len(tail)-4:]
			}
			if !strings.Contains(plain, tail) {
				t.Fatalf("prompt dropped the typed response %q:\n%s", tail, plain)
			}
			if !strings.Contains(plain, "ctrl+j newline") {
				t.Fatalf("the editor does not advertise its newline key:\n%s", plain)
			}
		})
	}
}

// Every prompt kind has to fit inside the region it asks for: too much chrome and
// the input wraps, pushing the footer out of view (review T13a-1-followup f1). The
// editor asks for more rows than the pick does, so each kind is measured against
// its own budget (T14 W5).
func TestEveryPromptKindStaysWithinItsRegion(t *testing.T) {
	base := populatedViewModel(t)
	base.status.Phase = state.PhaseAwaitingApproval

	textPrompt := base
	textPrompt.status.Phase = state.PhasePausedForInput
	textPrompt.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingPlanQuestion,
		Prompt: "질문",
	}
	updated, _ := textPrompt.Update(specialKey(tea.KeyEnter))
	textPrompt = updated.(model)
	updated, _ = textPrompt.Update(tea.PasteMsg{
		Content: strings.Repeat("긴 응답 ", 30),
	})
	textPrompt = updated.(model)

	capPrompt := base
	capPrompt.status.Phase = state.PhasePausedForInput
	capPrompt.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingTaskCap,
		Prompt: "cap reached",
	}
	updated, _ = capPrompt.Update(specialKey(tea.KeyEnter))
	capPrompt = updated.(model)

	confirmPrompt := base
	updated, _ = confirmPrompt.Update(printableKey('a'))
	confirmPrompt = updated.(model)

	for _, promptCase := range []struct {
		name   string
		model  model
		footer string
	}{
		{"editor", textPrompt, "ctrl+j newline"},
		{"task_cap pick", capPrompt, "choose"},
		{"approve confirm", confirmPrompt, "enter confirm"},
	} {
		t.Run(promptCase.name, func(t *testing.T) {
			region := promptRegionRows(promptCase.model)
			for _, width := range []int{40, 60, 80, 120} {
				innerWidth := width - 2
				plain := ansi.Strip(
					renderStatusBar(promptCase.model, width, region),
				)
				if !strings.Contains(plain, promptCase.footer) {
					t.Fatalf(
						"width=%d footer %q was pushed out of the region:\n%s",
						width,
						promptCase.footer,
						plain,
					)
				}
				lines := strings.Split(plain, "\n")
				if len(lines) > region {
					t.Fatalf(
						"width=%d prompt used %d rows, region is %d:\n%s",
						width,
						len(lines),
						region,
						plain,
					)
				}
				for index, line := range lines {
					if cells := ansi.StringWidth(line); cells > innerWidth {
						t.Fatalf(
							"width=%d row %d is %d cells, region is %d:\n%s",
							width,
							index,
							cells,
							innerWidth,
							plain,
						)
					}
				}
			}
		})
	}
}

func TestPromptFooterStaysOnOneRow(t *testing.T) {
	current := populatedViewModel(t)
	current.prompt = promptResume
	current.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingTaskCap,
		Prompt: "retry or abort",
	}
	current.promptValue = "maybe"
	current.promptError = "Task cap response must be retry or abort."

	for _, width := range []int{40, 24, 12} {
		plain := ansi.Strip(renderStatusBar(current, width, 4))
		lines := strings.Split(plain, "\n")
		if len(lines) > 4 {
			t.Fatalf(
				"width=%d used %d rows, region is 4:\n%s",
				width,
				len(lines),
				plain,
			)
		}
		for index, line := range lines {
			if cells := ansi.StringWidth(line); cells > width-2 {
				t.Fatalf(
					"width=%d row %d is %d cells:\n%s",
					width,
					index,
					cells,
					plain,
				)
			}
		}
		if !strings.Contains(plain, "×") {
			t.Fatalf("width=%d lost the error marker:\n%s", width, plain)
		}
	}
}

// The header must keep `role · cli · elapsed` intact at the minimum wide width
// even when the request is long — reverting the prefix-width fix has to fail here
// (review round-3 f3).
func TestMainHeaderKeepsDescriptorAtMinimumWideWidth(t *testing.T) {
	current := populatedViewModel(t)
	current.request = strings.Repeat("update the readme thoroughly ", 6)
	current.activeRole = "plan_reviewer"
	current.activeCLI = "codex"
	// Put the active stage on the clock so the descriptor carries an elapsed time.
	// observePhase only drives the approval window, so the step has to start it.
	current.status.Phase = state.PhasePlanning
	start := current.now()
	current.stages.stepStarted(pipeline.StepPlan, start)
	current.now = func() time.Time { return start.Add(95 * time.Second) }

	// The main pane at the minimum wide layout.
	headerWidth := wideBreakpointWidth - sidebarWidth - 4
	plain := ansi.Strip(renderMainHeader(current, headerWidth))
	if cells := ansi.StringWidth(plain); cells > headerWidth {
		t.Fatalf("header is %d cells, budget is %d:\n%s", cells, headerWidth, plain)
	}
	for _, want := range []string{"plan_reviewer", "codex", "1m35s"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("header dropped %q:\n%s", want, plain)
		}
	}
}

// The tag column names the *source* and the meta column names the step. The
// harness used to render `INFO` — severity vocabulary that clashed with the icon
// column, so a failed row read `INFO × … failed` — and repeated `control ·` on
// every row while the text carried the real role (T14 W11, user-reported).
func TestLifecycleEntriesNameSourceAndStep(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind: pipeline.EventStepStarted,
		Step: pipeline.StepPlan,
		Role: "plan_writer",
		CLI:  "claude",
	}})
	current = updated.(model)

	result := runner.RunResult{Exit: 1}
	updated, _ = current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:    pipeline.EventAttemptFinished,
		Step:    pipeline.StepPlan,
		Role:    "plan_writer",
		Attempt: 2,
		Result:  &result,
	}})
	current = updated.(model)

	feed := ansi.Strip(renderFeed(current, 120))
	if strings.Contains(feed, "INFO") {
		t.Fatalf("harness tag still uses severity vocabulary:\n%s", feed)
	}
	if strings.Contains(feed, "control ·") {
		t.Fatalf("meta column still repeats a constant role:\n%s", feed)
	}
	for _, want := range []string{
		"CTRX",
		"plan_writer ·",
		"claude started",
		"attempt 2 exited 1",
	} {
		if !strings.Contains(feed, want) {
			t.Fatalf("lifecycle feed lacks %q:\n%s", want, feed)
		}
	}

	// The role belongs to the meta column *only*. Asserting mere presence let a
	// row read `plan_writer · plan_writer · claude started` — exactly the
	// duplication W11 removes (review T14a-r2 f3).
	rows := map[string]string{}
	for _, line := range strings.Split(feed, "\n") {
		switch {
		case strings.Contains(line, "claude started"):
			rows["step started"] = line
		case strings.Contains(line, "attempt 2 exited 1"):
			rows["attempt finished"] = line
		}
	}
	if len(rows) != 2 {
		t.Fatalf("expected one started row and one attempt row, got %v", rows)
	}
	for name, line := range rows {
		if got := strings.Count(line, "plan_writer"); got != 1 {
			t.Fatalf(
				"the %s row names the role %d times, want exactly 1:\n%s",
				name,
				got,
				line,
			)
		}
	}
}

// The artifact markdown is cached at one width, so that width must equal the real
// body width of the box that shows it. Too wide and the right edge is truncated
// (wide) or lipgloss re-wraps and silently adds rows that push the bottom box off
// screen (compact) — review T14a f1.
func TestArtifactCacheWidthMatchesBoxBodyWidth(t *testing.T) {
	mainWidth := wideBreakpointWidth - sidebarWidth
	cached := dashboardMainInnerWidth(wideBreakpointWidth, wideBreakpointHeight)
	if want := max(8, mainWidth-4) - 4; cached != want {
		t.Fatalf("wide cache width=%d want=%d", cached, want)
	}
	if got, want := dashboardMainInnerWidth(80, 24), 80-4-4; got != want {
		t.Fatalf("compact cache width=%d want=%d", got, want)
	}

	// A line exactly as wide as the cache must survive to its last cell.
	current := populatedViewModel(t)
	current.artifactRender = strings.Repeat("x", cached-3) + "END"
	frame := ansi.Strip(renderMain(
		current,
		mainWidth,
		wideBreakpointHeight-topBarHeight-2,
	))
	if !strings.Contains(frame, "END") {
		t.Fatalf("artifact right edge was truncated:\n%s", frame)
	}
}

// Priority — not render order — decides which boxes survive a tight budget:
// ACTIVITY (the live edge) must outlive FEED (review T14a f5).
func TestBoxHeightPriorityDecidesSurvivors(t *testing.T) {
	order := []mainBox{boxPending, boxFeed, boxLiveOutput, boxActivity}
	wants := []int{1, 1, 1, 1}

	for _, testCase := range []struct {
		name   string
		total  int
		chrome int
		want   []mainBox
	}{
		{"wide: one box fits", 3, 2, []mainBox{boxPending}},
		{"wide: two boxes fit", 6, 2, []mainBox{boxPending, boxActivity}},
		{
			"wide: three boxes fit",
			9,
			2,
			[]mainBox{boxPending, boxLiveOutput, boxActivity},
		},
		{"compact: two boxes fit", 4, 1, []mainBox{boxPending, boxActivity}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			heights := distributeMainBoxHeights(
				order,
				wants,
				testCase.total,
				testCase.chrome,
			)
			alive := make([]mainBox, 0, len(order))
			sum := 0
			for index, box := range order {
				sum += heights[index]
				if heights[index] > 0 {
					alive = append(alive, box)
				}
			}
			if sum > testCase.total {
				t.Fatalf("allocated %d rows, total is %d", sum, testCase.total)
			}
			if len(alive) != len(testCase.want) {
				t.Fatalf("survivors=%v want=%v", alive, testCase.want)
			}
			for index, box := range testCase.want {
				if alive[index] != box {
					t.Fatalf("survivors=%v want=%v", alive, testCase.want)
				}
			}
		})
	}
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
	}, 80, false))
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
			renderLogIcon(current.theme, iconCase.entry, false),
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
		false,
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
			}, 60, false)
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

		// The sidebar keeps the *signal*; the question body moved to the main
		// pane's PENDING box (T14 W1) because ~30 cells turned a paragraph into
		// an unreadable vertical ribbon that also consumed the 26-row budget.
		wide := renderSidebar(candidate, sidebarWidth, sidebarBudget)
		assertVisibleSignals(
			t,
			wide,
			sidebarWidth,
			"task_cap",
			"pending-error-marker",
		)
		if strings.Contains(ansi.Strip(wide), "retry or abort") {
			t.Fatalf("sidebar still renders the question body:\n%s", wide)
		}
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

// The `?` overlay is the only overlay: `?` toggles it, `esc` closes it, and nothing
// stacks on top (T14 W6).
func TestHelpOverlayTogglesAndCoversTheContent(t *testing.T) {
	current := populatedViewModel(t)
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight

	if strings.Contains(ansi.Strip(renderDashboard(current)), "KEYS") {
		t.Fatal("the overlay is up before `?` was pressed")
	}

	updated, _ := current.Update(printableKey('?'))
	current = updated.(model)
	frame := ansi.Strip(renderDashboard(current))
	for _, expected := range []string{"KEYS", "esc closes", "tab", "1 · 2 · 3"} {
		if !strings.Contains(frame, expected) {
			t.Fatalf("the overlay lacks %q:\n%s", expected, frame)
		}
	}
	// It is drawn over the content, not beside it — the status bar stays visible.
	if !strings.Contains(frame, "orchestration") {
		t.Fatalf("the overlay replaced the whole frame:\n%s", frame)
	}

	// `?` again closes it, and so does `esc` — with no stack to unwind.
	updated, _ = current.Update(printableKey('?'))
	if updated.(model).helpOpen {
		t.Fatal("`?` did not toggle the overlay closed")
	}
	updated, _ = current.Update(specialKey(tea.KeyEscape))
	if updated.(model).helpOpen {
		t.Fatal("esc did not close the overlay")
	}
}

// The overlay and the status-bar hint line come from one table, so a key cannot be
// advertised in one and missing from the other — and no key may be documented that
// updateKey does not actually dispatch (T14 W6, the guard that caught `y`/`w` being
// listed while they are still T13b work).
func TestDocumentedKeysAreActuallyDispatched(t *testing.T) {
	source, err := os.ReadFile("model.go")
	if err != nil {
		t.Fatal(err)
	}
	dispatch := string(source)

	current := populatedViewModel(t)
	current.status.Phase = state.PhaseAwaitingApproval
	seen := 0
	for _, group := range keyHelpGroups(current) {
		for _, entry := range group.entries {
			for _, token := range strings.Split(entry.keys, "·") {
				token = strings.TrimSpace(token)
				if token == "" {
					continue
				}
				seen++
				if !strings.Contains(dispatch, `"`+token+`"`) {
					t.Fatalf(
						"the overlay documents %q but no case in model.go handles it",
						token,
					)
				}
			}
		}
	}
	if seen < 10 {
		t.Fatalf("only %d keys were checked — the table looks truncated", seen)
	}

	// The hint line is a projection of the same table.
	hints := keyHintLine(current)
	for _, group := range keyHelpGroups(current) {
		for _, entry := range group.entries {
			if entry.short == "" {
				continue
			}
			if !strings.Contains(hints, entry.short) {
				t.Fatalf("hint line %q dropped %q", hints, entry.short)
			}
		}
	}
	if strings.Contains(hints, "a approve") == (current.status.Phase !=
		state.PhaseAwaitingApproval) {
		t.Fatalf("phase actions are not reflected in the hint line: %q", hints)
	}
}
