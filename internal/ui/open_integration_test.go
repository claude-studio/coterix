package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ridenow/coterix/internal/runner"
)

// These exercise the real `ui.Open` — the program, the observer wiring, the status
// load and the once-guard — against runs that exist on disk.
//
// The synthetic model tests seed `openRunID`/`pendingOperation` by hand and feed a
// hand-built snapshot, which skips application.go, newOpenModel, Init's Status call,
// the observer, and the errors.Join that carries the failure out. `ui.Open` could stop
// passing the seed through entirely and those tests would still pass (review T15 f2).
func TestOpenDispatchesThroughTheRealProgram(t *testing.T) {
	t.Run("approve reaches the executor", func(t *testing.T) {
		root, currentRun := seedAwaitingIntegrationRun(t, "open-approve")
		executor := newBlockingPlanExecutor()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			executor.stop()
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			// Not interactive: no keyboard is needed because Open dispatches on its own.
			_, _ = Open(
				ctx,
				executor,
				root,
				currentRun.ID,
				"approve",
				nil,
				RunOptions{Width: 120, Height: 30},
			)
		}()

		// The seed reached the core: the approve produced a mutating subprocess.
		request := waitForIntegrationExecutor(t, executor)
		if request.Effect != runner.EffectMutating {
			cancel()
			executor.stop()
			t.Fatalf("approve dispatched effect=%d, want mutating", request.Effect)
		}
		cancel()
		executor.stop()
		<-done

		// And it really went through the run on disk.
		persisted := openIntegrationRun(t, root, currentRun.ID)
		if persisted.State.ApprovedPlanHash == nil {
			t.Fatal("approve did not persist through the real program")
		}
	})

	t.Run("reject carries its response", func(t *testing.T) {
		root, currentRun := seedAwaitingIntegrationRun(t, "open-reject")
		executor := newBlockingPlanExecutor()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			executor.stop()
		})

		response := "revise the gate command"
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = Open(
				ctx,
				executor,
				root,
				currentRun.ID,
				"reject",
				&response,
				RunOptions{Width: 120, Height: 30},
			)
		}()

		// A reject with a response replans, which is a read-only planning subprocess.
		request := waitForIntegrationExecutor(t, executor)
		if request.Effect == runner.EffectMutating {
			cancel()
			executor.stop()
			t.Fatal("reject dispatched a mutating effect")
		}
		// The response has to have travelled all the way into the revision prompt —
		// that is the whole reason Open takes it (T15 W1 · R2).
		carried := strings.Join(request.Args, " ") + string(request.Stdin)
		if !strings.Contains(carried, response) {
			cancel()
			executor.stop()
			t.Fatalf("the reject response never reached the prompt:\n%s", carried)
		}
		cancel()
		executor.stop()
		<-done
	})

	// f1: a bare resume on a run that is not paused must reach the controller and get
	// its validation error — not consume the seed and sit in a blank dashboard.
	t.Run("bare resume on a wrong phase errors out", func(t *testing.T) {
		root, currentRun := seedAwaitingIntegrationRun(t, "open-resume-wrong")
		executor := newBlockingPlanExecutor()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			executor.stop()
		})

		type outcome struct{ err error }
		results := make(chan outcome, 1)
		go func() {
			_, err := Open(
				ctx,
				executor,
				root,
				currentRun.ID,
				"resume",
				nil,
				RunOptions{Width: 120, Height: 30},
			)
			results <- outcome{err: err}
		}()

		select {
		case got := <-results:
			if got.err == nil {
				t.Fatal("a resume on the wrong phase returned no error — the headless " +
					"command exits 1, so this must too")
			}
			if !strings.Contains(got.err.Error(), "paused_for_input") {
				t.Fatalf("err=%v want the controller's validation message", got.err)
			}
		case <-time.After(10 * time.Second):
			cancel()
			executor.stop()
			t.Fatal("Open hung instead of reporting the wrong phase")
		}
	})

	t.Run("a missing run returns an error", func(t *testing.T) {
		root, _ := newIntegrationRepository(t)
		executor := newBlockingPlanExecutor()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			executor.stop()
		})

		results := make(chan error, 1)
		go func() {
			_, err := Open(
				ctx,
				executor,
				root,
				"no-such-run",
				"approve",
				nil,
				RunOptions{Width: 120, Height: 30},
			)
			results <- err
		}()

		select {
		case err := <-results:
			if err == nil {
				t.Fatal("a missing run exited cleanly — parity with the non-TTY exit 1 " +
					"requires an error (T15-3R-1)")
			}
		case <-time.After(10 * time.Second):
			cancel()
			executor.stop()
			t.Fatal("Open hung on a missing run instead of quitting")
		}
	})

	t.Run("only the control commands can open a run", func(t *testing.T) {
		root, currentRun := seedAwaitingIntegrationRun(t, "open-bad-command")
		if _, err := Open(
			context.Background(),
			newBlockingPlanExecutor(),
			root,
			currentRun.ID,
			"status",
			nil,
			RunOptions{Width: 120, Height: 30},
		); err == nil {
			t.Fatal("`status` was accepted as an interactive entry")
		}
		if _, err := Open(
			context.Background(),
			newBlockingPlanExecutor(),
			root,
			"",
			"approve",
			nil,
			RunOptions{Width: 120, Height: 30},
		); err == nil {
			t.Fatal("an empty run id was accepted")
		}
	})
}
