package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// The progress bar has to appear in the *real* dashboard. The first version only
// showed one when this was called at width 30; the production rail is 32 cells, which
// leaves the row 14, and the bar was squeezed out of every actual frame
// (review T14d f1). So this renders through renderDashboard.
func TestProgressBarAppearsInTheProductionDashboard(t *testing.T) {
	current := populatedViewModel(t)
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight
	current.status.TaskOrder = []string{"T1", "T2", "T3", "T4"}
	current.status.Tasks = map[string]state.TaskState{
		"T1": {Status: state.TaskConfirmed},
		"T2": {Status: state.TaskCandidate},
		"T3": {Status: state.TaskOpen},
		"T4": {Status: state.TaskOpen},
	}

	frame := ansi.Strip(renderDashboard(current))
	if !strings.Contains(frame, "■") || !strings.Contains(frame, "□") {
		t.Fatalf("no progress bar in the real dashboard:\n%s", frame)
	}
	if !strings.Contains(frame, "1/4") {
		t.Fatalf("the counts were displaced by the bar:\n%s", frame)
	}
	// The round survives, abbreviated if that is what makes the bar fit.
	if !strings.Contains(frame, "round 3") && !strings.Contains(frame, "r3") {
		t.Fatalf("the plan round disappeared:\n%s", frame)
	}
	// Human-intervention signals still outrank it.
	if !strings.Contains(frame, "APPROVAL NEEDED") {
		t.Fatalf("the bar displaced the approval signal:\n%s", frame)
	}
	// And no row overflows the rail.
	for index, row := range strings.Split(frame, "\n") {
		if cells := ansi.StringWidth(row); cells > wideBreakpointWidth {
			t.Fatalf("row %d is %d cells wide:\n%s", index, cells, frame)
		}
	}

	// compact folds the rail away entirely; it must not grow a bar row there.
	compact := current
	compact.width = 80
	compact.height = 24
	compactFrame := ansi.Strip(renderDashboard(compact))
	if strings.Contains(compactFrame, "progress:") {
		t.Fatalf("compact grew a sidebar row:\n%s", compactFrame)
	}
}

// The bar is a proportion, so the ends must be exact: nothing filled at zero, every
// cell filled when the plan is done.
func TestProgressBarEndsAreExact(t *testing.T) {
	current := populatedViewModel(t)
	base := deriveSidebar(current)

	for _, testCase := range []struct {
		name             string
		confirmed, total int
		filled, empty    bool
	}{
		{name: "nothing done", confirmed: 0, total: 5, empty: true},
		{name: "all done", confirmed: 5, total: 5, filled: true},
		{name: "partway", confirmed: 3, total: 5, filled: true, empty: true},
		// No plan yet means no proportion to draw — neither half of the bar.
		{name: "no tasks yet", confirmed: 0, total: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := base
			data.Confirmed = testCase.confirmed
			data.Total = testCase.total
			row := ansi.Strip(renderProgressValue(current.theme, data, 40))
			if got := strings.Contains(row, "■"); got != testCase.filled {
				t.Fatalf("filled cells present=%v, want %v: %q",
					got, testCase.filled, row)
			}
			if got := strings.Contains(row, "□"); got != testCase.empty {
				t.Fatalf("empty cells present=%v, want %v: %q",
					got, testCase.empty, row)
			}
		})
	}
}

func progressRow(t *testing.T, rows []string) string {
	t.Helper()
	for _, row := range rows {
		if strings.Contains(row, "progress:") {
			return row
		}
	}
	t.Fatalf("no progress row in:\n%s", strings.Join(rows, "\n"))
	return ""
}

// r2-f3: the share caps bound the final box height, not the content rows. Applying
// them to content alone let LIVE OUTPUT grow chrome rows past its third of the pane
// and take them from FEED (review T14c-r2 f3).
func TestBoxSharesCapTheFinalHeightNotTheContent(t *testing.T) {
	current := populatedViewModel(t)
	current.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingPlanQuestion,
		Prompt: strings.Repeat("a long pending question ", 20),
	}
	for index := 0; index < 60; index++ {
		current.appendLog(logEntry{Role: fmt.Sprintf("role-%02d", index), Text: "x"})
	}

	for _, layout := range []struct {
		name   string
		total  int
		chrome int
	}{
		{name: "wide", total: 23, chrome: 2},
		{name: "compact", total: 20, chrome: 1},
	} {
		t.Run(layout.name, func(t *testing.T) {
			order := mainBoxOrder(current)
			wants := make([]int, len(order))
			for index, box := range order {
				wants[index] = mainBoxWantRows(
					current,
					box,
					mainBoxBody(current, box, 90, layout.total),
					layout.total,
					layout.chrome,
				)
			}
			heights := distributeMainBoxHeights(
				order,
				wants,
				layout.total,
				layout.chrome,
			)
			for index, box := range order {
				limit := 0
				switch box {
				case boxPending:
					limit = layout.total / 2
				case boxLiveOutput:
					limit = layout.total / 3
				default:
					continue
				}
				// FEED absorbs the slack, so only the capped boxes are checked.
				if heights[index] > limit {
					t.Fatalf("box %d got %d rows, its share is %d",
						box, heights[index], limit)
				}
			}
		})
	}
}

