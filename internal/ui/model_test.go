package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Reading history must survive arriving content: the offset is measured from the
// newest line, so it has to be nudged as lines land or the window slides one row
// per line and the user loses their place. The box also has to say there is more
// below, and `end` must bring it back to the live edge (review T14a f2).
func TestPausedBoxKeepsItsLinesWhenContentArrives(t *testing.T) {
	const windowHeight = 4
	current := populatedViewModel(t)
	appendSteps := func(from, count int) {
		for index := from; index < from+count; index++ {
			updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
				Kind: pipeline.EventStepStarted,
				Step: pipeline.StepPlan,
				Role: fmt.Sprintf("role-%02d", index),
				CLI:  "claude",
			}})
			current = updated.(model)
		}
	}
	appendSteps(0, 12)
	current.focus = boxLiveOutput

	// Scroll back into history.
	for index := 0; index < 3; index++ {
		updated, _ := current.Update(printableKey('k'))
		current = updated.(model)
	}
	offsetBefore := current.boxScroll[boxLiveOutput]
	if offsetBefore == 0 {
		t.Fatal("k did not move the lifecycle box off the live edge")
	}
	window := func() string {
		return visibleLines(
			lifecycleBody(current, 100),
			windowHeight,
			current.boxScroll[boxLiveOutput],
		)
	}
	before := ansi.Strip(window())

	// Two more entries land while the user is reading.
	appendSteps(12, 2)
	if after := ansi.Strip(window()); after != before {
		t.Fatalf(
			"paused view drifted when content arrived:\nbefore:\n%s\nafter:\n%s",
			before,
			after,
		)
	}
	if current.boxScroll[boxLiveOutput] != offsetBefore+2 {
		t.Fatalf(
			"offset=%d want=%d — it must grow with the new rows",
			current.boxScroll[boxLiveOutput],
			offsetBefore+2,
		)
	}

	// The box announces that there is more below.
	frame := ansi.Strip(renderMain(current, 140, 40))
	if !strings.Contains(frame, "↓ new") {
		t.Fatalf("paused box does not announce new content:\n%s", frame)
	}

	// end returns to the live edge and clears the cue.
	updated, _ := current.Update(specialKey(tea.KeyEnd))
	current = updated.(model)
	if current.boxScroll[boxLiveOutput] != 0 {
		t.Fatalf("end left offset=%d", current.boxScroll[boxLiveOutput])
	}
	if frame = ansi.Strip(renderMain(current, 140, 40)); strings.Contains(frame, "↓ new") {
		t.Fatalf("cue survived a return to the live edge:\n%s", frame)
	}
}

// The LIVE OUTPUT fix was not enough: FEED's body is replaced wholesale and
// ACTIVITY renders a rolling tail, so a bumped offset still landed on different
// lines in both boxes. Each path is pinned separately (review T14a-r2 f1).
func TestPausedFeedAndActivityKeepTheirLinesToo(t *testing.T) {
	const windowHeight = 3

	t.Run("feed follows an appended artifact", func(t *testing.T) {
		current := populatedViewModel(t)
		previous := strings.Join([]string{
			"plan-00", "plan-01", "plan-02", "plan-03",
			"plan-04", "plan-05", "plan-06", "plan-07",
		}, "\n")
		current.artifactRender = previous
		current.boxScroll[boxFeed] = 2
		before := visibleLines(previous, windowHeight, 2)

		next := previous + "\nplan-08\nplan-09"
		current.artifactRender = next
		current.reanchorFeed(previous, next)

		if got := current.boxScroll[boxFeed]; got != 4 {
			t.Fatalf("feed offset=%d want 4 — it must grow with the appended rows", got)
		}
		after := visibleLines(next, windowHeight, current.boxScroll[boxFeed])
		if after != before {
			t.Fatalf(
				"paused feed drifted when an artifact was appended:\nbefore:\n%s\nafter:\n%s",
				before,
				after,
			)
		}
	})

	// A re-wrap or an edited artifact shares no line identity with the old body,
	// so the offset must not park the window at an arbitrary new position.
	t.Run("feed returns to the live edge on a real replacement", func(t *testing.T) {
		current := populatedViewModel(t)
		current.boxScroll[boxFeed] = 5
		current.reanchorFeed("plan-00\nplan-01", "rewrapped-00\nrewrapped-01\nrewrapped-02")
		if got := current.boxScroll[boxFeed]; got != 0 {
			t.Fatalf("feed offset=%d want 0 after an incompatible replacement", got)
		}
	})

	// The width change that re-renders the markdown goes through the same path.
	t.Run("a width change re-anchors the feed", func(t *testing.T) {
		current := populatedViewModel(t)
		current.artifacts.PlanMarkdown = strings.Repeat("plan body text ", 40)
		current.refreshArtifactRender()
		current.boxScroll[boxFeed] = 6

		current.width = wideBreakpointWidth + 40
		current.refreshArtifactRender()
		if got := current.boxScroll[boxFeed]; got != 0 {
			t.Fatalf("feed offset=%d want 0 after a re-wrap at a new width", got)
		}
	})

	t.Run("activity keeps its lines while the tail rolls", func(t *testing.T) {
		current := feedNumberedActivity(t, populatedViewModel(t), 0, 10)
		current.focus = boxActivity
		for index := 0; index < 2; index++ {
			updated, _ := current.Update(printableKey('k'))
			current = updated.(model)
		}
		window := func() string {
			return ansi.Strip(visibleLines(
				mainBoxBody(current, boxActivity, 100, 10),
				windowHeight,
				current.boxScroll[boxActivity],
			))
		}
		before := window()
		if !strings.Contains(before, "line-05") {
			t.Fatalf("paused activity window is not in history:\n%s", before)
		}

		current = feedNumberedActivity(t, current, 10, 1)
		if after := window(); after != before {
			t.Fatalf(
				"paused activity drifted when the tail rolled:\nbefore:\n%s\nafter:\n%s",
				before,
				after,
			)
		}
	})

	// Scrolling back must not resize the box: the paused body is longer than the
	// tail, and letting it set the height would steal FEED's rows mid-read.
	t.Run("a paused activity does not grow its box", func(t *testing.T) {
		current := feedNumberedActivity(t, populatedViewModel(t), 0, 12)
		current.status.PendingAction = nil
		order := mainBoxOrder(current)
		rest := distributeMainBoxHeights(order, wantRowsFor(current, order, 100), 30, 2)

		current.boxScroll[boxActivity] = 4
		paused := distributeMainBoxHeights(order, wantRowsFor(current, order, 100), 30, 2)
		for index := range order {
			if rest[index] != paused[index] {
				t.Fatalf("heights changed on scroll: rest=%v paused=%v", rest, paused)
			}
		}
	})

	// At the ring buffer's cap every arrival also evicts the oldest line, so the
	// offset has to survive the shift, not just the append.
	t.Run("offsets survive buffer eviction", func(t *testing.T) {
		current := populatedViewModel(t)
		current.logs = make([]logEntry, 0, maxLogLines)
		for index := 0; index < maxLogLines; index++ {
			current.appendLog(logEntry{Text: fmt.Sprintf("evict-%04d", index)})
		}
		current.focus = boxLiveOutput
		current.boxScroll[boxLiveOutput] = 7
		window := func() string {
			return ansi.Strip(visibleLines(
				lifecycleBody(current, 100),
				windowHeight,
				current.boxScroll[boxLiveOutput],
			))
		}
		before := window()

		current.appendLog(logEntry{Text: "evict-1000"})
		if len(current.logs) != maxLogLines {
			t.Fatalf("buffer len=%d want the cap %d", len(current.logs), maxLogLines)
		}
		if after := window(); after != before {
			t.Fatalf(
				"paused view drifted when the buffer evicted:\nbefore:\n%s\nafter:\n%s",
				before,
				after,
			)
		}
	})
}

