package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

type uiControlCall struct {
	kind     operationKind
	runID    string
	response *string
}

type fakeUIControl struct {
	calls  []uiControlCall
	result pipeline.RunStatus
	err    error
}

func (fake *fakeUIControl) Run(
	context.Context,
	string,
	string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, uiControlCall{kind: operationRun})
	return fake.result, fake.err
}

func (fake *fakeUIControl) Approve(
	_ context.Context,
	_ string,
	runID string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, uiControlCall{
		kind:  operationApprove,
		runID: runID,
	})
	return fake.result, fake.err
}

func (fake *fakeUIControl) Reject(
	_ context.Context,
	_ string,
	runID string,
	response string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, uiControlCall{
		kind:     operationReject,
		runID:    runID,
		response: pointerTo(response),
	})
	return fake.result, fake.err
}

func (fake *fakeUIControl) Resume(
	_ context.Context,
	_ string,
	runID string,
	response *string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, uiControlCall{
		kind:     operationResume,
		runID:    runID,
		response: clonePointer(response),
	})
	return fake.result, fake.err
}

func (fake *fakeUIControl) Status(
	context.Context,
	string,
	string,
) ([]pipeline.RunStatus, error) {
	return nil, fake.err
}

func TestModelStreamsBoundedActivityTailAndAttemptCompletion(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	current = feedActivityLines(t, current, maxActivityLines+25)
	// Subprocess output lands in the pinned tail bounded by maxActivityLines and
	// never accumulates in the scrolling lifecycle feed (plan T13 W2).
	if len(current.activity) != maxActivityLines {
		t.Fatalf(
			"activity buffer length=%d want=%d",
			len(current.activity),
			maxActivityLines,
		)
	}
	if len(current.logs) != 0 {
		t.Fatalf(
			"lifecycle feed must stay free of subprocess lines, got %d",
			len(current.logs),
		)
	}

	result := runner.RunResult{Exit: -1, TimedOut: true}
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:    pipeline.EventAttemptFinished,
		RunID:   "run-1",
		Step:    pipeline.StepPlanReview,
		Role:    "plan_reviewer",
		CLI:     "codex",
		Attempt: 2,
		Result:  &result,
		Err:     errors.New("idle timeout"),
	}})
	current = updated.(model)
	last := current.logs[len(current.logs)-1]
	if last.Stream != runner.StreamStderr ||
		!containsAll(last.Text, "attempt 2", "timed out", "idle timeout") {
		t.Fatalf("attempt completion log=%#v", last)
	}
}

// The tail is per-step: a new step clears it, a finished step keeps it until the
// next one starts (so gate-failure context survives), and a failed step widens
// the rendered window to the whole buffer (plan T13 W2 · R4).
func TestActivityTailStepBoundariesAndFailureWidening(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	current = feedActivityLines(t, current, 8)

	if limit := activityTailLimit(current); limit != activityTailWide {
		t.Fatalf("healthy wide limit=%d want=%d", limit, activityTailWide)
	}
	compact := current
	compact.width = wideBreakpointWidth - 1
	compact.height = wideBreakpointHeight - 1
	if limit := activityTailLimit(compact); limit != activityTailCompact {
		t.Fatalf("compact limit=%d want=%d", limit, activityTailCompact)
	}

	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind: pipeline.EventStepFinished,
		Step: pipeline.StepPlan,
		Role: "plan_writer",
	}})
	current = updated.(model)
	if len(current.activity) != 8 {
		t.Fatalf(
			"a finished step must keep its tail, got %d entries",
			len(current.activity),
		)
	}

	updated, _ = current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind: pipeline.EventStepStarted,
		Step: pipeline.StepPlanReview,
		Role: "plan_reviewer",
		CLI:  "codex",
	}})
	current = updated.(model)
	if len(current.activity) != 0 {
		t.Fatalf("new step kept %d stale tail entries", len(current.activity))
	}
	if current.activityFailed {
		t.Fatal("new step inherited the failed flag")
	}

	current = feedActivityLines(t, current, maxActivityLines)
	result := runner.RunResult{Exit: 1}
	updated, _ = current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:    pipeline.EventAttemptFinished,
		Step:    pipeline.StepPlanReview,
		Role:    "plan_reviewer",
		Attempt: 1,
		Result:  &result,
	}})
	current = updated.(model)
	if !current.activityFailed {
		t.Fatal("a nonzero exit did not mark the tail as failed")
	}
	if limit := activityTailLimit(current); limit != maxActivityLines {
		t.Fatalf("failed limit=%d want=%d", limit, maxActivityLines)
	}
}