// W9: the run id and the withheld-rows marker are OSC8 links to the paths they name.
// Measured (x/ansi v0.11.7): the escape is zero-width, survives the uv compositor,
// truncates without losing its closing sequence, and ansi.Strip removes it — so the
// link costs no layout budget and unsupporting terminals just show the text.
func TestPathsRenderAsZeroWidthHyperlinks(t *testing.T) {
	current := populatedViewModel(t)
	current.repoRoot = "/repo root/coterix"
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight

	wantDir := "file:///repo%20root/coterix/.coterix/runs/run-9"

	t.Run("the run id links to the run directory", func(t *testing.T) {
		bar := renderTopBar(current, 120, topBarHeight)
		if !strings.Contains(bar, wantDir) {
			t.Fatalf("the run id is not linked:\n%q", bar)
		}
		// Zero-width: stripping the escapes must not change the laid-out row.
		plain := ansi.Strip(bar)
		if strings.Contains(plain, "file://") {
			t.Fatalf("the escape leaked into the visible text:\n%s", plain)
		}
		for index, row := range strings.Split(plain, "\n") {
			if cells := ansi.StringWidth(row); cells > 120 {
				t.Fatalf("row %d is %d cells with a link in it", index, cells)
			}
		}
		if !strings.Contains(plain, "run-9") {
			t.Fatalf("the label was lost:\n%s", plain)
		}
	})

	t.Run("the withheld marker links to logs", func(t *testing.T) {
		linked := current
		linked.appendLog(logEntry{
			Step: pipeline.StepGate,
			Role: "gate",
			Text: strings.Repeat("a long gate failure explanation ", 40),
		})
		linked.focus = boxLiveOutput
		updated, _ := linked.Update(printableKey('k'))
		linked = updated.(model)
		updated, _ = linked.Update(specialKey(tea.KeyEnter))
		linked = updated.(model)

		body := lifecycleBody(linked, 90)
		if !strings.Contains(body, wantDir+"/logs") {
			t.Fatalf("the withheld marker is not linked:\n%q", body)
		}
		if !strings.Contains(ansi.Strip(body), "more rows in logs/") {
			t.Fatalf("the marker text was lost:\n%s", ansi.Strip(body))
		}
	})

	t.Run("no run means no link, not a broken one", func(t *testing.T) {
		bare := populatedViewModel(t)
		bare.repoRoot = ""
		bar := renderTopBar(bare, 120, topBarHeight)
		// Check for the escape itself: an empty path would render as the bare scheme
		// `file:`, which a "file://" check would happily miss.
		if strings.Contains(bar, "\x1b]8;;") {
			t.Fatalf("a link was emitted without a run directory:\n%q", bar)
		}
	})

	t.Run("plan.md is linked from the Plan tab", func(t *testing.T) {
		tabbed := tabbedViewModel(t)
		tabbed.repoRoot = current.repoRoot
		strip := renderArtifactTabs(tabbed)
		if !strings.Contains(strip, wantDir+"/plan.md") {
			t.Fatalf("the Plan tab does not link plan.md:\n%q", strip)
		}
		if !strings.Contains(ansi.Strip(strip), "1 Plan") {
			t.Fatalf("the tab label was lost:\n%s", ansi.Strip(strip))
		}
	})

	t.Run("the displayed full-log path is the link to it", func(t *testing.T) {
		waiting := current
		waiting.activeRole = "impl_writer"
		waiting.activeCLI = "codex"
		waiting.activeStep = pipeline.StepImplementation
		body := activityWaitingBody(waiting, 90)
		if !strings.Contains(body, wantDir+"/logs") {
			t.Fatalf("the full-log row is plain text:\n%q", body)
		}
		plain := ansi.Strip(body)
		if !strings.Contains(plain, "full log: .coterix/runs/run-9/logs/") {
			t.Fatalf("the path text was lost:\n%s", plain)
		}
	})

	t.Run("compact reaches the run directory too", func(t *testing.T) {
		compact := current
		compact.width = 80
		compact.height = 24
		frame := renderDashboard(compact)
		if !strings.Contains(frame, wantDir) {
			t.Fatalf("compact has no run anchor:\n%q", ansi.Strip(frame))
		}
	})

	t.Run("wide and compact frames both fall back to plain text", func(t *testing.T) {
		for _, size := range [][2]int{{wideBreakpointWidth, wideBreakpointHeight}, {80, 24}} {
			sized := current
			sized.width, sized.height = size[0], size[1]
			plain := ansi.Strip(renderDashboard(sized))
			if strings.Contains(plain, "file://") || strings.Contains(plain, "\x1b]8") {
				t.Fatalf("%dx%d leaked an escape into the text:\n%s",
					size[0], size[1], plain)
			}
			for index, row := range strings.Split(plain, "\n") {
				if cells := ansi.StringWidth(row); cells > size[0] {
					t.Fatalf("%dx%d row %d is %d cells", size[0], size[1], index, cells)
				}
			}
		}
	})

	t.Run("the link uses the previously unconsumed token", func(t *testing.T) {
		cell := findStyledCell(t, renderTopBar(current, 120, topBarHeight), "9")
		assertSameColor(
			t,
			cell.Style.Fg,
			lipgloss.Color(current.theme.tokens.Component.Link),
		)
	})
}

// T13 W6: the terminal tab is often the only place a long run is visible.
func TestWindowTitleNamesTheRunAndPhase(t *testing.T) {
	current := populatedViewModel(t)
	if got := windowTitle(current); !strings.Contains(got, "run-9") ||
		!strings.Contains(got, string(state.PhaseAwaitingApproval)) {
		t.Fatalf("window title = %q, want the run id and phase", got)
	}
	// The real View() must carry it, not just the helper.
	if got := current.View().WindowTitle; got != windowTitle(current) {
		t.Fatalf("View() title = %q, helper = %q", got, windowTitle(current))
	}
	// Before a run is loaded there is nothing to name.
	bare := testModel(t, &fakeUIControl{})
	if got := windowTitle(bare); got != "coterix" {
		t.Fatalf("title before a run = %q", got)
	}
}