// The tab order is the contract's order, not the render order: PENDING renders
// first but comes last in the cycle (review T14a-r2 f2).
func TestFocusCycleOrderIsIndependentOfRenderOrder(t *testing.T) {
	current := feedNumberedActivity(t, populatedViewModel(t), 0, 4)
	current.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingTaskCap,
		Prompt: "raise the cap or abort",
	}
	if first := mainBoxOrder(current)[0]; first != boxPending {
		t.Fatalf("render order no longer leads with PENDING (got %d)", first)
	}
	current.focus = boxFeed

	forward := []mainBox{boxLiveOutput, boxActivity, boxPending, boxSidebar, boxFeed}
	for step, want := range forward {
		updated, _ := current.Update(specialKey(tea.KeyTab))
		current = updated.(model)
		if current.focus != want {
			t.Fatalf("tab step %d: focus=%d want=%d", step+1, current.focus, want)
		}
	}

	backward := []mainBox{boxSidebar, boxPending, boxActivity, boxLiveOutput, boxFeed}
	for step, want := range backward {
		updated, _ := current.Update(tea.KeyPressMsg(tea.Key{
			Code: tea.KeyTab,
			Mod:  tea.ModShift,
		}))
		current = updated.(model)
		if current.focus != want {
			t.Fatalf("shift+tab step %d: focus=%d want=%d", step+1, current.focus, want)
		}
	}
}

// Compact has no focus, so one scroll gesture has to reach every section. Driving
// FEED alone left the rows a squeezed ACTIVITY or LIVE OUTPUT hid unreachable —
// a regression from the T13 single feed (review T14a-r2 f2).
func TestCompactSingleScrollReachesEverySection(t *testing.T) {
	current := feedNumberedActivity(t, populatedViewModel(t), 0, 10)
	current.width = 80
	current.height = 24
	current.status.PendingAction = &state.PendingAction{
		Kind:   state.PendingTaskCap,
		Prompt: strings.Repeat("the cap was reached and the run needs a decision ", 3),
	}
	frame := func() string { return ansi.Strip(renderMainCompact(current, 80, 20)) }
	if rest := frame(); strings.Contains(rest, "line-00") {
		t.Fatalf("the oldest activity line was already visible at rest:\n%s", rest)
	}

	updated, _ := current.Update(printableKey('k'))
	scrolled := updated.(model)
	for _, box := range mainBoxOrder(scrolled) {
		if scrolled.boxScroll[box] != 1 {
			t.Fatalf(
				"box %d offset=%d — one compact gesture must move every section",
				box,
				scrolled.boxScroll[box],
			)
		}
	}

	updated, _ = current.Update(specialKey(tea.KeyHome))
	current = updated.(model)
	if parked := frame(); !strings.Contains(parked, "line-00") {
		t.Fatalf("compact scrolling cannot reach the activity history:\n%s", parked)
	}

	updated, _ = current.Update(specialKey(tea.KeyEnd))
	current = updated.(model)
	for _, box := range mainBoxOrder(current) {
		if current.boxScroll[box] != 0 {
			t.Fatalf("end left box %d at offset %d", box, current.boxScroll[box])
		}
	}
}