// codex writes its progress to stderr, so the tail must NOT paint healthy output
// as failure — every line would turn red. This is exactly what R5 anticipated;
// the v4 "stderr is empty" measurement only covered claude (live-smoke finding,
// 2026-07-25). Real failures still surface in the lifecycle feed, which keeps
// stream-based marking.
func TestActivityTailDoesNotPaintStderrProgressAsFailure(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	line := runner.Line{Attempt: 1, Stream: runner.StreamStderr, Text: "boom"}
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:  pipeline.EventStepLog,
		RunID: "run-1",
		Step:  pipeline.StepPlan,
		Role:  "plan_writer",
		CLI:   "claude",
		Line:  &line,
	}})
	current = updated.(model)

	if len(current.activity) != 1 {
		t.Fatalf("activity length=%d want=1", len(current.activity))
	}
	if current.activity[0].Stream != runner.StreamStderr {
		t.Fatalf("stream=%v want stderr", current.activity[0].Stream)
	}
	rendered := ansi.Strip(
		renderActivityTail(current, 120, activityTailLimit(current)),
	)
	if !containsAll(rendered, "ACTIVITY", "boom") {
		t.Fatalf("activity tail render=%q", rendered)
	}
	if strings.Contains(rendered, "×") {
		t.Fatalf("stderr progress was marked as a failure: %q", rendered)
	}

	// The lifecycle feed still marks stderr, so genuine failures stay visible.
	feed := ansi.Strip(
		renderLogLine(current.theme, current.activity[0], 120, false),
	)
	if !strings.Contains(feed, "×") {
		t.Fatalf("lifecycle feed lost its stderr marker: %q", feed)
	}
}

// The tail must render the *newest* lines within its limit, widen to the whole
// buffer on failure, and claim its rows without dropping the lifecycle signal or
// overflowing the card (review T13a-1 f1).
func TestActivityTailRendersNewestLinesAndHeightBudget(t *testing.T) {
	const produced = 22
	oldestRetained := fmt.Sprintf("line-%02d", produced-maxActivityLines)
	newest := fmt.Sprintf("line-%02d", produced-1)

	current := feedActivityLines(t, populatedViewModel(t), produced)
	if got := current.activity[0].Text; got != oldestRetained {
		t.Fatalf(
			"oldest retained=%q want=%q — buffer must keep the newest %d lines",
			got,
			oldestRetained,
			maxActivityLines,
		)
	}
	if got := current.activity[len(current.activity)-1].Text; got != newest {
		t.Fatalf("newest retained=%q want=%q", got, newest)
	}

	wide := ansi.Strip(
		renderActivityTail(current, 120, activityTailLimit(current)),
	)
	for offset := 0; offset < activityTailWide; offset++ {
		want := fmt.Sprintf("line-%02d", produced-1-offset)
		if !strings.Contains(wide, want) {
			t.Fatalf("wide tail lacks %q:\n%s", want, wide)
		}
	}
	if cut := fmt.Sprintf(
		"line-%02d",
		produced-1-activityTailWide,
	); strings.Contains(wide, cut) {
		t.Fatalf(
			"wide tail rendered %q past its %d-line limit:\n%s",
			cut,
			activityTailWide,
			wide,
		)
	}

	compact := current
	compact.width = wideBreakpointWidth - 1
	compact.height = wideBreakpointHeight - 1
	compactTail := ansi.Strip(
		renderActivityTail(compact, 80, activityTailLimit(compact)),
	)
	if !strings.Contains(compactTail, newest) {
		t.Fatalf("compact tail lacks the newest line:\n%s", compactTail)
	}
	if cut := fmt.Sprintf(
		"line-%02d",
		produced-1-activityTailCompact,
	); strings.Contains(compactTail, cut) {
		t.Fatalf(
			"compact tail rendered %q past its %d-line limit:\n%s",
			cut,
			activityTailCompact,
			compactTail,
		)
	}

	// A failed attempt widens the window to the whole buffer so the line that
	// actually caused the failure is still on screen (R4).
	result := runner.RunResult{Exit: 2}
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:    pipeline.EventAttemptFinished,
		Step:    pipeline.StepPlan,
		Role:    "plan_writer",
		Attempt: 1,
		Result:  &result,
	}})
	afterAttempt := updated.(model)
	widened := ansi.Strip(
		renderActivityTail(afterAttempt, 120, activityTailLimit(afterAttempt)),
	)
	if !containsAll(widened, oldestRetained, newest) {
		t.Fatalf(
			"failed tail must span %q..%q:\n%s",
			oldestRetained,
			newest,
			widened,
		)
	}

	stepFailed := feedActivityLines(t, populatedViewModel(t), produced)
	updated, _ = stepFailed.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind: pipeline.EventStepFinished,
		Step: pipeline.StepPlan,
		Role: "plan_writer",
		Err:  errors.New("gate failed"),
	}})
	stepFailed = updated.(model)
	if !stepFailed.activityFailed {
		t.Fatal("EventStepFinished error did not widen the tail")
	}
	stepTail := ansi.Strip(
		renderActivityTail(stepFailed, 120, activityTailLimit(stepFailed)),
	)
	if !strings.Contains(stepTail, oldestRetained) {
		t.Fatalf("step-failure tail lacks %q:\n%s", oldestRetained, stepTail)
	}

	current.logs = []logEntry{{
		Role: "plan_writer",
		CLI:  "claude",
		Icon: logIconStart,
		Text: "plan_writer · claude started",
	}}
	frame := ansi.Strip(renderMain(current, 140, 40))
	if rows := strings.Count(frame, "\n") + 1; rows > 40 {
		t.Fatalf("main pane overflowed its height budget: %d rows want <=40", rows)
	}
	for _, want := range []string{
		"plan_writer · claude started",
		"ACTIVITY",
		newest,
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("main pane lacks %q:\n%s", want, frame)
		}
	}
}