// T13 W7: the WORKING text shimmers by sliding its gradient one cell per spinner tick.
// The spinner already animates its own frames, so this must add no second timer and no
// extra render cost — the blend is computed once and the window slides.
func TestWorkingTextShimmerAdvancesWithTheSpinner(t *testing.T) {
	current := populatedViewModel(t)
	current.activeRole = "impl_writer"
	current.activeStep = pipeline.StepImplementation

	first := statusSignal(current)
	advanced := current
	advanced.shimmerPhase++
	second := statusSignal(advanced)

	if first == second {
		t.Fatal("the shimmer did not move between phases")
	}
	// Only the colours move: the text is identical.
	if ansi.Strip(first) != ansi.Strip(second) {
		t.Fatalf("the shimmer changed the text:\n%q\n%q",
			ansi.Strip(first), ansi.Strip(second))
	}
	// A full cycle returns to the start, so the animation is stable rather than drifting.
	width := ansi.StringWidth(ansi.Strip(" " + strings.ToUpper("impl_writer") + " WORKING"))
	cycled := current
	cycled.shimmerPhase += width
	if statusSignal(cycled) != first {
		t.Fatal("a full cycle did not return to the starting frame")
	}

	// The tick has to be wired: a spinner tick *while working* advances the phase.
	// Bumping shimmerPhase by hand above proves the render reacts, not that anything
	// drives it (self-correction: the wiring mutation passed until this was added).
	working := populatedViewModel(t)
	working.activeRole = "impl_writer"
	working.activeStep = pipeline.StepImplementation
	ticked, _ := working.Update(working.spinner.Tick())
	if ticked.(model).shimmerPhase != working.shimmerPhase+1 {
		t.Fatalf("a spinner tick did not advance the shimmer: %d -> %d",
			working.shimmerPhase, ticked.(model).shimmerPhase)
	}

	// The tick is the spinner's, and it only advances while working.
	idle := populatedViewModel(t)
	idle.activeRole = ""
	idle.activeStep = ""
	idle.operation = ""
	updated, _ := idle.Update(idle.spinner.Tick())
	if updated.(model).shimmerPhase != idle.shimmerPhase {
		t.Fatal("the shimmer advanced while idle")
	}
}

// The "no extra render cost" half of W7 is a claim about the blend, so it is measured:
// sliding the window over a full cycle must not blend again (review T13b/W7 f2).
func TestWorkingTextShimmerBlendsOncePerTextAndTheme(t *testing.T) {
	blends := 0
	original := blend1D
	blend1D = func(size int, stops ...color.Color) []color.Color {
		blends++
		return original(size, stops...)
	}
	// The palette is memoized process-wide, so a populated cache would make any count
	// look right. Both the restore and the reset have to happen for the next test too.
	reset := func() {
		shimmerPalettes.Clear()
	}
	t.Cleanup(func() {
		blend1D = original
		reset()
	})
	reset()

	current := populatedViewModel(t)
	current.activeRole = "impl_writer"
	current.activeStep = pipeline.StepImplementation
	width := ansi.StringWidth(" " + strings.ToUpper("impl_writer") + " WORKING")

	frames := make(map[string]bool, width)
	for phase := 0; phase <= width; phase++ {
		frame := current
		frame.shimmerPhase = phase
		frames[statusSignal(frame)] = true
	}
	if blends != 1 {
		t.Fatalf("%d blends over %d phases, want 1", blends, width+1)
	}
	// And the memo did not flatten the animation into one frame.
	if len(frames) != width {
		t.Fatalf("%d distinct frames over a %d-cell cycle", len(frames), width)
	}

	// A different step is a different rune count, so it earns exactly one more blend.
	other := current
	other.activeRole = "reviewer"
	if statusSignal(other) == "" {
		t.Fatal("the reviewer chip did not render")
	}
	if blends != 2 {
		t.Fatalf("a new text length caused %d blends in total, want 2", blends)
	}
	// Re-rendering it does not.
	statusSignal(other)
	if blends != 2 {
		t.Fatalf("a repeated text length blended again: %d", blends)
	}

	// A theme with different stops is a different palette at the same width, so the memo
	// must not answer for it — otherwise a theme swap would keep the old brand colours.
	restyled := current.theme
	restyled.tokens.Gradient.BrandLeftToRight = []string{"#112233", "#445566"}
	sameWidth := " " + strings.ToUpper("impl_writer") + " WORKING"
	recoloured := workingGradientText(restyled, sameWidth, 0)
	if blends != 3 {
		t.Fatalf("a new gradient caused %d blends in total, want 3", blends)
	}
	if recoloured == workingGradientText(current.theme, sameWidth, 0) {
		t.Fatal("the two themes rendered the same frame")
	}
	if blends != 3 {
		t.Fatalf("re-rendering either theme blended again: %d", blends)
	}

	// A theme with no stops has nothing to blend, so it must not blend — the memo stores
	// only successes, so an unguarded failure path pays the per-frame cost forever on the
	// one input that has no palette at all.
	stopless := current.theme
	stopless.tokens.Gradient.BrandLeftToRight = nil
	for phase := 0; phase < 3; phase++ {
		if workingGradientText(stopless, sameWidth, phase) == "" {
			t.Fatal("a stopless theme rendered nothing instead of the plain chip")
		}
	}
	if blends != 3 {
		t.Fatalf("a stopless theme blended %d times in total, want none added", blends)
	}
}

