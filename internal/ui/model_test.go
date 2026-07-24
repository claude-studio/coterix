package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
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

func TestModelStreamsBoundedLogsAndAttemptCompletion(t *testing.T) {
	current := testModel(t, &fakeUIControl{})
	for index := 0; index < maxLogLines+25; index++ {
		line := runner.Line{
			Attempt: 1,
			Stream:  runner.StreamStdout,
			Text:    "line",
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
	if len(current.logs) != maxLogLines {
		t.Fatalf("log buffer length=%d want=%d", len(current.logs), maxLogLines)
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