// feedActivityLines emits numbered lines so tests can assert *which* lines
// survive the buffer and the render limit — identical text would let a
// newest/oldest mix-up pass unnoticed (review T13a-1 f1).
// f3/f4 of the follow-up review: the earlier tests checked endpoints and
// ANSI-stripped glyphs, so a renderer that emitted only the newest compact line,
// dropped the middle of a widened failure tail, or painted muted stderr red would
// still have passed. These assert the exact sets, the styling, and the real
// lifecycle path.
func TestActivityTailExactSetsStylingAndHeightBudgets(t *testing.T) {
	const produced = 22
	wide := feedActivityLines(t, populatedViewModel(t), produced)

	compact := wide
	compact.width = wideBreakpointWidth - 1
	compact.height = wideBreakpointHeight - 1
	assertActivityLines(
		t,
		renderActivityTail(compact, 80, activityTailLimit(compact)),
		produced,
		activityTailCompact,
	)
	assertActivityLines(
		t,
		renderActivityTail(wide, 120, activityTailLimit(wide)),
		produced,
		activityTailWide,
	)

	// A failed attempt widens the window to the entire buffer — every one of the
	// retained lines, in order, not just the two endpoints.
	result := runner.RunResult{Exit: 1}
	updated, _ := wide.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:    pipeline.EventAttemptFinished,
		Step:    pipeline.StepPlan,
		Role:    "plan_writer",
		Attempt: 1,
		Result:  &result,
	}})
	failed := updated.(model)
	assertActivityLines(
		t,
		renderActivityTail(failed, 120, activityTailLimit(failed)),
		produced,
		maxActivityLines,
	)

	// The lifecycle feed must carry the failure with an explicit error-styled
	// icon — the tail's muting must not reach it.
	feed := renderFeed(failed, 120)
	if !strings.Contains(ansi.Strip(feed), "×") {
		t.Fatalf("lifecycle feed lost the failure marker:\n%s", ansi.Strip(feed))
	}
	assertSameColor(
		t,
		findStyledCell(t, feed, "×").Style.Fg,
		lipgloss.Color(failed.theme.tokens.Status.Error.FG),
	)

	// Muted stderr progress in the tail keeps the neutral value color and the
	// `·` icon (not a red `×`).
	stderrModel := populatedViewModel(t)
	line := runner.Line{Attempt: 1, Stream: runner.StreamStderr, Text: "boom"}
	updated, _ = stderrModel.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:  pipeline.EventStepLog,
		RunID: "run-1",
		Step:  pipeline.StepPlan,
		Role:  "plan_writer",
		CLI:   "codex",
		Line:  &line,
	}})
	stderrModel = updated.(model)
	tail := renderActivityTail(stderrModel, 120, activityTailWide)
	assertSameColor(
		t,
		findStyledCell(t, tail, "b").Style.Fg,
		lipgloss.Color(stderrModel.theme.tokens.Theme.FGBase),
	)
	assertSameColor(
		t,
		findStyledCell(t, tail, "·").Style.Fg,
		lipgloss.Color(stderrModel.theme.tokens.Theme.FGMostSubtle),
	)

	// Height budgets: neither layout may overflow, and the lifecycle signal must
	// survive alongside a widened tail.
	for _, layout := range []struct {
		name    string
		current model
		width   int
		height  int
	}{
		{"wide healthy", wide, 140, 40},
		{"wide failed", failed, 140, 40},
		{"compact healthy", compact, 80, 22},
	} {
		t.Run(layout.name, func(t *testing.T) {
			candidate := layout.current
			candidate.logs = []logEntry{{
				Role: "plan_writer",
				CLI:  "claude",
				Icon: logIconStart,
				Text: "plan_writer · claude started",
			}}
			frame := ansi.Strip(
				renderMain(candidate, layout.width, layout.height),
			)
			if rows := strings.Count(frame, "\n") + 1; rows > layout.height {
				t.Fatalf(
					"main pane used %d rows, budget is %d:\n%s",
					rows,
					layout.height,
					frame,
				)
			}
			if !strings.Contains(frame, "plan_writer · claude started") {
				t.Fatalf("lifecycle signal was dropped:\n%s", frame)
			}
		})
	}
}