// feedNumberedActivity streams `count` distinctly numbered stdout lines starting
// at `from`, so a test can tell an appended line from a re-sent one.
func feedNumberedActivity(t *testing.T, current model, from, count int) model {
	t.Helper()
	for index := from; index < from+count; index++ {
		line := runner.Line{
			Attempt: 1,
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

func wantRowsFor(current model, order []mainBox, innerWidth int) []int {
	wants := make([]int, len(order))
	for index, box := range order {
		wants[index] = mainBoxWantRows(
			current,
			box,
			mainBoxBody(current, box, innerWidth, 10),
			30,
			2,
		)
	}
	return wants
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
		for _, target := range resumed.scrollTargets() {
			if target == boxPending {
				t.Fatal("j/k would drive an off-screen offset")
			}
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
				renderBoxCard(focused.theme, "T", "", "body", 20, true),
				"╭",
			).Style.Fg,
			lipgloss.Color(focused.theme.tokens.Component.BorderFocused),
		)
		assertSameColor(
			t,
			findStyledCell(
				t,
				renderBoxCard(focused.theme, "T", "", "body", 20, false),
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

		// `a` now only arms the confirmation; `enter` commits it (T14 W5).
		updated, command := current.Update(printableKey('a'))
		current = updated.(model)
		if command != nil || current.prompt != promptApproveConfirm {
			t.Fatal("approve key did not arm the confirmation step")
		}
		updated, command = current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		if command == nil || current.operation != operationApprove {
			t.Fatal("approve confirmation did not start a control operation")
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

// The lifecycle cursor: `j/k` move it while LIVE OUTPUT is focused (selection *is*
// the scroll), `enter` expands and opens that step's artifact, `esc` lets go, and
// the other boxes keep the raw offset (T14 W4).
func TestLifecycleCursorMovesSelectsAndExpands(t *testing.T) {
	current := populatedViewModel(t)
	for index := 0; index < 12; index++ {
		current.appendLog(logEntry{
			Step: pipeline.StepPlan,
			Role: fmt.Sprintf("role-%02d", index),
			CLI:  "claude",
			Text: strings.Repeat("a long diagnostic sentence ", 6),
		})
	}
	current.focus = boxLiveOutput

	// The first press only reveals the cursor on the newest entry.
	updated, _ := current.Update(printableKey('k'))
	current = updated.(model)
	if !current.hasSelection || current.selectedEntry != len(current.logs)-1 {
		t.Fatalf("first k did not reveal the cursor on the newest entry: %d/%v",
			current.selectedEntry, current.hasSelection)
	}
	if current.boxScroll[boxLiveOutput] != 0 {
		t.Fatalf("the newest entry is not at the live edge: %d",
			current.boxScroll[boxLiveOutput])
	}

	// Then it walks, dragging the viewport: the cursor sits on the window's last row.
	for step := 1; step <= 3; step++ {
		updated, _ = current.Update(printableKey('k'))
		current = updated.(model)
		wantIndex := len(current.logs) - 1 - step
		if current.selectedEntry != wantIndex {
			t.Fatalf("k step %d: cursor=%d want=%d", step, current.selectedEntry, wantIndex)
		}
		if got := current.boxScroll[boxLiveOutput]; got != step {
			t.Fatalf("k step %d: offset=%d want=%d — selection must drive the scroll",
				step, got, step)
		}
	}
	updated, _ = current.Update(printableKey('j'))
	current = updated.(model)
	if current.selectedEntry != len(current.logs)-3 {
		t.Fatalf("j did not walk back down: cursor=%d", current.selectedEntry)
	}

	// The cursor is marked without a background, and without colour alone.
	body := ansi.Strip(lifecycleBody(current, 100))
	rows := strings.Split(body, "\n")
	marked := 0
	for _, row := range rows {
		if strings.HasPrefix(row, "▌▸") {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("expected exactly one marked row, got %d:\n%s", marked, body)
	}
	// Every other row keeps the gutter, so the columns stay aligned.
	for _, row := range rows {
		if !strings.HasPrefix(row, "▌▸") && !strings.HasPrefix(row, "  ") {
			t.Fatalf("row lost the gutter column, columns will not line up: %q", row)
		}
	}

	// enter expands the entry to its full text and opens the step's artifact.
	current.artifactTab = tabVerdict
	updated, _ = current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if !current.entryExpanded {
		t.Fatal("enter did not expand the selected entry")
	}
	if current.artifactTab != tabPlan {
		t.Fatalf("expanding a plan step opened tab %d, want Plan", current.artifactTab)
	}
	expanded := ansi.Strip(lifecycleBody(current, 60))
	if countRows(expanded) <= len(current.logs) {
		t.Fatalf("the expanded entry did not wrap onto extra rows:\n%s", expanded)
	}
	// Only the cursor's rows are expanded — the rest stay truncated to one row — so
	// assert against the block that starts at the marker.
	block := make([]string, 0, 8)
	for _, row := range strings.Split(expanded, "\n") {
		if strings.HasPrefix(row, "▌▸") {
			block = append(block, row)
			continue
		}
		if len(block) > 0 {
			if !strings.HasPrefix(row, "    ") {
				break
			}
			block = append(block, row)
		}
	}
	if len(block) < 3 {
		t.Fatalf("the cursor's block is %d rows, expected the text to wrap:\n%s",
			len(block), expanded)
	}
	joined := strings.Join(block, "\n")
	if strings.Contains(joined, "…") {
		t.Fatalf("the expanded entry is still truncated:\n%s", joined)
	}
	// A hard wrap can break mid-word, so compare with the spacing removed: what
	// matters is that no characters were dropped.
	squeeze := func(text string) string {
		return strings.ReplaceAll(strings.Join(strings.Fields(text), ""), "…", "")
	}
	if want := squeeze(current.logs[current.selectedEntry].Text); !strings.Contains(
		squeeze(joined),
		want,
	) {
		t.Fatalf("the expanded entry does not show its full text:\n%s", joined)
	}

	// esc lets go and follows again.
	updated, _ = current.Update(specialKey(tea.KeyEscape))
	current = updated.(model)
	if current.hasSelection || current.entryExpanded ||
		current.boxScroll[boxLiveOutput] != 0 {
		t.Fatalf("esc did not return to following: %#v",
			[]any{current.hasSelection, current.entryExpanded,
				current.boxScroll[boxLiveOutput]})
	}
}

// j/k keep their raw-offset meaning everywhere the cursor does not apply: the other
// boxes, and compact (which has no focus at all) — T14 W2/W4 reconciliation.
func TestCursorDoesNotHijackTheOtherBoxes(t *testing.T) {
	base := populatedViewModel(t)
	for index := 0; index < 6; index++ {
		base.appendLog(logEntry{Role: fmt.Sprintf("role-%02d", index), Text: "x"})
	}

	for _, box := range []mainBox{boxFeed, boxActivity, boxSidebar} {
		current := base
		current.focus = box
		if current.selectionDrivesKeys() {
			t.Fatalf("box %d claimed the lifecycle cursor", box)
		}
		updated, _ := current.Update(printableKey('k'))
		moved := updated.(model)
		if moved.hasSelection {
			t.Fatalf("k selected an entry while box %d was focused", box)
		}
		if moved.boxScroll[moved.normalizedFocus()] != 1 {
			t.Fatalf("k did not scroll the focused box %d", box)
		}
	}

	compact := base
	compact.width = 80
	compact.height = 24
	compact.focus = boxLiveOutput
	if compact.selectionDrivesKeys() {
		t.Fatal("compact has no focus concept, so it must not grow a cursor")
	}
	updated, _ := compact.Update(printableKey('k'))
	scrolled := updated.(model)
	for _, box := range mainBoxOrder(scrolled) {
		if scrolled.boxScroll[box] != 1 {
			t.Fatalf("compact k left box %d at %d", box, scrolled.boxScroll[box])
		}
	}
}

// The cursor names an entry, not a row number: eviction at the buffer cap has to
// renumber it, and drop it when its own row is the one evicted (T14 W4).
func TestCursorSurvivesEvictionAndLetsGoWhenItsRowIsDropped(t *testing.T) {
	current := populatedViewModel(t)
	current.logs = nil
	for index := 0; index < maxLogLines; index++ {
		current.appendLog(logEntry{Text: fmt.Sprintf("entry-%04d", index)})
	}
	current.focus = boxLiveOutput
	current.selectedEntry = 500
	current.hasSelection = true
	target := current.logs[500].Text

	current.appendLog(logEntry{Text: "fresh"})
	if !current.hasSelection {
		t.Fatal("the cursor let go of a row that is still in the buffer")
	}
	if got := current.logs[current.selectedEntry].Text; got != target {
		t.Fatalf("the cursor drifted to %q, want %q", got, target)
	}

	current.selectedEntry = 0
	current.appendLog(logEntry{Text: "evicts the cursor's row"})
	if current.hasSelection {
		t.Fatal("the cursor kept pointing at an evicted row")
	}
}

// task_cap becomes a pick instead of something to type, but the value handed to the
// validator is unchanged (T14 W5).
func TestTaskCapPromptIsAPickNotTyping(t *testing.T) {
	fake := &fakeUIControl{}
	current := testModel(t, fake)
	current.hasStatus = true
	current.status = pipeline.RunStatus{
		RunID: "run-cap",
		Phase: state.PhasePausedForInput,
		PendingAction: &state.PendingAction{
			Kind:        state.PendingTaskCap,
			ResumePhase: state.PhaseImplementing,
			Prompt:      "attempt cap reached",
		},
	}

	updated, _ := current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if current.promptValue != "retry" {
		t.Fatalf("the pick did not default to retry, got %q", current.promptValue)
	}
	if current.usesTextarea() {
		t.Fatal("task_cap opened a free-text editor")
	}

	// Typing must not reach the value — that is the whole point of the pick.
	updated, _ = current.Update(printableKey('z'))
	current = updated.(model)
	updated, _ = current.Update(tea.PasteMsg{Content: "garbage"})
	current = updated.(model)
	if current.promptValue != "retry" {
		t.Fatalf("typing leaked into the pick: %q", current.promptValue)
	}

	updated, _ = current.Update(specialKey(tea.KeyRight))
	current = updated.(model)
	if current.promptValue != "abort" {
		t.Fatalf("→ did not move the pick, got %q", current.promptValue)
	}
	plain := ansi.Strip(renderStatusBar(current, 100, promptRowsSingle))
	if !strings.Contains(plain, "▸ abort") {
		t.Fatalf("the chosen option carries no non-color marker:\n%s", plain)
	}
	if strings.Contains(plain, "▸ retry") {
		t.Fatalf("two options are marked as chosen:\n%s", plain)
	}

	updated, command := current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if command == nil {
		t.Fatal("enter did not submit the pick")
	}
	_ = operationDoneFromCommand(t, command)
	response := "abort"
	assertUICall(t, fake, operationResume, "run-cap", &response)
}

// Measured: the textarea binds InsertNewline to `enter` by default, which is the
// submit key here. So `enter` submits and `ctrl+j` inserts the newline (T14 W5).
func TestEditorSubmitsOnEnterAndBreaksLinesOnCtrlJ(t *testing.T) {
	fake := &fakeUIControl{}
	current := testModel(t, fake)
	current.hasStatus = true
	current.status = pipeline.RunStatus{
		RunID: "run-r",
		Phase: state.PhaseAwaitingApproval,
	}

	updated, _ := current.Update(printableKey('r'))
	current = updated.(model)
	if !current.usesTextarea() {
		t.Fatal("reject did not open the editor")
	}

	updated, _ = current.Update(tea.PasteMsg{Content: "first"})
	current = updated.(model)
	// ctrl+j carries no text — a terminal sends it as a control code, and passing
	// Text would make the editor insert a literal "j".
	updated, _ = current.Update(tea.KeyPressMsg(tea.Key{
		Code: 'j',
		Mod:  tea.ModCtrl,
	}))
	current = updated.(model)
	updated, _ = current.Update(tea.PasteMsg{Content: "second"})
	current = updated.(model)

	if got := current.promptResponse(); got != "first\nsecond" {
		t.Fatalf("ctrl+j did not insert a newline: %q", got)
	}
	if current.prompt == promptNone {
		t.Fatal("ctrl+j submitted instead of inserting a newline")
	}

	// tab must not move box focus while the editor has it.
	before := current.focus
	updated, _ = current.Update(specialKey(tea.KeyTab))
	current = updated.(model)
	if current.focus != before {
		t.Fatalf("tab moved focus out from under the editor: %d -> %d",
			before, current.focus)
	}

	updated, command := current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if command == nil || current.prompt != promptNone {
		t.Fatal("enter did not submit the editor")
	}
	_ = operationDoneFromCommand(t, command)
	response := "first\nsecond"
	assertUICall(t, fake, operationReject, "run-r", &response)
}

// Approval takes two keys now, and `esc` on the confirmation must not approve
// (T14 W5). The T15 CLI entry stays exempt by seeding the operation directly.
func TestApproveNeedsConfirmationAndEscapeCancels(t *testing.T) {
	fake := &fakeUIControl{}
	current := testModel(t, fake)
	current.hasStatus = true
	current.status = pipeline.RunStatus{
		RunID: "run-a",
		Phase: state.PhaseAwaitingApproval,
	}

	updated, command := current.Update(printableKey('a'))
	current = updated.(model)
	if command != nil || current.prompt != promptApproveConfirm {
		t.Fatal("`a` approved without confirmation")
	}
	updated, command = current.Update(specialKey(tea.KeyEscape))
	current = updated.(model)
	if command != nil || current.prompt != promptNone || len(fake.calls) != 0 {
		t.Fatalf("esc did not cancel the confirmation: calls=%d", len(fake.calls))
	}

	updated, _ = current.Update(printableKey('a'))
	current = updated.(model)
	updated, command = current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if command == nil || current.operation != operationApprove {
		t.Fatal("the confirmation did not approve")
	}
	_ = operationDoneFromCommand(t, command)
	assertUICall(t, fake, operationApprove, "run-a", nil)
}

// f1: the artifact link has to work on the entries the *pipeline* produces, not only
// on hand-built ones. appendSystemLog used to stamp every entry's Step as "coterix",
// which disabled the link while the test suite still passed (review T14c f1).
func TestExpandingARealPipelineEntryOpensItsArtifact(t *testing.T) {
	for _, test := range []struct {
		step string
		want artifactTab
		same bool
	}{
		{step: pipeline.StepPlan, want: tabPlan},
		{step: pipeline.StepImplementation, want: tabDiff},
		{step: pipeline.StepPlanReview, want: tabVerdict},
		{step: pipeline.StepImplementationReview, want: tabVerdict},
		// Steps with no artifact of their own must leave the tab alone.
		{step: pipeline.StepGate, want: tabDiff, same: true},
		{step: pipeline.StepFix, want: tabDiff, same: true},
	} {
		t.Run(test.step, func(t *testing.T) {
			current := populatedViewModel(t)
			current.focus = boxLiveOutput
			current.artifactTab = tabDiff
			// The real path: a pipeline event, not a hand-built logEntry.
			updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
				Kind: pipeline.EventStepStarted,
				Step: test.step,
				Role: "some_role",
				CLI:  "claude",
			}})
			current = updated.(model)
			if got := current.logs[len(current.logs)-1].Step; got != test.step {
				t.Fatalf("the lifecycle entry lost its pipeline step: %q", got)
			}

			updated, _ = current.Update(printableKey('k'))
			current = updated.(model)
			updated, _ = current.Update(specialKey(tea.KeyEnter))
			current = updated.(model)
			if !current.entryExpanded {
				t.Fatal("enter did not expand the entry")
			}
			if current.artifactTab != test.want {
				t.Fatalf("tab=%d want=%d", current.artifactTab, test.want)
			}
			if test.same && current.artifactTab != tabDiff {
				t.Fatalf("a step with no artifact moved the tab to %d", current.artifactTab)
			}
		})
	}
}

// f2: a cursor on the newest entry is still a cursor. Its offset is 0, which the
// drift correction reads as "following", so the viewport used to run ahead and leave
// the marker behind (review T14c f2).
func TestCursorOnNewestEntryStaysAnchoredAsLogsArrive(t *testing.T) {
	current := populatedViewModel(t)
	for index := 0; index < 6; index++ {
		current.appendLog(logEntry{Role: fmt.Sprintf("role-%02d", index), Text: "x"})
	}
	current.focus = boxLiveOutput

	updated, _ := current.Update(printableKey('k'))
	current = updated.(model)
	anchored := current.logs[current.selectedEntry].Role

	for index := 6; index < 12; index++ {
		current.appendLog(logEntry{Role: fmt.Sprintf("role-%02d", index), Text: "x"})
		if got := current.logs[current.selectedEntry].Role; got != anchored {
			t.Fatalf("the cursor drifted to %q after %d new rows, want %q",
				got, index-5, anchored)
		}
		if want := len(current.logs) - 1 - current.selectedEntry; current.boxScroll[boxLiveOutput] != want {
			t.Fatalf("offset=%d want=%d — the cursor must drag the viewport",
				current.boxScroll[boxLiveOutput], want)
		}
	}

	// The marker is still the last row of the window the box actually renders.
	rows := strings.Split(ansi.Strip(visibleLines(
		lifecycleBody(current, 100),
		4,
		current.boxScroll[boxLiveOutput],
	)), "\n")
	if !strings.HasPrefix(rows[len(rows)-1], "▌▸") {
		t.Fatalf("the cursor left the window:\n%s", strings.Join(rows, "\n"))
	}
}

// f3: an entry longer than the box must still be readable — head, marker and all —
// inside the height LIVE OUTPUT is actually given (review T14c f3).
func TestExpandedEntryFitsTheHeightItIsGiven(t *testing.T) {
	current := populatedViewModel(t)
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight
	for index := 0; index < 8; index++ {
		current.appendLog(logEntry{Role: fmt.Sprintf("role-%02d", index), Text: "short"})
	}
	current.appendLog(logEntry{
		Step: pipeline.StepGate,
		Role: "gate",
		Text: strings.Repeat("a very long gate failure explanation ", 30),
	})
	current.focus = boxLiveOutput

	updated, _ := current.Update(printableKey('k'))
	current = updated.(model)
	updated, _ = current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)

	frame := ansi.Strip(renderMain(current, wideBreakpointWidth-sidebarWidth, 26))
	if !strings.Contains(frame, "▌▸") {
		t.Fatalf("the expanded entry's marker was clipped out of the box:\n%s", frame)
	}
	if !strings.Contains(frame, "gate") {
		t.Fatalf("the expanded entry's columns were clipped:\n%s", frame)
	}
	// Everything withheld is named rather than silently dropped.
	if !strings.Contains(frame, "more rows in logs/") {
		t.Fatalf("the truncated tail is not accounted for:\n%s", frame)
	}
}

// f4: the overlay is modal. Nothing may open behind it (review T14c f4).
func TestHelpOverlaySwallowsActionKeys(t *testing.T) {
	base := populatedViewModel(t)
	base.status.Phase = state.PhaseAwaitingApproval

	for _, key := range []rune{'a', 'r', 'j', 'k', '1', '?'} {
		current := base
		updated, _ := current.Update(printableKey('?'))
		current = updated.(model)
		if !current.helpOpen {
			t.Fatal("`?` did not open the overlay")
		}
		updated, command := current.Update(printableKey(key))
		current = updated.(model)

		if key == '?' {
			if current.helpOpen {
				t.Fatal("`?` did not close the overlay")
			}
			continue
		}
		if command != nil {
			t.Fatalf("key %q started an operation from behind the overlay", key)
		}
		if current.prompt != promptNone {
			t.Fatalf("key %q opened a prompt behind the overlay", key)
		}
		if !current.helpOpen {
			t.Fatalf("key %q closed the overlay", key)
		}
		if current.hasSelection || current.artifactTab != base.artifactTab {
			t.Fatalf("key %q changed state behind the overlay", key)
		}
		// esc still closes it, on the first press.
		updated, _ = current.Update(specialKey(tea.KeyEscape))
		if updated.(model).helpOpen {
			t.Fatalf("esc did not close the overlay after %q", key)
		}
	}
}

// r2-f1: home/end and the cursor cannot both own the viewport. With a cursor up,
// home parks it on the oldest entry and end lets it go — offsets alone left the
// marker off screen and the next log jumped the view back (review T14c-r2 f1).
func TestHomeAndEndAgreeWithTheCursor(t *testing.T) {
	base := populatedViewModel(t)
	for index := 0; index < 12; index++ {
		base.appendLog(logEntry{Role: fmt.Sprintf("role-%02d", index), Text: "x"})
	}
	base.focus = boxLiveOutput
	withCursor := func() model {
		current := base
		for press := 0; press < 3; press++ {
			updated, _ := current.Update(printableKey('k'))
			current = updated.(model)
		}
		return current
	}
	// The cursor normally parks on the window's last row, but at the top of the
	// buffer the window cannot scroll further up — there the rule is simply that the
	// cursor is on screen.
	markerVisible := func(t *testing.T, current model) {
		t.Helper()
		rows := strings.Split(ansi.Strip(visibleLines(
			lifecycleBody(current, 100),
			4,
			current.boxScroll[boxLiveOutput],
		)), "\n")
		for _, row := range rows {
			if strings.HasPrefix(row, "▌▸") {
				return
			}
		}
		t.Fatalf("the cursor is not in the window:\n%s", strings.Join(rows, "\n"))
	}

	t.Run("home parks the cursor on the oldest entry", func(t *testing.T) {
		current := withCursor()
		updated, _ := current.Update(specialKey(tea.KeyHome))
		current = updated.(model)
		if !current.hasSelection || current.selectedEntry != 0 {
			t.Fatalf("home left the cursor at %d (selection=%v)",
				current.selectedEntry, current.hasSelection)
		}
		markerVisible(t, current)

		// A new log must not jump the view: the cursor still owns it.
		oldest := current.logs[0].Role
		current.appendLog(logEntry{Role: "fresh", Text: "x"})
		if current.logs[current.selectedEntry].Role != oldest {
			t.Fatal("a new log moved the cursor off the oldest entry")
		}
		markerVisible(t, current)
	})

	t.Run("end lets the cursor go and really follows", func(t *testing.T) {
		current := withCursor()
		updated, _ := current.Update(specialKey(tea.KeyEnd))
		current = updated.(model)
		if current.hasSelection {
			t.Fatal("end kept a cursor while claiming to follow")
		}
		if current.boxScroll[boxLiveOutput] != 0 {
			t.Fatalf("end left offset=%d", current.boxScroll[boxLiveOutput])
		}
		// And it stays at the live edge as logs arrive.
		current.appendLog(logEntry{Role: "fresh", Text: "x"})
		if current.boxScroll[boxLiveOutput] != 0 {
			t.Fatalf("the view jumped back to a stale cursor: offset=%d",
				current.boxScroll[boxLiveOutput])
		}
	})
}

// r2-f2: PENDING outranks LIVE OUTPUT for rows, so a bounded block is still taller
// than its box in the normal blocked state. `j/k` walk the block so its head and
// marker are always reachable (review T14c-r2 f2).
func TestExpandedBlockIsWalkableUnderPendingPressure(t *testing.T) {
	current := populatedViewModel(t)
	current.width = wideBreakpointWidth
	current.height = wideBreakpointHeight
	current.status.Phase = state.PhasePausedForInput
	current.status.PendingAction = &state.PendingAction{
		Kind: state.PendingPlanQuestion,
		Prompt: strings.Repeat(
			"which package should own the retry feedback loop and why ", 6),
	}
	for index := 0; index < 6; index++ {
		current.appendLog(logEntry{Role: fmt.Sprintf("role-%02d", index), Text: "short"})
	}
	current.appendLog(logEntry{
		Step: pipeline.StepGate,
		Role: "gate",
		Text: strings.Repeat("a long gate failure explanation ", 40),
	})
	current.focus = boxLiveOutput

	updated, _ := current.Update(printableKey('k'))
	current = updated.(model)
	updated, _ = current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)

	frame := func(model model) string {
		return ansi.Strip(renderDashboard(model))
	}
	// The block starts out bottom-anchored, so its tail shows first.
	if !strings.Contains(frame(current), "more rows in logs/") {
		t.Fatalf("the withheld tail is not accounted for:\n%s", frame(current))
	}

	// Walking up must bring the head — marker and columns — into view.
	found := false
	for press := 0; press < maxExpandedBlockRows && !found; press++ {
		updated, _ = current.Update(printableKey('k'))
		current = updated.(model)
		if strings.Contains(frame(current), "▌▸") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the expanded block's head is unreachable under PENDING:\n%s",
			frame(current))
	}
	if current.expandScroll == 0 {
		t.Fatal("k did not walk inside the block")
	}
	// Walking back down returns to the block's tail without moving the cursor.
	anchored := current.selectedEntry
	for press := current.expandScroll; press > 0; press-- {
		updated, _ = current.Update(printableKey('j'))
		current = updated.(model)
	}
	if current.expandScroll != 0 || current.selectedEntry != anchored {
		t.Fatalf("j left expandScroll=%d cursor=%d (want 0, %d)",
			current.expandScroll, current.selectedEntry, anchored)
	}
	// Collapsing resets the walk.
	updated, _ = current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if current.entryExpanded || current.expandScroll != 0 {
		t.Fatalf("collapse left expanded=%v scroll=%d",
			current.entryExpanded, current.expandScroll)
	}
}

// r3-f2: a prompt takes the keyboard *and* shrinks LIVE OUTPUT, so an expanded block
// would sit clipped with no key able to reach its head. Opening a prompt therefore
// ends the reading mode but keeps the cursor (review T14c-r3 f2). This goes through
// renderDashboard with the prompt actually open — setting PendingAction alone leaves
// statusHeight at 2 and misses the whole path.
func TestOpeningAPromptCollapsesTheExpandedBlock(t *testing.T) {
	build := func(t *testing.T) model {
		t.Helper()
		current := populatedViewModel(t)
		current.width = wideBreakpointWidth
		current.height = wideBreakpointHeight
		current.status.Phase = state.PhasePausedForInput
		current.status.PendingAction = &state.PendingAction{
			Kind: state.PendingPlanQuestion,
			Prompt: strings.Repeat(
				"which package should own the retry feedback loop ", 6),
		}
		for index := 0; index < 6; index++ {
			current.appendLog(logEntry{
				Role: fmt.Sprintf("role-%02d", index),
				Text: "short",
			})
		}
		current.appendLog(logEntry{
			Step: pipeline.StepGate,
			Role: "gate",
			Text: strings.Repeat("a long gate failure explanation ", 40),
		})
		current.focus = boxLiveOutput

		updated, _ := current.Update(printableKey('k'))
		current = updated.(model)
		updated, _ = current.Update(specialKey(tea.KeyEnter))
		current = updated.(model)
		if !current.entryExpanded {
			t.Fatal("the entry did not expand before the prompt was opened")
		}
		// Walk into the block so the collapse has something to undo.
		updated, _ = current.Update(printableKey('k'))
		current = updated.(model)
		if current.expandScroll == 0 {
			t.Fatal("k did not walk inside the block")
		}
		return current
	}

	for _, promptCase := range []struct {
		name string
		open func(model) model
	}{
		{
			name: "plan_question editor",
			open: func(current model) model {
				// The real path: focus another box, then enter opens the editor.
				current.focus = boxPending
				updated, _ := current.Update(specialKey(tea.KeyEnter))
				return updated.(model)
			},
		},
		{
			name: "approve confirmation",
			open: func(current model) model {
				current.status.Phase = state.PhaseAwaitingApproval
				current.status.PendingAction = nil
				updated, _ := current.Update(printableKey('a'))
				return updated.(model)
			},
		},
	} {
		t.Run(promptCase.name, func(t *testing.T) {
			current := promptCase.open(build(t))
			if current.prompt == promptNone {
				t.Fatal("the prompt did not open")
			}
			if current.entryExpanded || current.expandScroll != 0 {
				t.Fatalf("the block stayed expanded behind the prompt: expanded=%v scroll=%d",
					current.entryExpanded, current.expandScroll)
			}
			if !current.hasSelection {
				t.Fatal("the cursor was dropped — only the reading mode should end")
			}

			// With the prompt open the cursor's row is one row again, so the marker is
			// on screen in the shrunken box and nothing is clipped unreachably.
			frame := ansi.Strip(renderDashboard(current))
			if !strings.Contains(frame, "▌▸") {
				t.Fatalf("the cursor is not visible with the prompt open:\n%s", frame)
			}
			if strings.Contains(frame, "more rows in logs/") {
				t.Fatalf("a truncated block is still drawn behind the prompt:\n%s", frame)
			}
		})
	}
}

// W8: accepting a keypress is otherwise invisible while the operation runs, so the
// status bar acknowledges it for a few seconds. Expiry is decided by the injected
// clock, not by the tick, so the test is deterministic (T14 W8).
func TestOperationAcknowledgementAppearsAndExpires(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	fake := &fakeUIControl{}
	current := testModel(t, fake)
	current.now = func() time.Time { return clock }
	current.hasStatus = true
	current.status = pipeline.RunStatus{
		RunID: "run-t",
		Phase: state.PhaseAwaitingApproval,
	}

	// The real path: `a` arms, `enter` commits.
	updated, _ := current.Update(printableKey('a'))
	current = updated.(model)
	if current.toastLive() {
		t.Fatal("arming the confirmation already acknowledged it")
	}
	updated, command := current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if command == nil {
		t.Fatal("the confirmation did not start the operation")
	}
	if !current.toastLive() {
		t.Fatal("accepting the approval was not acknowledged")
	}
	frame := ansi.Strip(renderStatusBar(current, 120, 2))
	if !strings.Contains(frame, "approve sent") {
		t.Fatalf("the acknowledgement is not in the status bar:\n%s", frame)
	}
	// It *replaces* the hints rather than pushing them anywhere. Row counting alone
	// cannot see this: the status bar is height-clamped, so an extra row silently
	// displaces the hints into the detail slot instead of overflowing.
	if strings.Contains(frame, "j/k scroll") {
		t.Fatalf("the hints survived alongside the acknowledgement:\n%s", frame)
	}
	if rows := strings.Count(frame, "\n"); rows > 1 {
		t.Fatalf("the acknowledgement added rows: %d newlines", rows)
	}

	// Just before the window closes it is still there; after, it is gone.
	clock = clock.Add(toastDuration - time.Millisecond)
	if !current.toastLive() {
		t.Fatal("the acknowledgement expired early")
	}
	clock = clock.Add(2 * time.Millisecond)
	if current.toastLive() {
		t.Fatal("the acknowledgement outlived its window")
	}
	if strings.Contains(ansi.Strip(renderStatusBar(current, 120, 2)), "approve sent") {
		t.Fatal("an expired acknowledgement is still drawn")
	}
	// The scheduled redraw clears the text so it cannot come back.
	updated, _ = current.Update(toastExpiredMsg{})
	if updated.(model).toast != "" {
		t.Fatal("the expiry tick did not clear the acknowledgement")
	}
	// Hints are back.
	if !strings.Contains(ansi.Strip(renderStatusBar(updated.(model), 120, 2)), "quit") {
		t.Fatal("the hints did not return after the acknowledgement")
	}
}

// A prompt owns the whole status region, so an acknowledgement must not compete with
// it — the reject/resume acknowledgement only appears once the prompt has closed
// (T14 W8 item 7).
func TestAcknowledgementNeverSharesTheStatusBarWithAPrompt(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	fake := &fakeUIControl{}
	current := testModel(t, fake)
	current.now = func() time.Time { return clock }
	current.hasStatus = true
	current.status = pipeline.RunStatus{
		RunID: "run-p",
		Phase: state.PhaseAwaitingApproval,
	}

	updated, _ := current.Update(printableKey('r'))
	current = updated.(model)
	if !current.usesTextarea() {
		t.Fatal("reject did not open the editor")
	}
	updated, _ = current.Update(tea.PasteMsg{Content: "needs another gate"})
	current = updated.(model)

	// While the editor is open the region is the editor's.
	editor := ansi.Strip(renderStatusBar(current, 120, promptRowsArea))
	if strings.Contains(editor, "sent") {
		t.Fatalf("an acknowledgement leaked into the prompt region:\n%s", editor)
	}

	updated, command := current.Update(specialKey(tea.KeyEnter))
	current = updated.(model)
	if command == nil || current.prompt != promptNone {
		t.Fatal("enter did not submit and close the editor")
	}
	frame := ansi.Strip(renderStatusBar(current, 120, 2))
	if !strings.Contains(frame, "reject sent") {
		t.Fatalf("submitting was not acknowledged once the prompt closed:\n%s", frame)
	}
}

// T13a-2 wiring: the tail shows what the step is doing, not the CLI's session
// bookkeeping. This goes through the real event path with the real captured stream,
// so a decoder that stops being called is caught here and not only in cli's tests.
func TestActivityTailDecodesStreamJSONFromTheRealCapture(t *testing.T) {
	raw, err := os.ReadFile("../cli/testdata/claude-stream-json.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	current := populatedViewModel(t)
	for _, text := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line := runner.Line{
			Attempt: 1,
			Stream:  runner.StreamStdout,
			Text:    text,
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

	if len(current.activity) != 2 {
		t.Fatalf("tail kept %d of 10 lines, want 2:\n%#v",
			len(current.activity), current.activity)
	}
	rendered := ansi.Strip(renderActivityTail(current, 120, activityTailLimit(current)))
	for _, leaked := range []string{"hook", "rate_limit", "\"type\""} {
		if strings.Contains(strings.ToLower(rendered), leaked) {
			t.Fatalf("bookkeeping reached the tail (%q):\n%s", leaked, rendered)
		}
	}
	if !strings.Contains(rendered, "OK") {
		t.Fatalf("the assistant's answer never reached the tail:\n%s", rendered)
	}
}

// Severity comes from the payload, not the stream: codex writes progress to stderr
// and claude reports failures inside a stdout envelope (T13 R5 · T13a-2).
func TestActivityTailMarksFailureFromThePayloadNotTheStream(t *testing.T) {
	current := populatedViewModel(t)
	feed := func(from model, stream runner.Stream, text string) model {
		line := runner.Line{Attempt: 1, Stream: stream, Text: text}
		updated, _ := from.Update(pipelineEventMsg{Event: pipeline.Event{
			Kind:  pipeline.EventStepLog,
			RunID: "run-1",
			Step:  pipeline.StepPlan,
			Role:  "plan_writer",
			CLI:   "claude",
			Line:  &line,
		}})
		return updated.(model)
	}

	// A failure envelope on *stdout* must be marked.
	current = feed(current, runner.StreamStdout,
		`{"type":"result","subtype":"success","is_error":true,"result":"boom"}`)
	if len(current.activity) != 1 || current.activity[0].Icon != logIconFail {
		t.Fatalf("a stdout failure envelope was not marked: %#v", current.activity)
	}

	// Ordinary progress on *stderr* must not be.
	current = feed(current, runner.StreamStderr, "compiling internal/ui")
	last := current.activity[len(current.activity)-1]
	if last.Icon == logIconFail {
		t.Fatalf("stderr progress was marked as a failure: %#v", last)
	}
	if last.Text != "compiling internal/ui" {
		t.Fatalf("a non-JSON line was not passed through: %q", last.Text)
	}
}

// T15 W1: Open seeds through the *event* path. The status load emits a snapshot, the
// snapshot loads the artifacts, and only then does the operation dispatch — a reject
// prompt with a blank artifact pane would be unanswerable.
func TestOpenSeedsThroughTheSnapshotAndDispatchesOnce(t *testing.T) {
	response := "please revise the gate"
	for _, test := range []struct {
		name      string
		command   string
		response  *string
		pending   *state.PendingAction
		phase     state.Phase
		wantOp    operationKind
		wantPromp promptMode
	}{
		{
			name:    "approve dispatches immediately",
			command: "approve",
			phase:   state.PhaseAwaitingApproval,
			wantOp:  operationApprove,
		},
		{
			name:     "reject with a response dispatches immediately",
			command:  "reject",
			response: &response,
			phase:    state.PhaseAwaitingApproval,
			wantOp:   operationReject,
		},
		{
			name:      "bare reject asks for feedback",
			command:   "reject",
			phase:     state.PhaseAwaitingApproval,
			wantPromp: promptReject,
		},
		{
			name:    "auth resume needs no answer",
			command: "resume",
			phase:   state.PhasePausedForInput,
			pending: &state.PendingAction{Kind: state.PendingAuth},
			wantOp:  operationResume,
		},
		{
			name:      "task_cap resume asks which way",
			command:   "resume",
			phase:     state.PhasePausedForInput,
			pending:   &state.PendingAction{Kind: state.PendingTaskCap, Prompt: "cap"},
			wantPromp: promptResume,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeUIControl{}
			kind, err := operationKindForCommand(test.command)
			if err != nil {
				t.Fatal(err)
			}
			current := testModel(t, fake)
			current.openRunID = "run-open"
			current.operation = ""
			current.pendingOperation = &initialOperation{
				Kind:     kind,
				Response: test.response,
			}

			status := pipeline.RunStatus{
				RunID:         "run-open",
				Phase:         test.phase,
				PendingAction: test.pending,
			}
			updated, command := current.Update(pipelineEventMsg{Event: pipeline.Event{
				Kind:     pipeline.EventStateSnapshot,
				RunID:    "run-open",
				RepoRoot: t.TempDir(),
				Status:   &status,
			}})
			current = updated.(model)

			if !current.hasStatus {
				t.Fatal("the snapshot did not seed the model")
			}
			if current.artifactLoadingKey == "" {
				t.Fatal("the artifacts were not requested — a prompt would be blank")
			}
			if command == nil {
				t.Fatal("the snapshot returned no command")
			}
			if current.pendingOperation != nil {
				t.Fatal("the pending operation was not consumed")
			}
			if test.wantOp != "" && current.operation != test.wantOp {
				t.Fatalf("operation=%q want %q", current.operation, test.wantOp)
			}
			if test.wantPromp != promptNone {
				if current.prompt != test.wantPromp {
					t.Fatalf("prompt=%d want %d", current.prompt, test.wantPromp)
				}
				if current.operation != "" {
					t.Fatalf("an operation started before the answer: %q",
						current.operation)
				}
			}

			// A re-emitted snapshot must not dispatch a second time.
			before := current.operation
			updated, _ = current.Update(pipelineEventMsg{Event: pipeline.Event{
				Kind:     pipeline.EventStateSnapshot,
				RunID:    "run-open",
				RepoRoot: current.repoRoot,
				Status:   &status,
			}})
			again := updated.(model)
			if again.pendingOperation != nil {
				t.Fatal("the guard rearmed itself")
			}
			if again.operation != before {
				t.Fatalf("a second snapshot redispatched: %q -> %q",
					before, again.operation)
			}
			// `beginOperation` starts the core call immediately — operationTracker.start
			// launches its goroutine at construction, not when the Cmd is executed — so
			// the call log has to be read after that goroutine is done, not racily
			// alongside it (review T15 f2).
			if current.operation != "" {
				current.tracker.waitLatest()
			}
		})
	}
}

// T15-3R-1: a run that cannot be loaded must not leave a blank dashboard hanging on
// exit 0 — that breaks parity with the non-TTY exit 1.
func TestOpenQuitsAndReportsWhenTheRunCannotBeLoaded(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	current.openRunID = "missing"
	current.operation = ""

	failure := errors.New("pipeline: open run: no such run")
	updated, command := current.Update(runLoadedMsg{err: failure})
	current = updated.(model)

	if command == nil {
		t.Fatal("a failed load did not quit")
	}
	if !errors.Is(current.operationErr, failure) {
		t.Fatalf("operationErr=%v — the error must reach ui.Open's return", current.operationErr)
	}
	if len(current.logs) == 0 ||
		!strings.Contains(current.logs[len(current.logs)-1].Text, "no such run") {
		t.Fatalf("the failure is not in the feed: %#v", current.logs)
	}
	// A successful load says nothing and waits for the snapshot.
	clean := testModel(t, &fakeUIControl{})
	clean.openRunID = "run-1"
	updated, command = clean.Update(runLoadedMsg{})
	if command != nil {
		t.Fatal("a successful load should not quit")
	}
	if updated.(model).operationErr != nil {
		t.Fatal("a successful load recorded an error")
	}
}

// Only the three control commands can open an existing run.
func TestOperationKindForCommandRejectsAnythingElse(t *testing.T) {
	for command, want := range map[string]operationKind{
		"approve": operationApprove,
		"reject":  operationReject,
		"resume":  operationResume,
	} {
		got, err := operationKindForCommand(command)
		if err != nil || got != want {
			t.Fatalf("%q -> %q, %v", command, got, err)
		}
	}
	for _, command := range []string{"run", "status", "", "APPROVE"} {
		if _, err := operationKindForCommand(command); err == nil {
			t.Fatalf("%q was accepted as an interactive entry", command)
		}
	}
}

// T13 W4: `y` copies the run id. Measured — bubbletea v2 ships SetClipboard (OSC52),
// so there is no hand-rolled sequence and no new dependency. OSC52 is fire-and-forget:
// the acknowledgement says the copy was *sent*, not that it landed (R10).
func TestCopyRunIDSendsTheClipboardSequenceAndSaysSo(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	current := testModel(t, &fakeUIControl{})
	current.now = func() time.Time { return clock }

	// With no run there is nothing to copy and nothing to claim.
	updated, command := current.Update(printableKey('y'))
	if command != nil || updated.(model).toastLive() {
		t.Fatal("`y` acted with no run loaded")
	}

	current.hasStatus = true
	current.status = pipeline.RunStatus{RunID: "run-copy"}
	updated, command = current.Update(printableKey('y'))
	current = updated.(model)
	if command == nil {
		t.Fatal("`y` produced no clipboard command")
	}
	if !current.toastLive() {
		t.Fatal("`y` did not acknowledge the attempt")
	}
	frame := ansi.Strip(renderStatusBar(current, 120, 2))
	if !strings.Contains(frame, "copy sent") {
		t.Fatalf("the acknowledgement does not say it was sent:\n%s", frame)
	}
	// The wording must not promise the clipboard was actually updated.
	for _, overclaim := range []string{"copied to clipboard", "copied!"} {
		if strings.Contains(frame, overclaim) {
			t.Fatalf("the acknowledgement overclaims (%q):\n%s", overclaim, frame)
		}
	}
}

// T13 W9: `w` trades rows for the rest of a long line, capped so one line cannot take
// the whole tail.
func TestWrapToggleShowsMoreOfALongTailLineWithinACap(t *testing.T) {
	current := populatedViewModel(t)
	long := strings.Repeat("a long streamed progress sentence ", 8)
	line := runner.Line{Attempt: 1, Stream: runner.StreamStdout, Text: long}
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:  pipeline.EventStepLog,
		RunID: "run-1",
		Step:  pipeline.StepPlan,
		Role:  "plan_writer",
		CLI:   "claude",
		Line:  &line,
	}})
	current = updated.(model)

	truncated := ansi.Strip(activityBody(current, 60, 5))
	if countRows(truncated) != 1 {
		t.Fatalf("the default is one row per line, got %d:\n%s",
			countRows(truncated), truncated)
	}
	if !strings.Contains(truncated, "…") {
		t.Fatalf("the default did not truncate:\n%s", truncated)
	}

	updated, _ = current.Update(printableKey('w'))
	wrapped := updated.(model)
	if !wrapped.wrapTail {
		t.Fatal("`w` did not toggle wrapping")
	}
	body := ansi.Strip(activityBody(wrapped, 60, 5))
	rows := countRows(body)
	if rows < 2 {
		t.Fatalf("wrapping showed no extra rows:\n%s", body)
	}
	if rows > maxWrappedTailRows {
		t.Fatalf("one line took %d rows, cap is %d:\n%s",
			rows, maxWrappedTailRows, body)
	}
	// More of the line is visible than before.
	if len(strings.Join(strings.Fields(body), "")) <=
		len(strings.Join(strings.Fields(truncated), "")) {
		t.Fatalf("wrapping did not reveal more text:\n%s", body)
	}

	// And it toggles back.
	updated, _ = wrapped.Update(printableKey('w'))
	if updated.(model).wrapTail {
		t.Fatal("`w` did not toggle back")
	}
}