// T13 W5: the changed-files list is last in the rail and only uses rows the signals did
// not need. The budget contract (R2) is that intervention signals are never pushed off.
func TestChangedFilesYieldToInterventionSignals(t *testing.T) {
	// The totals stay single-digit on purpose: `N files (M hidden) · +A/-D` has to fit
	// the rail's 26 inner cells for the complete row to be assertable at all, and +45/-36
	// does not. Additions and deletions still differ so a swapped pair fails.
	files := make([]changedFile, 0, 9)
	for index := 0; index < 9; index++ {
		files = append(files, changedFile{
			Path:      fmt.Sprintf("internal/ui/file-%02d.go", index),
			Additions: 1,
			Deletions: index % 2,
		})
	}
	current := populatedViewModel(t)
	current.artifacts.ChangedFiles = files

	t.Run("it renders with room and reports the total", func(t *testing.T) {
		body := ansi.Strip(renderChangedFiles(current.theme, files, 26, 12))
		if !strings.Contains(body, "CHANGED") {
			t.Fatalf("no section heading:\n%s", body)
		}
		if !strings.Contains(body, "9 files") {
			t.Fatalf("no total:\n%s", body)
		}
		// Capped at five, and the hidden count is stated rather than dropped silently.
		if !strings.Contains(body, "hidden") {
			t.Fatalf("hidden files not accounted for:\n%s", body)
		}
		if got := countRows(body); got > 7 {
			t.Fatalf("the section took %d rows:\n%s", got, body)
		}
	})

	t.Run("it disappears when there is no room", func(t *testing.T) {
		for _, budget := range []int{-3, 0, 1, 2} {
			if body := renderChangedFiles(current.theme, files, 26, budget); body != "" {
				t.Fatalf("budget %d still rendered:\n%s", budget, body)
			}
		}
	})

	// The pressure cases are built by **real state transitions against a real git repo**,
	// not by hand-assembling a model. The previous version rendered through renderSidebar
	// and renderDashboard yet still proved nothing, because the states it rendered cannot
	// occur (review T13b/W5 f1, verified in the source):
	//
	//   - `awaiting_approval` forces CurrentTaskID=nil and every task to open
	//     (plan_cycle.go), and loadArtifactData skips changed files when CurrentTaskID is
	//     nil (artifacts.go) — so approval **never** has a file list to displace.
	//   - `LastError` is written only ever together with PhaseFailed (state/transition.go,
	//     both writers) — so no pending or approval state carries an error.
	//   - task_cap requires ResumePhase=implementing, TaskID==CurrentTaskID and an open or
	//     repairing task (state.validatePendingAction) — the old candidate-task fixture
	//     would have been rejected outright.
	//
	// So the two states that can actually put the file list under pressure are a task_cap
	// pause and a failure, and each is produced here through the state API that validates
	// it.
	for _, pressure := range []struct {
		name string
		// attempts is how many full dirty-review attempts run before seed applies the
		// final transition. task_cap needs exactly MaxTaskAttempts so the next
		// BeginTaskAttempt is the one that caps.
		attempts int
		seed     func(*testing.T, *pipeline.Run, string)
		signals  []string
		// yields is the other half of the R2 contract: when the signals leave fewer than
		// three rows, the section is dropped entirely rather than pushing a signal off.
		yields bool
	}{
		{
			name:     "task_cap pause",
			attempts: pressureAttemptsToCap,
			seed: func(t *testing.T, currentRun *pipeline.Run, taskID string) {
				t.Helper()
				capErr := observePressureCap(t, currentRun, taskID)
				if err := currentRun.State.PauseForTaskCap(
					taskID,
					fmt.Sprintf(
						"Task %s attempt cap reached (%d >= %d). "+
							"Respond with retry or abort.",
						taskID,
						capErr.Current,
						capErr.Maximum,
					),
				); err != nil {
					t.Fatal(err)
				}
			},
			signals: []string{"PENDING · task_cap"},
		},
		{
			// The abort branch of task_cap: the operator answers "abort", ResumePending
			// fails the run and writes its own LastError (transition.go:230-240). Using
			// that instead of a hand-written string is what makes the message causally
			// consistent with the attempt counter — a string saying "second attempt" at
			// Attempt=1 was not something the cycle could have produced
			// (review T13b b4 f3). Measured at 42 cells, so it still hardwraps to the two
			// rows this case exists to spend.
			name:     "aborted after the attempt cap",
			attempts: pressureAttemptsToCap,
			seed: func(t *testing.T, currentRun *pipeline.Run, taskID string) {
				t.Helper()
				observePressureCap(t, currentRun, taskID)
				if err := currentRun.State.PauseForTaskCap(
					taskID,
					"Task T1 attempt cap reached. Respond with retry or abort.",
				); err != nil {
					t.Fatal(err)
				}
				abort := "abort"
				if _, err := currentRun.State.ResumePending(&abort); err != nil {
					t.Fatal(err)
				}
			},
			signals: []string{
				"task T1 aborted after reaching attempt cap",
				"T1 · failed",
			},
		},
		{
			// The fixer HEAD mismatch (task_evidence.go:510-521), reached before the next
			// BeginTaskAttempt when HEAD no longer matches the recorded candidate. Its
			// length is not invented: two full 40-character SHAs make it ~131 cells, which
			// hardwraps to six rows and leaves fewer than three for the section. That is
			// the other half of the R2 contract — the section is dropped entirely rather
			// than pushing a signal off — and it was previously only ever checked by
			// calling renderChangedFiles directly.
			name:     "fixer HEAD mismatch long enough to drop the section",
			attempts: 1,
			seed: func(t *testing.T, currentRun *pipeline.Run, taskID string) {
				t.Helper()
				task := currentRun.State.Tasks[taskID]
				if err := currentRun.State.Fail(fmt.Sprintf(
					"pipeline: fixer HEAD %s does not match candidate_sha %s",
					*task.BaseSHA,
					*task.CandidateSHA,
				)); err != nil {
					t.Fatal(err)
				}
			},
			signals: []string{"pipeline: fixer HEAD"},
			yields:  true,
		},
	} {
		// Witness note (measured, not assumed). Both budget mutations — body-only budget,
		// and chrome-not-charged — are caught by the **abort** case and by the **fixer HEAD
		// mismatch** case, and by neither of them alone would be enough: the first spends
		// two error rows and still expects the section, the second spends six and expects
		// it gone, so together they pin both sides of the R2 boundary. The task_cap chip is
		// a single row, so a wrong budget still happens to fit and that case passes both
		// mutations; it is kept because it witnesses what the other two cannot — that a
		// *pending* signal is not displaced.
		t.Run("signals survive: "+pressure.name, func(t *testing.T) {
			candidate := pressureModelFromRealRun(t, pressure.attempts, pressure.seed)

			rail := ansi.Strip(renderSidebar(
				candidate,
				sidebarWidth,
				candidate.height-topBarHeight-2,
			))
			frame := ansi.Strip(renderDashboard(candidate))
			for _, surface := range []struct {
				name     string
				rendered string
			}{{"rail", rail}, {"composed rail", railColumn(frame)}} {
				// Signals are hardwrapped to the rail width, and a wrap can fall *inside*
				// a word ("secon" / "d attempt"). Joining on spaces would not rejoin that,
				// so compare with the whitespace and the card's own verticals removed.
				flat := flattenRail(surface.rendered)
				for _, signal := range pressure.signals {
					if !strings.Contains(flat, flattenRail(signal)) {
						t.Fatalf("%s: the file list displaced %q:\n%s",
							surface.name, signal, surface.rendered)
					}
				}
				if pressure.yields {
					// The signal took the rows, so the section is gone entirely — not
					// half-rendered, and not pushing the signal off the card.
					if strings.Contains(surface.rendered, "CHANGED") {
						t.Fatalf("%s: the section should have yielded to the signal:\n%s",
							surface.name, surface.rendered)
					}
					if !strings.Contains(surface.rendered, "╰") {
						t.Fatalf("%s: the STATUS card does not close:\n%s",
							surface.name, surface.rendered)
					}
					continue
				}
				// And the section itself is present — otherwise this proves nothing about
				// yielding.
				if !strings.Contains(surface.rendered, "CHANGED") {
					t.Fatalf("%s: the file list is absent, so the budget was not "+
						"exercised:\n%s", surface.name, surface.rendered)
				}
				shown, hidden := changedFilesAccounting(t, surface.name, surface.rendered)
				if shown < 1 || shown+hidden != pressureChangedFileCount {
					t.Fatalf("%s: %d shown + %d hidden does not account for %d files:\n%s",
						surface.name, shown, hidden, pressureChangedFileCount,
						surface.rendered)
				}
				// And the card still closes after the section. 26 rows is the whole rail,
				// borders included, so a list that only fits by pushing the STATUS card's
				// bottom edge past the cut has not fit — it just moved the damage.
				if _, after, found := strings.Cut(
					surface.rendered,
					"hidden) · +9/-4",
				); !found || !strings.Contains(after, "╰") {
					t.Fatalf("%s: the STATUS card does not close after the file list:\n%s",
						surface.name, surface.rendered)
				}
			}
			if rows := countRows(rail); rows > sidebarRowBudget {
				t.Fatalf("the rail overflowed to %d rows:\n%s", rows, rail)
			}
			if rows := countRows(frame); rows != candidate.height {
				t.Fatalf("the frame is %d rows, want %d:\n%s",
					rows, candidate.height, frame)
			}
		})
	}
}