// Each box owns its scroll offset and `tab` cycles focus through the boxes that
// are actually on screen. Before T14 a single offset drove the whole pane, so
// scrolling to read the artifacts displaced the live activity and vice versa
// (T14 W1/W2).
func TestMainBoxesScrollIndependentlyAndTabCyclesFocus(t *testing.T) {
	current := feedActivityLines(t, populatedViewModel(t), 20)
	current.status.PendingAction = nil

	if current.focus != boxFeed {
		t.Fatalf("default focus=%d want boxFeed", current.focus)
	}

	// k scrolls only the focused box.
	updated, _ := current.Update(printableKey('k'))
	current = updated.(model)
	if current.boxScroll[boxFeed] != 1 {
		t.Fatalf("focused box offset=%d want 1", current.boxScroll[boxFeed])
	}
	for _, box := range []mainBox{boxLiveOutput, boxActivity} {
		if current.boxScroll[box] != 0 {
			t.Fatalf("box %d moved with an unrelated scroll", box)
		}
	}

	// tab hands scrolling to the next box; the previous offset survives.
	updated, _ = current.Update(specialKey(tea.KeyTab))
	current = updated.(model)
	if current.focus != boxLiveOutput {
		t.Fatalf("focus after tab=%d want boxLiveOutput", current.focus)
	}
	updated, _ = current.Update(printableKey('k'))
	current = updated.(model)
	if current.boxScroll[boxLiveOutput] != 1 || current.boxScroll[boxFeed] != 1 {
		t.Fatalf(
			"offsets=%v — each box must keep its own",
			current.boxScroll,
		)
	}

	// The cycle ends at the sidebar and wraps back to the feed.
	for _, want := range []mainBox{boxActivity, boxSidebar, boxFeed} {
		updated, _ = current.Update(specialKey(tea.KeyTab))
		current = updated.(model)
		if current.focus != want {
			t.Fatalf("focus=%d want %d", current.focus, want)
		}
	}

	// shift+tab walks the same cycle backwards.
	updated, _ = current.Update(tea.KeyPressMsg(tea.Key{
		Code: tea.KeyTab,
		Mod:  tea.ModShift,
	}))
	current = updated.(model)
	if current.focus != boxSidebar {
		t.Fatalf("shift+tab focus=%d want boxSidebar", current.focus)
	}

	// PENDING joins the cycle only while the run is blocked on a question.
	blocked := current
	blocked.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingTaskCap,
		Prompt: "retry or abort",
	}
	if order := mainBoxOrder(blocked); order[0] != boxPending {
		t.Fatalf("pending run must lead with boxPending, got %v", order)
	}
	if order := mainBoxOrder(current); indexOfBox(order, boxPending) >= 0 {
		t.Fatalf("unblocked run must not include boxPending: %v", order)
	}
}

// The wide pane draws one box per section and marks the focused one; the compact
// pane keeps the single-column stack with no box chrome.
func TestMainPaneRendersOneBoxPerSection(t *testing.T) {
	current := feedActivityLines(t, populatedViewModel(t), 6)
	current.logs = []logEntry{{
		Role: "plan_writer",
		CLI:  "claude",
		Icon: logIconStart,
		Text: "plan_writer · claude started",
	}}
	current.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingTaskCap,
		Prompt: "retry or abort after reviewing the evidence",
	}

	wide := ansi.Strip(renderMain(current, 140, 40))
	for _, want := range []string{
		"PENDING · task_cap",
		"retry or abort",
		"PIPELINE FEED",
		"LIVE OUTPUT",
		"plan_writer · claude started",
		"ACTIVITY",
		"line-05",
	} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide pane lacks %q:\n%s", want, wide)
		}
	}
	// Four boxes → four top borders.
	if boxes := strings.Count(wide, "╭"); boxes < 4 {
		t.Fatalf("expected one box per section, found %d:\n%s", boxes, wide)
	}

	compact := current
	compact.width = 80
	compact.height = 24
	plain := ansi.Strip(renderMain(compact, 80, 22))
	if !strings.Contains(plain, "LIVE OUTPUT") {
		t.Fatalf("compact pane lost its sections:\n%s", plain)
	}
	if !strings.Contains(plain, "retry or abort") {
		t.Fatalf("compact pane dropped the pending question:\n%s", plain)
	}
}