// review T13a-2 f2: only claude's stdout is a JSON stream. Decoding everything meant a
// codex progress line that happens to be a JSON object was silently dropped, which
// breaks the "codex writes raw progress lines" contract. This goes through the real
// event path for all four CLI/stream combinations.
func TestOnlyClaudeStdoutIsDecoded(t *testing.T) {
	const jsonLine = `{"status":"working","detail":"applying patch"}`

	for _, test := range []struct {
		name    string
		cli     string
		stream  runner.Stream
		dropped bool
	}{
		{name: "claude stdout is decoded", cli: "claude", stream: runner.StreamStdout, dropped: true},
		{name: "claude stderr is raw", cli: "claude", stream: runner.StreamStderr},
		{name: "codex stdout is raw", cli: "codex", stream: runner.StreamStdout},
		{name: "codex stderr is raw", cli: "codex", stream: runner.StreamStderr},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := populatedViewModel(t)
			line := runner.Line{Attempt: 1, Stream: test.stream, Text: jsonLine}
			updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
				Kind:  pipeline.EventStepLog,
				RunID: "run-1",
				Step:  pipeline.StepImplementation,
				Role:  "impl_writer",
				CLI:   test.cli,
				Line:  &line,
			}})
			current = updated.(model)

			if test.dropped {
				// An unknown envelope on claude stdout is bookkeeping: dropped.
				if len(current.activity) != 0 {
					t.Fatalf("an unknown claude envelope reached the tail: %#v",
						current.activity)
				}
				return
			}
			if len(current.activity) != 1 {
				t.Fatalf("a raw progress line was dropped: %#v", current.activity)
			}
			if got := current.activity[0].Text; got != jsonLine {
				t.Fatalf("the raw line was rewritten to %q", got)
			}
		})
	}
}