// pressureChangedFileCount is how many files the pressure repository changes. Nine is past
// maxChangedFilesShown, so the hidden count is exercised whenever the section renders at all
// — the yielding case drops the section and with it the count — and one addition plus
// alternating deletions keeps `9 files (M hidden) · +9/-4` at exactly the rail's 26 inner
// cells. A wider total would be truncated and could not be asserted whole: the +45/-36 the
// first draft produced needs 28.
const pressureChangedFileCount = 9

// pressureAttemptsToCap is how many completed attempts leave the next BeginTaskAttempt at
// the cap. integrationConfig sets MaxTaskAttempts to 3, and the seed asserts the cap it
// actually observes rather than trusting this number.
const pressureAttemptsToCap = 3

// pressureModelFromRealRun hand-builds a run state, validates it through the state API, and
// then loads it into the model along the **production** status → artifact → render path: a
// real repository with real commits, phase and task transitions driven through the
// transition API, `seed` applying the transition under test, a real controller.Status, and
// the real EventStateSnapshot → loadArtifactsCommand → artifactsLoadedMsg chain with its Cmd
// executed.
//
// Be precise about what that does and does not buy (review T13b b6 f1). It does **not**
// re-run TaskCycle — see the scope note in the attempt loop — and several fields the cycle
// owns (ApprovedPlanHash, CurrentTaskID, BaseSHA, CandidateSHA, the evidence pointers) are
// constructed here directly. So this is a *seeded* state that the state API accepts and the
// production render path consumes, not proof that the pipeline would have produced it; the
// individual reachability facts it reproduces are cited at each step instead.
func pressureModelFromRealRun(
	t *testing.T,
	attempts int,
	seed func(*testing.T, *pipeline.Run, string),
) model {
	t.Helper()
	const runID = "run-pressure"
	const taskID = "T1"

	root, config := newIntegrationRepository(t)
	// The files have to exist in the base commit: a brand-new file reports `1\t0`, and the
	// totals need deletions too so a swapped additions/deletions pair cannot pass.
	for index := 0; index < pressureChangedFileCount; index++ {
		writePressureFile(t, root, index, pressureFileBody(index, 0, attempts))
	}
	integrationGit(t, root, "add", "-A")
	commitPressureTree(t, root, "test: pressure base")

	currentRun := createIntegrationRun(t, root, runID, config)
	currentRun.State.TaskOrder = []string{taskID}
	currentRun.State.Tasks = map[string]*state.TaskState{
		taskID: {Status: state.TaskOpen},
	}
	// planning -> awaiting_approval -> implementing is the only route the phase table
	// allows, and open -> candidate -> repairing the only route to a repairing task
	// (state/transition.go). Driving both through the real API is what makes this state
	// reachable rather than merely well-formed.
	if err := currentRun.State.TransitionPhase(
		state.PhaseAwaitingApproval,
	); err != nil {
		t.Fatal(err)
	}
	// implementing is reached only through controller.Approve, which freezes plan.md,
	// copies PlanHash into ApprovedPlanHash and re-verifies it before transitioning
	// (control.go:145-170). Skipping that left `implementing` with no approved plan hash
	// and a writable plan — a state the pipeline cannot produce. The exported
	// VerifyApprovedPlan is called here to prove the seeded run satisfies the real
	// invariant rather than merely looking plausible.
	approved := *currentRun.State.PlanHash
	currentRun.State.ApprovedPlanHash = &approved
	// freezePlan clears the write bits and preserves the rest — `originalMode &^ 0o222`,
	// which is 0o400 for the 0o600 plan createIntegrationRun writes (control.go:515-518).
	// Chmod'ing to a flat 0o444 would also *grant* group and other read, which
	// VerifyApprovedPlan does not check because it only looks for write bits
	// (review T13b b4 f2).
	planPath := filepath.Join(currentRun.Dir, "plan.md")
	planInfo, err := os.Stat(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(planPath, planInfo.Mode().Perm()&^0o222); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.VerifyApprovedPlan(currentRun); err != nil {
		t.Fatal(err)
	}
	if err := currentRun.State.TransitionPhase(state.PhaseImplementing); err != nil {
		t.Fatal(err)
	}
	active := taskID
	currentRun.State.CurrentTaskID = &active

	// SCOPE (settled in review T13b b5): this is a **view** test, and what it owes is a
	// status the pipeline could have persisted — validated by the real state API, with real
	// artifacts loaded from a real repository. It does *not* claim to reproduce TaskCycle's
	// own sequencing; driving the real cycle with a fake executor belongs in
	// internal/pipeline, and the reviewer ruled it out of scope here.
	//
	// So each round below is a stand-in for a completed dirty attempt, not a replay of one:
	// Attempt moves only through BeginTaskAttempt (the cap-enforcing API), the task walks
	// open -> candidate -> repairing through TransitionTask, and every round commits
	// something different because the fixer postcondition requires a new candidate.
	//
	// Measured limit of this witness: replacing the BeginTaskAttempt call with a bare
	// `Attempt++` still passes, because the state it leaves is identical — going through the
	// cap-enforcing API is not observable in the resulting state. What *is* observable, and
	// is asserted by the task_cap seed above, is that the API refuses the next attempt with
	// a CapError of exactly MaxTaskAttempts.
	// BaseSHA is written **once**, before the first attempt, as implementTask does
	// (task_cycle.go:163-168). The repair path never touches it — task_evidence.go only
	// reads BaseSHA and assigns CandidateSHA (:633) — so rewriting it per attempt described
	// a chain production cannot produce, and made the cumulative diff look smaller than it
	// was (review T13b b5 f1).
	firstBase := integrationGit(t, root, "rev-parse", "HEAD")
	currentRun.State.Tasks[taskID].BaseSHA = &firstBase
	for attempt := 1; attempt <= attempts; attempt++ {
		task := currentRun.State.Tasks[taskID]
		if err := currentRun.State.BeginTaskAttempt(
			taskID,
			currentRun.Config.MaxTaskAttempts,
			nil,
		); err != nil {
			t.Fatal(err)
		}
		// Each repair produces a **different** commit: the fixer postcondition requires
		// HEAD to differ from the previous candidate, so reusing one SHA for every attempt
		// described a repair that changed nothing (review T13b b4 f1). The per-file
		// transformation converts one slice of files per round, so the cumulative
		// base..candidate diff **reaches** the nine files at +9/-4 the rail total asserts on
		// the last round — earlier rounds are smaller and are never rendered — with BaseSHA
		// left at the first attempt's base as production leaves it.
		for index := 0; index < pressureChangedFileCount; index++ {
			writePressureFile(t, root, index, pressureFileBody(index, attempt, attempts))
		}
		integrationGit(t, root, "add", "-A")
		commitPressureTree(t, root, fmt.Sprintf("test: pressure attempt %d", attempt))
		attemptCandidate := integrationGit(t, root, "rev-parse", "HEAD")
		if task.CandidateSHA != nil && attemptCandidate == *task.CandidateSHA {
			t.Fatal("the repair produced no new commit")
		}
		if attemptCandidate == firstBase {
			t.Fatal("the attempt produced no new commit")
		}
		task.CandidateSHA = &attemptCandidate
		if err := currentRun.State.TransitionTask(
			taskID,
			state.TaskCandidate,
		); err != nil {
			t.Fatal(err)
		}
		writePressureEvidence(t, currentRun, taskID, attemptCandidate)
		if err := currentRun.State.TransitionTask(
			taskID,
			state.TaskRepairing,
		); err != nil {
			t.Fatal(err)
		}
	}

	seed(t, currentRun, taskID)
	if err := currentRun.SaveState(); err != nil {
		t.Fatal(err)
	}

	statuses, err := pipeline.NewController(newBlockingPlanExecutor()).Status(
		context.Background(),
		root,
		runID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("controller returned %d statuses, want 1", len(statuses))
	}

	current := testModel(t, &fakeUIControl{})
	current.width, current.height = wideBreakpointWidth, wideBreakpointHeight
	current.repoRoot = root
	updated, command := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:     pipeline.EventStateSnapshot,
		Status:   &statuses[0],
		RepoRoot: root,
	}})
	if command == nil {
		t.Fatal("the state snapshot did not ask for the artifacts")
	}
	loaded, ok := command().(artifactsLoadedMsg)
	if !ok {
		t.Fatalf("the artifact command returned %T", command())
	}
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	final, _ := updated.(model).Update(loaded)
	ready := final.(model)
	if len(ready.artifacts.ChangedFiles) != pressureChangedFileCount {
		t.Fatalf("the real artifact load produced %d changed files, want %d: %#v",
			len(ready.artifacts.ChangedFiles), pressureChangedFileCount,
			ready.artifacts.ChangedFiles)
	}
	// The attempt count the fixture seeded has to survive all the way into the rendered
	// status, otherwise the fixture proves nothing about the state it claims to be in
	// (review T13b b3 f1). It is `attempts` because BeginTaskAttempt ran once per seeded
	// round and the capped call does not increment.
	rendered, exists := ready.status.Tasks[taskID]
	if !exists {
		t.Fatalf("the rendered status lost task %s: %#v", taskID, ready.status.Tasks)
	}
	if rendered.Attempt != attempts {
		t.Fatalf("the rendered task is on attempt %d, want the %d the fixture seeded",
			rendered.Attempt, attempts)
	}
	// The task's final status is the *case's* business — abort legitimately ends on
	// `failed` while a cap pause ends on `repairing` — so each case asserts it on the rail
	// instead of the helper insisting on one of them.
	if rendered.BaseSHA == nil || rendered.CandidateSHA == nil ||
		rendered.GateResult == nil || rendered.ReviewResult == nil {
		t.Fatalf("the rendered task lost its evidence: %#v", rendered)
	}
	if ready.status.PlanRound == 0 || ready.status.RunID != runID {
		t.Fatalf("the rendered status is not the seeded run: %#v", ready.status)
	}
	return ready
}