// The focus contract: compact has no focus concept, focus never rests on a box
// that is off screen, and the focused box carries a non-color cue as well as the
// focused border color (review T14a f3).
func TestFocusContractCompactHiddenBoxesAndCues(t *testing.T) {
	wide := feedActivityLines(t, populatedViewModel(t), 4)
	wide.status.PendingAction = nil

	t.Run("compact ignores focus keys", func(t *testing.T) {
		compact := wide
		compact.width = 80
		compact.height = 24
		compact.focus = boxFeed

		updated, _ := compact.Update(specialKey(tea.KeyTab))
		moved := updated.(model)
		if moved.focus != boxFeed {
			t.Fatalf("compact tab moved focus to %d", moved.focus)
		}
		// One scroll still drives the single column.
		updated, _ = moved.Update(printableKey('k'))
		scrolled := updated.(model)
		if scrolled.boxScroll[boxFeed] != 1 {
			t.Fatalf("compact scroll=%v want the feed offset to move", scrolled.boxScroll)
		}
	})

	t.Run("hidden focus normalizes", func(t *testing.T) {
		blocked := wide
		blocked.status.PendingAction = &state.PendingAction{
			Kind:   state.PendingTaskCap,
			Prompt: "retry or abort",
		}
		blocked.focus = boxPending
		if got := blocked.normalizedFocus(); got != boxPending {
			t.Fatalf("focus=%d want boxPending while blocked", got)
		}

		// The run resumes: PENDING leaves the screen and focus must follow.
		resumed := blocked
		resumed.status.PendingAction = nil
		if got := resumed.normalizedFocus(); got == boxPending {
			t.Fatal("focus stayed on the vanished PENDING box")
		}
		if got := resumed.scrollTarget(); got == boxPending {
			t.Fatal("j/k would drive an off-screen offset")
		}
		// Some box must still be drawn as focused.
		frame := ansi.Strip(renderMain(resumed, 140, 40))
		if !strings.Contains(frame, "▸ ") {
			t.Fatalf("no box carries the focus cue:\n%s", frame)
		}
	})

	t.Run("focus cue is not color alone", func(t *testing.T) {
		focused := wide
		focused.focus = boxLiveOutput
		frame := renderMain(focused, 140, 40)
		plain := ansi.Strip(frame)
		if !strings.Contains(plain, "▸ LIVE OUTPUT") {
			t.Fatalf("focused box lacks its non-color cue:\n%s", plain)
		}
		if strings.Contains(plain, "▸ PIPELINE FEED") {
			t.Fatalf("unfocused box carries the cue:\n%s", plain)
		}
		// And the border really uses the focused token. Assert on the box chrome
		// directly — the first `╭` in a full frame belongs to MainCard.
		assertSameColor(
			t,
			findStyledCell(
				t,
				renderBoxCard(focused.theme, "T", "body", 20, true),
				"╭",
			).Style.Fg,
			lipgloss.Color(focused.theme.tokens.Component.BorderFocused),
		)
		assertSameColor(
			t,
			findStyledCell(
				t,
				renderBoxCard(focused.theme, "T", "body", 20, false),
				"╭",
			).Style.Fg,
			lipgloss.Color(focused.theme.tokens.Theme.Separator),
		)
	})

	t.Run("sidebar takes focus chrome", func(t *testing.T) {
		side := wide
		side.focus = boxSidebar
		plain := ansi.Strip(renderSidebar(side, sidebarWidth, 26))
		if !strings.Contains(plain, "▸ PIPELINE") {
			t.Fatalf("focused sidebar lacks its cue:\n%s", plain)
		}
	})
}

// A widened failure tail, a long PENDING question and a last_error all compete
// for the same rows. Before the shared budget the compact path forced each part
// to its own minimum and overflowed, so composeUV silently cut the newest failure
// lines off the bottom (review round-3 f1).
func TestMainPaneSharesOneRowBudgetUnderPressure(t *testing.T) {
	base := feedActivityLines(t, populatedViewModel(t), maxActivityLines+5)
	result := runner.RunResult{Exit: 1}
	updated, _ := base.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:    pipeline.EventAttemptFinished,
		Step:    pipeline.StepPlan,
		Role:    "plan_writer",
		Attempt: 1,
		Result:  &result,
	}})
	base = updated.(model)
	if !base.activityFailed {
		t.Fatal("fixture must carry a widened tail")
	}
	base.status.Phase = state.PhasePausedForInput
	base.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingTaskCap,
		Prompt: strings.Repeat("retry or abort after reviewing the evidence. ", 8),
	}
	base.status.LastError = pointerTo("pending-error-marker")
	base.logs = []logEntry{{
		Role: "plan_writer",
		CLI:  "claude",
		Icon: logIconFail,
		Text: "attempt 1 exited 1",
	}}
	newest := fmt.Sprintf("line-%02d", maxActivityLines+4)

	for _, layout := range []struct {
		name   string
		modelW int
		modelH int
		width  int
		height int
	}{
		{"compact 80x24", 80, 24, 80, 20},
		{
			"minimum wide",
			wideBreakpointWidth,
			wideBreakpointHeight,
			wideBreakpointWidth - sidebarWidth,
			wideBreakpointHeight - topBarHeight - 2,
		},
	} {
		t.Run(layout.name, func(t *testing.T) {
			candidate := base
			candidate.width = layout.modelW
			candidate.height = layout.modelH
			frame := ansi.Strip(
				renderMain(candidate, layout.width, layout.height),
			)
			if rows := strings.Count(frame, "\n") + 1; rows > layout.height {
				t.Fatalf(
					"used %d rows, budget is %d:\n%s",
					rows,
					layout.height,
					frame,
				)
			}
			// The question blocking the run and the newest failure line both live.
			for _, want := range []string{"PENDING", "retry or abort", newest} {
				if !strings.Contains(frame, want) {
					t.Fatalf("lost %q under pressure:\n%s", want, frame)
				}
			}
		})
	}
}

// assertActivityLines checks that exactly the newest `limit` lines are rendered,
// in order, out of `produced` emitted lines.
func assertActivityLines(
	t *testing.T,
	rendered string,
	produced, limit int,
) {
	t.Helper()
	lines := make([]string, 0, limit)
	for _, line := range strings.Split(ansi.Strip(rendered), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, "ACTIVITY") {
			continue
		}
		lines = append(lines, trimmed)
	}
	if len(lines) != limit {
		t.Fatalf("rendered %d activity lines, want %d:\n%s", len(lines), limit, rendered)
	}
	for index, line := range lines {
		want := fmt.Sprintf("line-%02d", produced-limit+index)
		if !strings.Contains(line, want) {
			t.Fatalf(
				"activity row %d is %q, want it to contain %q (order must be preserved)",
				index,
				line,
				want,
			)
		}
	}
}

