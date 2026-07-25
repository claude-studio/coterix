package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

type fakeBrowseControl struct {
	statuses []pipeline.RunStatus
	err      error
	calls    int
}

func (fake *fakeBrowseControl) Status(
	_ context.Context,
	_ string,
	_ string,
) ([]pipeline.RunStatus, error) {
	fake.calls++
	return fake.statuses, fake.err
}

// assertMarkedRow pins the invariant the marker exists for: the physical line carrying
// `▌▸` is the line carrying `chosenStatus().RunID`, and it is the only marked line.
func assertMarkedRow(t *testing.T, current browserModel) {
	t.Helper()
	want := current.chosenStatus().RunID
	marked := make([]string, 0, 1)
	for _, line := range strings.Split(ansi.Strip(renderBrowser(current)), "\n") {
		if strings.HasPrefix(line, "▌▸") {
			marked = append(marked, line)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("expected exactly one marked line, got %d:\n%s",
			len(marked), strings.Join(marked, "\n"))
	}
	if !strings.Contains(marked[0], want) {
		t.Fatalf("the marker is on %q but the selection is %q", marked[0], want)
	}
}

func browserWith(t *testing.T, single bool, statuses ...pipeline.RunStatus) browserModel {
	t.Helper()
	currentTheme, err := loadTheme()
	if err != nil {
		t.Fatal(err)
	}
	return browserModel{
		theme:    currentTheme,
		statuses: statuses,
		single:   single,
		width:    wideBreakpointWidth,
		height:   wideBreakpointHeight,
	}
}

func pressBrowser(model browserModel, key rune) browserModel {
	updated, _ := model.Update(printableKey(key))
	return updated.(browserModel)
}

// W2: bare `status` lists the runs, `j/k` moves, `enter` opens the detail, and `esc`
// comes back to the list (T16 W2).
func TestBrowserPickerMovesOpensAndReturns(t *testing.T) {
	current := browserWith(
		t,
		false,
		pipeline.RunStatus{RunID: "run-a", Phase: state.PhaseImplementing},
		pipeline.RunStatus{RunID: "run-b", Phase: state.PhaseAwaitingApproval},
		pipeline.RunStatus{RunID: "run-c", Phase: state.PhaseDone},
	)

	list := ansi.Strip(renderBrowser(current))
	for _, runID := range []string{"run-a", "run-b", "run-c"} {
		if !strings.Contains(list, runID) {
			t.Fatalf("the picker omits %q:\n%s", runID, list)
		}
	}
	// The marker has to be on the *selected run's own line*. Asserting that `▌▸`
	// exists somewhere let the gutter sit on the table header while `enter` opened a
	// different run (review T16 f1).
	assertMarkedRow(t, current)

	current = pressBrowser(current, 'j')
	if current.selected != 1 {
		t.Fatalf("j selected %d", current.selected)
	}
	assertMarkedRow(t, current)
	current = pressBrowser(current, 'j')
	current = pressBrowser(current, 'j')
	if current.selected != 2 {
		t.Fatalf("j ran past the end: %d", current.selected)
	}
	assertMarkedRow(t, current)
	current = pressBrowser(current, 'k')
	if current.selected != 1 || current.chosenStatus().RunID != "run-b" {
		t.Fatalf("k selected %d (%s)", current.selected, current.chosenStatus().RunID)
	}
	assertMarkedRow(t, current)

	updated, _ := current.Update(specialKey(tea.KeyEnter))
	current = updated.(browserModel)
	if !current.showingDetail() {
		t.Fatal("enter did not open the detail")
	}
	detail := ansi.Strip(renderBrowser(current))
	if !strings.Contains(detail, "run-b") || strings.Contains(detail, "run-c") {
		t.Fatalf("the detail is not the selected run:\n%s", detail)
	}

	updated, _ = current.Update(specialKey(tea.KeyEscape))
	current = updated.(browserModel)
	if current.showingDetail() {
		t.Fatal("esc did not return to the list")
	}
}

// W1: the browser mutates nothing. a/r/e leave it with the command the dashboard
// should open, and only in the phase where that command is legal (T16 W1).
func TestBrowserActionsLaunchTheDashboardOnlyWhenLegal(t *testing.T) {
	for _, test := range []struct {
		name    string
		key     rune
		phase   state.Phase
		command string
	}{
		{name: "approve", key: 'a', phase: state.PhaseAwaitingApproval, command: "approve"},
		{name: "reject", key: 'r', phase: state.PhaseAwaitingApproval, command: "reject"},
		{name: "respond", key: 'e', phase: state.PhasePausedForInput, command: "resume"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := browserWith(t, true, pipeline.RunStatus{
				RunID: "run-x",
				Phase: test.phase,
			})
			updated, command := current.Update(printableKey(test.key))
			chosen := updated.(browserModel)
			if command == nil {
				t.Fatal("the action did not leave the browser")
			}
			if chosen.action.Command != test.command ||
				chosen.action.RunID != "run-x" {
				t.Fatalf("action=%#v", chosen.action)
			}
		})

		t.Run(test.name+" is refused in the wrong phase", func(t *testing.T) {
			current := browserWith(t, true, pipeline.RunStatus{
				RunID: "run-x",
				Phase: state.PhaseImplementing,
			})
			updated, command := current.Update(printableKey(test.key))
			refused := updated.(browserModel)
			if command != nil {
				t.Fatal("an illegal action left the browser")
			}
			if refused.action.Command != "" {
				t.Fatalf("an illegal action was recorded: %#v", refused.action)
			}
			// Refusal is explained rather than silently dropped.
			frame := ansi.Strip(renderBrowser(refused))
			if !strings.Contains(frame, "needs phase") {
				t.Fatalf("the refusal was silent:\n%s", frame)
			}
		})
	}

	// The hint line only advertises what the phase allows.
	approving := browserWith(t, true, pipeline.RunStatus{
		RunID: "run-x",
		Phase: state.PhaseAwaitingApproval,
	})
	if hints := browserHints(approving); !strings.Contains(hints, "a approve") ||
		strings.Contains(hints, "e respond") {
		t.Fatalf("hints=%q", hints)
	}
	running := browserWith(t, true, pipeline.RunStatus{
		RunID: "run-x",
		Phase: state.PhaseImplementing,
	})
	if hints := browserHints(running); strings.Contains(hints, "approve") {
		t.Fatalf("hints offer an illegal action: %q", hints)
	}
}