// review T13a-2 f3/f4: `w` has to survive the height allocation, and its rows have to
// line up. The earlier test rendered activityBody directly, so it never went through
// mainBoxWantRows/distributeMainBoxHeights/visibleLines — the wrap was being clipped
// back out of existence in the real dashboard.
func TestWrappedTailSurvivesTheRealLayout(t *testing.T) {
	for _, layout := range []struct {
		name          string
		width, height int
	}{
		{name: "wide", width: wideBreakpointWidth, height: wideBreakpointHeight},
		{name: "compact", width: 80, height: 24},
	} {
		t.Run(layout.name, func(t *testing.T) {
			current := populatedViewModel(t)
			current.width, current.height = layout.width, layout.height
			current.status.PendingAction = nil
			// Two long lines with distinct markers at their ends, so clipping is visible.
			// The N×3 cap drops the *end* of a very long line by design, so the marker
			// that proves a logical line survived goes at its head.
			for _, marker := range []string{"FIRST-LINE-HEAD", "SECOND-LINE-HEAD"} {
				line := runner.Line{
					Attempt: 1,
					Stream:  runner.StreamStdout,
					Text: marker + " " +
						strings.Repeat("a long streamed progress sentence ", 4),
				}
				updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
					Kind:  pipeline.EventStepLog,
					RunID: "run-1",
					Step:  pipeline.StepImplementation,
					Role:  "impl_writer",
					CLI:   "codex",
					Line:  &line,
				}})
				current = updated.(model)
			}

			truncated := ansi.Strip(renderDashboard(current))
			updated, _ := current.Update(printableKey('w'))
			wrapped := updated.(model)
			frame := ansi.Strip(renderDashboard(wrapped))

			// Wrapping must reveal more of the text than truncation did.
			if len(strings.Join(strings.Fields(frame), "")) <=
				len(strings.Join(strings.Fields(truncated), "")) {
				t.Fatalf("wrapping revealed nothing in the real dashboard:\n%s", frame)
			}
			// Both logical lines must still be present — the wrap must not clip the
			// older one away.
			for _, marker := range []string{"FIRST-LINE-HEAD", "SECOND-LINE-HEAD"} {
				if !strings.Contains(frame, marker) {
					t.Fatalf("%s lost %q:\n%s", layout.name, marker, frame)
				}
			}
			// And the frame still fits its terminal.
			for index, row := range strings.Split(frame, "\n") {
				if cells := ansi.StringWidth(row); cells > layout.width {
					t.Fatalf("row %d is %d cells wide (limit %d)",
						index, cells, layout.width)
				}
			}
			if rows := strings.Count(frame, "\n") + 1; rows > layout.height {
				t.Fatalf("the frame grew to %d rows (limit %d)", rows, layout.height)
			}
		})
	}
}