func feedActivityLines(t *testing.T, current model, count int) model {
	t.Helper()
	return feedActivityAttempt(t, current, 1, count)
}

func feedActivityAttempt(
	t *testing.T,
	current model,
	attempt, count int,
) model {
	t.Helper()
	for index := 0; index < count; index++ {
		line := runner.Line{
			Attempt: attempt,
			Stream:  runner.StreamStdout,
			Text:    fmt.Sprintf("line-%02d", index),
		}
		updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
			Kind:  pipeline.EventStepLog,
			RunID: "run-1",
			Step:  pipeline.StepPlan,
			Role:  "plan_writer",
			CLI:   "claude",
			Line:  &line,
		}})
		current = updated.(model)
	}
	return current
}

// A retry is a fresh subprocess, so attempt N+1's output must not be mixed with
// attempt N's stale tail. The pipeline emits no attempt-started event, so the
// line's own attempt number is the only boundary signal available — without this
// the tail keeps showing a finished attempt's output (and stays widened) while a
// new one is already running, which is exactly the "looks stuck" symptom W2 set
// out to remove (live-smoke finding, 2026-07-25).
func TestActivityTailResetsAcrossAttemptBoundary(t *testing.T) {
	current := feedActivityAttempt(t, testModel(t, &fakeUIControl{}), 1, 6)

	result := runner.RunResult{Exit: 1}
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:    pipeline.EventAttemptFinished,
		Step:    pipeline.StepPlan,
		Role:    "plan_writer",
		Attempt: 1,
		Result:  &result,
	}})
	current = updated.(model)
	if !current.activityFailed || len(current.activity) != 6 {
		t.Fatalf(
			"a failed attempt must keep its widened tail: failed=%v len=%d",
			current.activityFailed,
			len(current.activity),
		)
	}

	current = feedActivityAttempt(t, current, 2, 1)
	if len(current.activity) != 1 {
		t.Fatalf(
			"attempt 2 kept %d stale lines from attempt 1",
			len(current.activity),
		)
	}
	if got := current.activity[0].Attempt; got != 2 {
		t.Fatalf("retained line attempt=%d want=2", got)
	}
	if current.activityFailed {
		t.Fatal("attempt 2 inherited attempt 1's failed flag — tail stays widened")
	}
	if limit := activityTailLimit(current); limit != activityTailWide {
		t.Fatalf("limit=%d want=%d after the boundary reset", limit, activityTailWide)
	}
}

// A running step with no output yet must still say so in the content pane —
// leaving it blank made a healthy run look stuck (live-smoke finding).
func TestActivityWaitingStateFillsContentPane(t *testing.T) {
	current := populatedViewModel(t)
	current.activity = nil

	frame := ansi.Strip(renderMain(current, 140, 40))
	for _, want := range []string{
		"ACTIVITY",
		"plan_writer · claude",
		"waiting for the first line",
		".coterix/runs/run-9/logs/",
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("waiting state lacks %q:\n%s", want, frame)
		}
	}

	// Once real output arrives the placeholder gives way to the lines.
	current = feedActivityLines(t, current, 2)
	frame = ansi.Strip(renderMain(current, 140, 40))
	if strings.Contains(frame, "waiting for the first line") {
		t.Fatalf("placeholder survived real output:\n%s", frame)
	}
	if !strings.Contains(frame, "line-01") {
		t.Fatalf("activity lines missing:\n%s", frame)
	}

	// No step running → no placeholder (a finished run must not look busy).
	idle := populatedViewModel(t)
	idle.activity = nil
	idle.activeStep = ""
	idle.activeRole = ""
	idle.activeCLI = ""
	if got := renderActivityTail(idle, 120, activityTailWide); got != "" {
		t.Fatalf("idle model rendered a waiting state: %q", got)
	}
}

func TestWorkingAnimationTicksOnlyWhileActive(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	current.operation = operationRun
	current.artifactRender = "cached-artifact"

	previousFrame := current.spinner.View()
	previousColor := firstStyledCell(t, previousFrame).Style.Fg
	for tick := 0; tick < 2; tick++ {
		updated, command := current.Update(current.spinner.Tick())
		current = updated.(model)
		if command == nil {
			t.Fatalf("active tick %d did not schedule the next frame", tick)
		}
		frame := current.spinner.View()
		if frame == previousFrame {
			t.Fatalf("active tick %d did not change the frame", tick)
		}
		color := firstStyledCell(t, frame).Style.Fg
		if rgba(color) == rgba(previousColor) {
			t.Fatalf("active tick %d did not change the frame color", tick)
		}
		if current.artifactRender != "cached-artifact" {
			t.Fatalf(
				"active tick %d rerendered cached artifacts: %q",
				tick,
				current.artifactRender,
			)
		}
		previousFrame = frame
		previousColor = color
	}

	current.activeStep = pipeline.StepImplementation
	current.activeRole = "impl_writer"
	current.activeCLI = "codex"
	updated, command := current.Update(operationDoneMsg{})
	current = updated.(model)
	if command != nil || current.isWorking() ||
		current.activeRole != "" || current.activeCLI != "" {
		t.Fatalf(
			"idle transition command=%v working=%t role=%q cli=%q",
			command,
			current.isWorking(),
			current.activeRole,
			current.activeCLI,
		)
	}
	idleFrame := current.spinner.View()
	updated, command = current.Update(current.spinner.Tick())
	current = updated.(model)
	if command != nil {
		t.Fatal("idle spinner tick scheduled another tick")
	}
	if current.spinner.View() != idleFrame {
		t.Fatal("idle spinner tick changed the frame")
	}
	if current.artifactRender != "cached-artifact" {
		t.Fatal("idle spinner tick changed the artifact render cache")
	}
}