// `status <run_id>` has no list behind it, so esc quits rather than going nowhere.
func TestBrowserSingleRunHasNoListToReturnTo(t *testing.T) {
	current := browserWith(t, true, pipeline.RunStatus{
		RunID: "run-only",
		Phase: state.PhaseDone,
	})
	if !current.showingDetail() {
		t.Fatal("single-run mode did not start in the detail")
	}
	updated, command := current.Update(specialKey(tea.KeyEscape))
	if command == nil {
		t.Fatal("esc did not quit single-run mode")
	}
	if !updated.(browserModel).showingDetail() {
		t.Fatal("single-run mode fell back to a list that does not exist")
	}
	if hints := browserHints(current); strings.Contains(hints, "esc back") {
		t.Fatalf("single-run mode offers a way back: %q", hints)
	}
}

// An empty runs directory is a normal state, not an error.
func TestBrowserHandlesNoRuns(t *testing.T) {
	current := browserWith(t, false)
	frame := ansi.Strip(renderBrowser(current))
	if !strings.Contains(frame, "no runs") {
		t.Fatalf("the empty state is not explained:\n%s", frame)
	}
	if !strings.Contains(browserHints(current), "q quit") {
		t.Fatal("the empty state offers no way out")
	}
	// Keys that need a run must not panic or invent one.
	for _, key := range []rune{'a', 'r', 'e', 'j', 'k'} {
		next := pressBrowser(current, key)
		if next.action.Command != "" {
			t.Fatalf("key %q acted with no runs", key)
		}
	}
}

// Browse reads through the control plane once and reports what it read.
func TestBrowseReadsOnceAndSurfacesErrors(t *testing.T) {
	statuses := []pipeline.RunStatus{{RunID: "run-1", Phase: state.PhaseDone}}
	fake := &fakeBrowseControl{statuses: statuses}
	status, command, err := Browse(
		context.Background(),
		fake,
		t.TempDir(),
		"run-1",
		RunOptions{Interactive: false, Width: 100, Height: 30},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Fatalf("the control plane was read %d times", fake.calls)
	}
	if status.RunID != "run-1" || command != "" {
		t.Fatalf("status=%#v command=%q", status, command)
	}

	failure := errors.New("pipeline: open run: no such run")
	if _, _, err := Browse(
		context.Background(),
		&fakeBrowseControl{err: failure},
		t.TempDir(),
		"missing",
		RunOptions{Interactive: false},
	); !errors.Is(err, failure) {
		t.Fatalf("err=%v want the load failure", err)
	}

	// Browsing one run with a mismatched status count is a programming error.
	if _, _, err := Browse(
		context.Background(),
		&fakeBrowseControl{statuses: append(statuses, pipeline.RunStatus{RunID: "x"})},
		t.TempDir(),
		"run-1",
		RunOptions{Interactive: false},
	); err == nil {
		t.Fatal("two statuses were accepted for one run")
	}
}