// observePressureCap runs the attempt that must report the cap and returns the CapError,
// mirroring task_cycle.go:167-190: the *next* attempt is what reports the cap, and that
// CapError is what pauses. Pausing at an arbitrary attempt count would be well-formed but
// unreachable, so the cap is observed rather than assumed.
func observePressureCap(
	t *testing.T,
	currentRun *pipeline.Run,
	taskID string,
) *state.CapError {
	t.Helper()
	var capErr *state.CapError
	err := currentRun.State.BeginTaskAttempt(
		taskID,
		currentRun.Config.MaxTaskAttempts,
		nil,
	)
	if !errors.As(err, &capErr) {
		t.Fatalf("the next attempt did not report the cap: %v", err)
	}
	if capErr.Current != currentRun.Config.MaxTaskAttempts ||
		capErr.Maximum != currentRun.Config.MaxTaskAttempts {
		t.Fatalf("cap reported %d/%d, want the configured %d",
			capErr.Current, capErr.Maximum, currentRun.Config.MaxTaskAttempts)
	}
	return capErr
}

// pressureFileBody is the content of one pressure file after `attempt` commits, out of
// `attempts` in total. Each commit converts the next slice of files from their base content
// to their final content, so every commit differs from the one before it while the
// cumulative base..candidate diff **at the last commit** is the same nine files at +9/-4
// whatever the attempt count is.
//
// The intermediate commits are deliberately *not* that value — at attempts=3 they are
// 3 files +3/-1 and 6 files +6/-3 (review T13b b6 f2). Only the final state is rendered, so
// only the final arithmetic is what the rail total asserts.
//
// Accumulating a change in every file on every commit instead would make the cumulative
// diff grow with the attempt count (+19/-4 at three attempts), which is what the fixture
// used to report only because it also rewrote BaseSHA on each repair — something production
// never does (review T13b b5 f1).
func pressureFileBody(index, attempt, attempts int) string {
	converted := index*max(1, attempts)/pressureChangedFileCount + 1
	if attempt < converted {
		return "one\ntwo\n"
	}
	if index%2 == 1 {
		// One line added and one replaced: +1/-1.
		return "one\nthree\n"
	}
	// One line added: +1/-0.
	return "one\ntwo\nthree\n"
}