func TestInitStartsOperationAndWorkingTick(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	current.operation = operationRun

	message := current.Init()()
	batch, ok := message.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init command returned %T, want tea.BatchMsg", message)
	}
	var operationStarted bool
	var tickStarted bool
	for _, command := range batch {
		switch command().(type) {
		case operationDoneMsg:
			operationStarted = true
		case spinner.TickMsg:
			tickStarted = true
		}
	}
	if !operationStarted || !tickStarted {
		t.Fatalf(
			"Init batch operation=%t tick=%t",
			operationStarted,
			tickStarted,
		)
	}
}

func TestArtifactStatusKeyChangesAtPlanReviewBoundary(t *testing.T) {
	status := pipeline.RunStatus{
		RunID:     "run-1",
		Phase:     state.PhasePlanning,
		PlanRound: 1,
		PlanHash:  pointerTo("plan-hash"),
	}
	planning := artifactStatusKey(status)
	status.Phase = state.PhaseAwaitingApproval
	if reviewed := artifactStatusKey(status); reviewed == planning {
		t.Fatal("artifact key did not distinguish planning from reviewed plan")
	}
	status.Phase = state.PhasePlanning
	status.PlanRound = 2
	if revised := artifactStatusKey(status); revised == planning {
		t.Fatal("artifact key did not distinguish plan rounds")
	}
}

func TestStateSnapshotUsesCanonicalRepositoryRootForArtifacts(t *testing.T) {
	repoRoot, runDir, runID := newArtifactTestRun(t)
	if err := os.WriteFile(
		filepath.Join(runDir, "plan.md"),
		[]byte("# Canonical plan\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repoRoot, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}

	current := testModel(t, &fakeUIControl{})
	current.repoRoot = nested
	status := pipeline.RunStatus{
		RunID: runID,
		Phase: state.PhaseAwaitingApproval,
	}
	updated, command := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:     pipeline.EventStateSnapshot,
		RepoRoot: repoRoot,
		RunID:    runID,
		Status:   &status,
	}})
	current = updated.(model)
	if current.repoRoot != repoRoot || command == nil {
		t.Fatalf(
			"snapshot root=%q command=%v, want %q and artifact load",
			current.repoRoot,
			command,
			repoRoot,
		)
	}
	loaded := command().(artifactsLoadedMsg)
	if loaded.err != nil ||
		!strings.Contains(loaded.data.PlanMarkdown, "Canonical plan") {
		t.Fatalf("canonical artifact load = %#v", loaded)
	}
}

func TestModelWaitsForActiveCoreOperationBeforeQuit(t *testing.T) {
	cancelled := false
	current := testModel(t, &fakeUIControl{})
	current.cancel = func() {
		cancelled = true
	}
	current.operation = operationApprove

	updated, command := current.Update(printableKey('q'))
	current = updated.(model)
	if command != nil || !current.stopping || !cancelled {
		t.Fatalf(
			"quit during operation: command=%v stopping=%t cancelled=%t",
			command,
			current.stopping,
			cancelled,
		)
	}

	updated, command = current.Update(operationDoneMsg{
		kind: operationApprove,
		err:  context.Canceled,
	})
	current = updated.(model)
	if command == nil {
		t.Fatal("completed cancellation did not quit the TUI")
	}
	message := command()
	if _, ok := message.(tea.QuitMsg); !ok {
		t.Fatalf("completion command returned %T, want tea.QuitMsg", message)
	}
	if !errors.Is(current.operationErr, context.Canceled) {
		t.Fatalf("operation cancellation error = %v", current.operationErr)
	}
}

func TestModelQuitsImmediatelyAtControlBoundary(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	updated, command := current.Update(printableKey('q'))
	current = updated.(model)
	if current.stopping || command == nil {
		t.Fatalf(
			"boundary quit: stopping=%t command=%v",
			current.stopping,
			command,
		)
	}
	message := command()
	if _, ok := message.(tea.QuitMsg); !ok {
		t.Fatalf("boundary command returned %T, want tea.QuitMsg", message)
	}
}