// The real `ui.Browse` program with injected keys. The model tests above drive
// `browserModel.Update` directly, so they never run tea.NewProgram, the input handling,
// or the action that Browse returns — a regression that stopped returning the chosen
// command would pass them all (review T16 f2).
func TestBrowseRunsTheRealProgramAndReturnsTheChoice(t *testing.T) {
	statuses := []pipeline.RunStatus{
		{RunID: "pick-a", Phase: state.PhaseImplementing},
		{RunID: "pick-b", Phase: state.PhaseAwaitingApproval},
	}

	t.Run("move, open, then act", func(t *testing.T) {
		input, keyboard := io.Pipe()
		output := &syncBuffer{}
		t.Cleanup(func() { _ = keyboard.Close() })

		type result struct {
			status  pipeline.RunStatus
			command string
			err     error
		}
		done := make(chan result, 1)
		go func() {
			status, command, err := Browse(
				context.Background(),
				&fakeBrowseControl{statuses: statuses},
				t.TempDir(),
				"",
				RunOptions{
					Interactive: true,
					Input:       input,
					Output:      output,
					Width:       140,
					Height:      40,
				},
			)
			done <- result{status: status, command: command, err: err}
		}()

		waitForFrame(t, output, "pick-a")
		// Move to the second run, open it, then approve — the phase only allows that on
		// pick-b, and `a` is only accepted once the detail is showing. So the returned
		// value alone proves all three steps landed.
		//
		// Measured: tea repaints only the cells that changed, so the accumulated output
		// holds fragments like "▌▸p" rather than whole frames after the first paint —
		// intermediate frames cannot be substring-matched. The contract to assert is
		// what Browse returns.
		for _, key := range []string{"j", "\r", "a"} {
			if _, err := keyboard.Write([]byte(key)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(80 * time.Millisecond)
		}

		select {
		case got := <-done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.command != "approve" || got.status.RunID != "pick-b" {
				t.Fatalf("Browse returned %q for %q, want approve for pick-b",
					got.command, got.status.RunID)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Browse never returned:\n%s", output.String())
		}
	})

	t.Run("quitting returns no command", func(t *testing.T) {
		input, keyboard := io.Pipe()
		output := &syncBuffer{}
		t.Cleanup(func() { _ = keyboard.Close() })

		done := make(chan string, 1)
		go func() {
			_, command, _ := Browse(
				context.Background(),
				&fakeBrowseControl{statuses: statuses},
				t.TempDir(),
				"",
				RunOptions{
					Interactive: true,
					Input:       input,
					Output:      output,
					Width:       140,
					Height:      40,
				},
			)
			done <- command
		}()

		waitForFrame(t, output, "pick-a")
		if _, err := keyboard.Write([]byte("q")); err != nil {
			t.Fatal(err)
		}
		select {
		case command := <-done:
			if command != "" {
				t.Fatalf("quitting returned the command %q", command)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("q did not quit the browser:\n%s", output.String())
		}
	})
}

// Why the browser needs an observer-less controller — and why attaching one anyway
// cannot corrupt it: the browser has no pipeline-event handling at all. `Status` emits
// one snapshot per run it reads, and this pins both halves of that contract
// (T16 W1/W2 · review T16 f2).
func TestBrowserIgnoresPipelineEventsEntirely(t *testing.T) {
	current := browserWith(
		t,
		false,
		pipeline.RunStatus{RunID: "run-a", Phase: state.PhaseImplementing},
		pipeline.RunStatus{RunID: "run-b", Phase: state.PhaseAwaitingApproval},
	)
	current = pressBrowser(current, 'j')
	before := renderBrowser(current)

	// Whatever the controller emits, the browser is a different model: it has no case
	// for these messages, so N snapshots cannot overwrite its selection or fire an
	// artifact chain the way the dashboard's model would.
	replacement := pipeline.RunStatus{RunID: "intruder", Phase: state.PhaseDone}
	for index := 0; index < 5; index++ {
		updated, command := current.Update(pipelineEventMsg{Event: pipeline.Event{
			Kind:   pipeline.EventStateSnapshot,
			RunID:  "intruder",
			Status: &replacement,
		}})
		if command != nil {
			t.Fatal("a pipeline event made the browser run a command")
		}
		current = updated.(browserModel)
	}
	if current.selected != 1 || current.chosenStatus().RunID != "run-b" {
		t.Fatalf("pipeline events moved the selection to %q",
			current.chosenStatus().RunID)
	}
	if renderBrowser(current) != before {
		t.Fatal("pipeline events changed what the browser draws")
	}

	// And the production loader really does emit one snapshot per run, which is the
	// reason the observer must stay off it.
	root, currentRun := seedAwaitingIntegrationRun(t, "browse-observer")
	events := 0
	observing := pipeline.NewController(
		newBlockingPlanExecutor(),
		pipeline.WithObserver(func(pipeline.Event) { events++ }),
	)
	if _, err := observing.Status(context.Background(), root, currentRun.ID); err != nil {
		t.Fatal(err)
	}
	if events == 0 {
		t.Fatal("Status emitted nothing — the observer-less requirement would be moot")
	}
}
