package ui

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/state"

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

// syncBuffer collects the program's frames so a test can wait for one. tea writes
// from its own goroutine, so the access has to be guarded.
type syncBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (buffer *syncBuffer) Write(chunk []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, chunk...)
	return len(chunk), nil
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return ansi.Strip(string(buffer.data))
}

// countingPlanExecutor records every request. blockingPlanExecutor drops extras on a
// non-blocking send, so it cannot witness "exactly once" (review T15-r2 f2).
type countingPlanExecutor struct {
	mu       sync.Mutex
	requests []runner.RunRequest
}

func (executor *countingPlanExecutor) Run(
	_ context.Context,
	request runner.RunRequest,
) (runner.RunResult, error) {
	executor.mu.Lock()
	executor.requests = append(executor.requests, request)
	executor.mu.Unlock()
	// A zero exit with no artifact fails the role's postcondition, which is enough to
	// drive the run to a terminal phase without pretending to be a real CLI.
	return runner.RunResult{Exit: 0}, nil
}

func (executor *countingPlanExecutor) count() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return len(executor.requests)
}

func (executor *countingPlanExecutor) mutating() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	total := 0
	for _, request := range executor.requests {
		if request.Effect == runner.EffectMutating {
			total++
		}
	}
	return total
}

func waitForFrame(t *testing.T, buffer *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buffer.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the dashboard never rendered %q:\n%s", want, buffer.String())
}

// The prompt paths through the *real* program: bare reject and the two resume
// branches. Round-1's tests only covered the immediate-dispatch cases, so a
// regression that sent auth resume to a prompt — or dropped the artifact load the
// feedback is written against — would have gone unnoticed (review T15-r2 f2).
func TestOpenPromptPathsRunThroughTheRealProgram(t *testing.T) {
	t.Run("bare reject asks, shows the plan, then dispatches", func(t *testing.T) {
		root, currentRun := seedAwaitingIntegrationRun(t, "open-bare-reject")
		// The seeded run already has a plan.md whose sha256 the state records, so it
		// must not be rewritten — the pipeline verifies that hash. Assert on a line from
		// the plan that is already there.
		//
		// Asserting on the *box title* would prove nothing: the box is drawn whether or
		// not the artifacts loaded. The assertion has to be on content that only exists
		// after the load (self-correction while verifying this test).
		const planMarker = "Prove UI actions use the pipeline control plane"
		executor := &countingPlanExecutor{}
		input, keyboard := io.Pipe()
		output := &syncBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			_ = keyboard.Close()
		})

		done := make(chan error, 1)
		go func() {
			_, err := Open(ctx, executor, root, currentRun.ID, "reject", nil,
				RunOptions{
					Interactive: true,
					Input:       input,
					Output:      output,
					Width:       140,
					Height:      40,
				})
			done <- err
		}()

		// The prompt is open and nothing has been dispatched yet.
		waitForFrame(t, output, "Reject feedback")
		if executor.count() != 0 {
			t.Fatalf("a bare reject dispatched before the answer: %d requests",
				executor.count())
		}
		// The artifacts the feedback is written against are on screen. This is what the
		// event-path seeding exists for — a blank pane makes the prompt unanswerable.
		waitForFrame(t, output, planMarker)

		if _, err := keyboard.Write([]byte("needs a stricter gate\r")); err != nil {
			t.Fatal(err)
		}
		// Submitting reaches the core: replanning is a read-only subprocess.
		deadline := time.Now().Add(10 * time.Second)
		for executor.count() == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if executor.count() == 0 {
			t.Fatalf("submitting the feedback dispatched nothing:\n%s", output.String())
		}
		if executor.mutating() != 0 {
			t.Fatal("a reject dispatched a mutating request")
		}
		cancel()
		<-done
	})

	t.Run("auth resume dispatches without asking", func(t *testing.T) {
		repoRoot, config := newIntegrationRepository(t)
		currentRun := createIntegrationRun(t, repoRoot, "open-auth-resume", config)
		if err := currentRun.State.PauseForAuth(nil, "claude is not logged in"); err != nil {
			t.Fatal(err)
		}
		if err := currentRun.SaveState(); err != nil {
			t.Fatal(err)
		}

		executor := &countingPlanExecutor{}
		input, keyboard := io.Pipe()
		output := &syncBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			_ = keyboard.Close()
		})

		done := make(chan error, 1)
		go func() {
			_, err := Open(ctx, executor, repoRoot, currentRun.ID, "resume", nil,
				RunOptions{
					Interactive: true,
					Input:       input,
					Output:      output,
					Width:       140,
					Height:      40,
				})
			done <- err
		}()

		// auth needs no answer: the run resumes on its own.
		deadline := time.Now().Add(10 * time.Second)
		for executor.count() == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if executor.count() == 0 {
			t.Fatalf("auth resume never dispatched:\n%s", output.String())
		}
		if frame := output.String(); strings.Contains(frame, "Response · auth") {
			t.Fatalf("auth resume asked for an answer it must not need:\n%s", frame)
		}
		cancel()
		<-done
	})

	t.Run("task_cap resume asks which way and prefills retry", func(t *testing.T) {
		repoRoot, config := newIntegrationRepository(t)
		currentRun := createIntegrationRun(t, repoRoot, "open-cap-resume", config)
		currentRun.State.TaskOrder = []string{"T1"}
		currentRun.State.Tasks = map[string]*state.TaskState{
			"T1": {Status: state.TaskOpen},
		}
		taskID := "T1"
		currentRun.State.CurrentTaskID = &taskID
		// task_cap only exists while implementing, so the run has to get there first.
		for _, phase := range []state.Phase{
			state.PhaseAwaitingApproval,
			state.PhaseImplementing,
		} {
			if err := currentRun.State.TransitionPhase(phase); err != nil {
				t.Fatal(err)
			}
		}
		if err := currentRun.State.PauseForTaskCap("T1", "attempt cap reached"); err != nil {
			t.Fatal(err)
		}
		if err := currentRun.SaveState(); err != nil {
			t.Fatal(err)
		}

		executor := &countingPlanExecutor{}
		input, keyboard := io.Pipe()
		output := &syncBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			_ = keyboard.Close()
		})

		done := make(chan error, 1)
		go func() {
			_, err := Open(ctx, executor, repoRoot, currentRun.ID, "resume", nil,
				RunOptions{
					Interactive: true,
					Input:       input,
					Output:      output,
					Width:       140,
					Height:      40,
				})
			done <- err
		}()

		waitForFrame(t, output, "choose")
		frame := output.String()
		if !strings.Contains(frame, "▸ retry") {
			t.Fatalf("the pick did not prefill retry:\n%s", frame)
		}
		if executor.count() != 0 {
			t.Fatalf("task_cap resume dispatched before the choice: %d", executor.count())
		}
		cancel()
		<-done
	})
}