// f4: the columns stay on the first row *with* the first fragment, and continuations
// start at the message column — not at cell 2, and not after an empty head row.
func TestWrappedTailKeepsItsColumns(t *testing.T) {
	current := populatedViewModel(t)
	line := runner.Line{
		Attempt: 1,
		Stream:  runner.StreamStdout,
		Text:    strings.Repeat("wrapped progress text ", 10),
	}
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:  pipeline.EventStepLog,
		RunID: "run-1",
		Step:  pipeline.StepImplementation,
		Role:  "impl_writer",
		CLI:   "codex",
		Line:  &line,
	}})
	current = updated.(model)
	current.wrapTail = true

	rows := strings.Split(ansi.Strip(activityBody(current, 80, 5)), "\n")
	if len(rows) < 2 {
		t.Fatalf("the line did not wrap:\n%s", strings.Join(rows, "\n"))
	}
	// The head row carries both the columns and text.
	if !strings.Contains(rows[0], "impl_writer") {
		t.Fatalf("the head row lost its columns: %q", rows[0])
	}
	if !strings.Contains(rows[0], "wrapped progress text") {
		t.Fatalf("the head row has no message — a row was spent on nothing: %q", rows[0])
	}
	// Continuations start under the message column, which is where the head row's own
	// text starts.
	// Cells, not bytes: the columns hold `·` and the text may hold CJK, so a byte
	// offset would silently disagree with the screen (review T13a-2-r2 f2).
	messageColumn := ansi.StringWidth(rows[0][:strings.Index(rows[0], "wrapped")])
	if messageColumn <= 2 {
		t.Fatalf("the message column looks wrong (%d): %q", messageColumn, rows[0])
	}
	for _, row := range rows[1:] {
		indent := ansi.StringWidth(
			row[:len(row)-len(strings.TrimLeft(row, " "))],
		)
		if indent != messageColumn {
			t.Fatalf("continuation starts at cell %d, want %d: %q",
				indent, messageColumn, row)
		}
	}
}