// writePressureEvidence writes the gate and review artefacts a completed dirty attempt
// leaves behind, in the schema production actually requires — not the abbreviated one the
// UI decoder happens to accept (review T13b b4 f1).
//
//   - gate.json needs all seven fields; readGateEvidence rejects a missing one
//     (task_evidence.go:1030-1043), and the two log paths must name real regular files
//     inside the run directory (runRelativeRegularFile, task_evidence.go:1104-1125).
//   - review.json needs schema_version, plan_hash, task_id, candidate_sha, clean and
//     findings, and `clean` must be false **only** with at least one blocking finding —
//     `clean != (blocking == 0)` is rejected (internal/cli/result.go:286-318, :426-437).
//     A finding needs id, severity (critical|major|minor), a path:line location, issue and
//     requested_change (result.go:340-401).
//
// Neither shape is re-decoded here, and that is a layering decision rather than a
// limitation: gate decoding is private to internal/pipeline, but the review verdict *is*
// reachable through the exported cli.NewOutputAdapter(...).NewAttempt().ValidateReviewResult
// path (review T13b b5 f5). A schema round-trip belongs in internal/cli or
// internal/pipeline, where the schema lives; here the shapes are pinned by construction
// against the references above so that this test stays about rendering.
func writePressureEvidence(
	t *testing.T,
	currentRun *pipeline.Run,
	taskID string,
	candidateSHA string,
) {
	t.Helper()
	logPrefix := fmt.Sprintf("task-%s-%s-gate", taskID, candidateSHA)
	stdoutLog := filepath.Join("logs", logPrefix+".stdout.log")
	stderrLog := filepath.Join("logs", logPrefix+".stderr.log")
	writeArtifactTestFile(t, filepath.Join(currentRun.Dir, stdoutLog), []byte("ok\n"))
	writeArtifactTestFile(t, filepath.Join(currentRun.Dir, stderrLog), nil)

	gate, err := json.Marshal(map[string]any{
		"command":       currentRun.Config.GateCommand,
		"cwd":           currentRun.Config.GateCWD,
		"candidate_sha": candidateSHA,
		"exit":          0,
		"timed_out":     false,
		"stdout_log":    stdoutLog,
		"stderr_log":    stderrLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	review, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"plan_hash":      *currentRun.State.PlanHash,
		"task_id":        taskID,
		"candidate_sha":  candidateSHA,
		"clean":          false,
		"findings": []map[string]any{{
			"id":               "f1",
			"severity":         "major",
			"location":         "internal/ui/view.go:1",
			"issue":            "the candidate does not satisfy the task",
			"requested_change": "make it satisfy the task",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	gatePath := filepath.Join("tasks", taskID, "gate.json")
	reviewPath := filepath.Join("tasks", taskID, "review.json")
	writeArtifactTestFile(t, filepath.Join(currentRun.Dir, gatePath), gate)
	writeArtifactTestFile(t, filepath.Join(currentRun.Dir, reviewPath), review)
	task := currentRun.State.Tasks[taskID]
	task.GateResult = &gatePath
	task.ReviewResult = &reviewPath
}

func writePressureFile(t *testing.T, root string, index int, body string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(root, fmt.Sprintf("file-%02d.go", index)),
		[]byte(body),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func commitPressureTree(t *testing.T, root, message string) {
	t.Helper()
	integrationGit(
		t,
		root,
		"-c",
		"user.name=Coterix UI Test",
		"-c",
		"user.email=ui@example.invalid",
		"commit",
		"-qm",
		message,
	)
}

// railColumn cuts the sidebar out of a composed frame. Without it the main pane's cells
// sit between two halves of a wrapped rail row, so a signal that wrapped cannot be
// rejoined — and what composeUV actually clipped into the rail column is what the
// operator sees.
func railColumn(frame string) string {
	lines := strings.Split(frame, "\n")
	column := make([]string, 0, len(lines))
	for _, line := range lines {
		column = append(column, ansi.TruncateLeftWc(
			line,
			max(0, ansi.StringWidth(line)-sidebarWidth),
			"",
		))
	}
	return strings.Join(column, "\n")
}

// flattenRail drops whitespace and the card's verticals so a row that hardwrapped
// mid-word compares as one string.
func flattenRail(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "│", "")), "")
}

// changedFilesAccounting reads the rendered CHANGED section back: how many file rows
// survived, and how many the total row says were hidden. A truncated or clipped total
// row has no match and fails here, which is the whole point — the hidden count is the
// only place the capped files are accounted for.
func changedFilesAccounting(t *testing.T, surface, rendered string) (int, int) {
	t.Helper()
	total := regexp.MustCompile(`(\d+) files \((\d+) hidden\) · \+9/-4`)
	shown := 0
	for _, line := range strings.Split(rendered, "\n") {
		if match := total.FindStringSubmatch(line); match != nil {
			all, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatal(err)
			}
			hidden, err := strconv.Atoi(match[2])
			if err != nil {
				t.Fatal(err)
			}
			if all != 9 {
				t.Fatalf("%s: the total row counts %d files, want 9", surface, all)
			}
			return shown, hidden
		}
		// "file-" and not "file": the total row says "9 files", which must not be counted
		// as a file row.
		if strings.Contains(line, "file-") {
			shown++
		}
	}
	t.Fatalf("%s: the complete total row is missing:\n%s", surface, rendered)
	return 0, 0
}