// Exactly-once through the real program: the observer re-emits snapshots while an
// approve runs, and a rearmed guard would dispatch a second time. The second call
// fails the controller's phase check, so it surfaces as an error out of Open — that is
// the witness. A counting executor (not the dropping one) records every request
// (review T15-r2 f2).
func TestOpenDispatchesExactlyOnceDespiteResnapshots(t *testing.T) {
	root, currentRun := seedAwaitingIntegrationRun(t, "open-once")
	executor := &countingPlanExecutor{}
	input, keyboard := io.Pipe()
	output := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = keyboard.Close()
	})

	done := make(chan error, 1)
	go func() {
		_, err := Open(ctx, executor, root, currentRun.ID, "approve", nil,
			RunOptions{
				Interactive: true,
				Input:       input,
				Output:      output,
				Width:       140,
				Height:      40,
			})
		done <- err
	}()

	// Wait for the approve to reach the core.
	deadline := time.Now().Add(10 * time.Second)
	for executor.mutating() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if executor.mutating() == 0 {
		t.Fatalf("approve never dispatched:\n%s", output.String())
	}
	// Give the observer time to re-emit its snapshots; a rearmed guard would approve
	// again and the second call would fail the phase check.
	time.Sleep(300 * time.Millisecond)
	frame := output.String()
	for _, doubled := range []string{
		"requires awaiting_approval",
		"approve requires",
	} {
		if strings.Contains(frame, doubled) {
			t.Fatalf("the seed dispatched twice (%q):\n%s", doubled, frame)
		}
	}
	cancel()
	<-done
}