// The same alignment with wide runes in the columns and the message: a byte-offset
// indent drifts here, a cell-measured one does not (review T13a-2-r2 f2).
func TestWrappedTailAlignsWithWideRunes(t *testing.T) {
	current := populatedViewModel(t)
	line := runner.Line{
		Attempt: 1,
		Stream:  runner.StreamStdout,
		Text:    strings.Repeat("한국어 진행 상황 텍스트 ", 8),
	}
	updated, _ := current.Update(pipelineEventMsg{Event: pipeline.Event{
		Kind:  pipeline.EventStepLog,
		RunID: "run-1",
		Step:  pipeline.StepImplementation,
		Role:  "impl_writer",
		CLI:   "codex",
		Line:  &line,
	}})
	current = updated.(model)
	current.wrapTail = true

	rows := strings.Split(ansi.Strip(activityBody(current, 80, 5)), "\n")
	if len(rows) < 2 {
		t.Fatalf("the wide-rune line did not wrap:\n%s", strings.Join(rows, "\n"))
	}
	at := strings.Index(rows[0], "한국어")
	if at < 0 {
		t.Fatalf("the head row lost its message: %q", rows[0])
	}
	want := ansi.StringWidth(rows[0][:at])
	for _, row := range rows[1:] {
		indent := ansi.StringWidth(
			row[:len(row)-len(strings.TrimLeft(row, " "))],
		)
		if indent != want {
			t.Fatalf("continuation starts at cell %d, want %d: %q", indent, want, row)
		}
	}
}