func TestModelApprovalRejectAndPendingActionsUseControlPlane(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		fake := &fakeUIControl{
			result: pipeline.RunStatus{
				RunID: "run-1",
				Phase: state.PhaseImplementing,
			},
		}
		current := testModel(t, fake)
		current.hasStatus = true
		current.status = pipeline.RunStatus{
			RunID: "run-1",
			Phase: state.PhaseAwaitingApproval,
		}

		updated, command := current.Update(printableKey('a'))
		current = updated.(model)
		if command == nil || current.operation != operationApprove {
			t.Fatal("approve key did not start a control operation")
		}
		done := operationDoneFromCommand(t, command)
		if done.status.Phase != state.PhaseImplementing {
			t.Fatalf("approve result phase=%s", done.status.Phase)
		}
		assertUICall(t, fake, operationApprove, "run-1", nil)
	})

	t.Run("reject confirm and cancel", func(t *testing.T) {
		fake := &fakeUIControl{}
		current := testModel(t, fake)
		current.hasStatus = true
		current.status = pipeline.RunStatus{
			RunID: "run-2",
			Phase: state.PhaseAwaitingApproval,
		}

		updated, command := current.Update(printableKey('r'))
		current = updated.(model)
		if command != nil || current.prompt != promptReject {
			t.Fatal("reject key did not open the minimal response prompt")
		}
		updated, command = current.Update(specialKey(tea.KeyEscape))
		current = updated.(model)
		if command != nil || current.prompt != promptNone || len(fake.calls) != 0 {
			t.Fatal("escape did not cancel reject prompt")
		}

		updated, _ = current.Update(printableKey('r'))
		current = updated.(model)
		updated, _ = current.Update(tea.PasteMsg{Content: "revise the gate"})
		current = updated.(model)
		updated, command = current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		if command == nil || current.prompt != promptNone {
			t.Fatal("reject confirmation did not start the core operation")
		}
		_ = operationDoneFromCommand(t, command)
		response := "revise the gate"
		assertUICall(t, fake, operationReject, "run-2", &response)
	})

	t.Run("pending response and auth resume", func(t *testing.T) {
		fake := &fakeUIControl{}
		current := testModel(t, fake)
		current.hasStatus = true
		current.status = pipeline.RunStatus{
			RunID: "run-3",
			Phase: state.PhasePausedForInput,
			PendingAction: &state.PendingAction{
				Kind:        state.PendingPlanQuestion,
				ResumePhase: state.PhasePlanning,
				Prompt:      "Choose a target",
			},
		}

		updated, _ := current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		updated, _ = current.Update(tea.PasteMsg{Content: "target A"})
		current = updated.(model)
		updated, command := current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		if command == nil {
			t.Fatal("pending response did not start resume")
		}
		_ = operationDoneFromCommand(t, command)
		response := "target A"
		assertUICall(t, fake, operationResume, "run-3", &response)

		fake.calls = nil
		current.operation = ""
		current.status.PendingAction = &state.PendingAction{
			Kind:        state.PendingAuth,
			ResumePhase: state.PhasePlanning,
			Prompt:      "Log in, then resume",
		}
		updated, command = current.Update(specialKey(tea.KeyEnter))
		if command == nil {
			t.Fatal("auth enter did not resume")
		}
		_ = updated
		_ = operationDoneFromCommand(t, command)
		assertUICall(t, fake, operationResume, "run-3", nil)
	})
}

func testModel(t *testing.T, control controlPlane) model {
	t.Helper()
	currentTheme, err := loadTheme()
	if err != nil {
		t.Fatal(err)
	}
	current := newModel(
		context.Background(),
		func() {},
		control,
		t.TempDir(),
		"request",
		currentTheme,
		false,
	)
	current.operation = ""
	return current
}

func printableKey(character rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{
		Code: character,
		Text: string(character),
	})
}

func specialKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code})
}

func assertUICall(
	t *testing.T,
	fake *fakeUIControl,
	kind operationKind,
	runID string,
	response *string,
) {
	t.Helper()
	if len(fake.calls) != 1 {
		t.Fatalf("control calls=%#v, want one", fake.calls)
	}
	call := fake.calls[0]
	if call.kind != kind || call.runID != runID ||
		!equalPointers(call.response, response) {
		t.Fatalf(
			"control call=%#v, want kind=%s run=%s response=%v",
			call,
			kind,
			runID,
			response,
		)
	}
}

func pointerTo(value string) *string {
	return &value
}

func clonePointer(value *string) *string {
	if value == nil {
		return nil
	}
	return pointerTo(*value)
}

func equalPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

func operationDoneFromCommand(t *testing.T, command tea.Cmd) operationDoneMsg {
	t.Helper()
	if command == nil {
		t.Fatal("nil command")
	}
	return operationDoneFromMessage(t, command())
}

func operationDoneFromMessage(t *testing.T, message tea.Msg) operationDoneMsg {
	t.Helper()
	switch message := message.(type) {
	case operationDoneMsg:
		return message
	case tea.BatchMsg:
		for _, command := range message {
			if command == nil {
				continue
			}
			switch child := command().(type) {
			case operationDoneMsg:
				return child
			case tea.BatchMsg:
				return operationDoneFromMessage(t, child)
			}
		}
		t.Fatal("batch command did not contain an operation result")
	default:
		t.Fatalf("command returned %T, want operation result", message)
	}
	return operationDoneMsg{}
}
