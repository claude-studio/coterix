package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const (
	maxLogLines          = 1000
	wideBreakpointWidth  = 120
	wideBreakpointHeight = 30
)

type promptMode uint8

const (
	promptNone promptMode = iota
	promptReject
	promptResume
)

type logEntry struct {
	Step    string
	Role    string
	CLI     string
	Stream  runner.Stream
	Text    string
	Attempt int
}

type artifactsLoadedMsg struct {
	runID string
	key   string
	data  artifactData
	err   error
}

type model struct {
	ctx       context.Context
	cancel    context.CancelFunc
	control   controlPlane
	tracker   *operationTracker
	repoRoot  string
	request   string
	theme     theme
	autoQuit  bool
	width     int
	height    int
	status    pipeline.RunStatus
	hasStatus bool

	activeStep          string
	activeRole          string
	activeCLI           string
	operation           operationKind
	logs                []logEntry
	artifacts           artifactData
	artifactKey         string
	artifactLoadingKey  string
	artifactLoadedKey   string
	artifactRender      string
	artifactRenderErr   error
	artifactRenderWidth int
	scroll              int
	spinner             spinner.Model

	prompt       promptMode
	promptValue  string
	promptError  string
	stopping     bool
	operationErr error
}

func newModel(
	ctx context.Context,
	cancel context.CancelFunc,
	controller controlPlane,
	repoRoot string,
	request string,
	theme theme,
	autoQuit bool,
	trackers ...*operationTracker,
) model {
	activity := spinner.New()
	activity.Spinner = spinner.Spinner{
		Frames: []string{"⋯"},
	}
	activity.Style = theme.styles.Busy
	tracker := &operationTracker{}
	if len(trackers) > 0 && trackers[0] != nil {
		tracker = trackers[0]
	}
	return model{
		ctx:       ctx,
		cancel:    cancel,
		control:   controller,
		tracker:   tracker,
		repoRoot:  repoRoot,
		request:   request,
		theme:     theme,
		autoQuit:  autoQuit,
		width:     wideBreakpointWidth,
		height:    wideBreakpointHeight,
		logs:      make([]logEntry, 0, maxLogLines),
		spinner:   activity,
		operation: operationRun,
	}
}

func (current model) Init() tea.Cmd {
	return runOperation(
		current.ctx,
		current.control,
		current.tracker,
		operationRun,
		current.repoRoot,
		current.request,
		"",
		nil,
	)
}

func (current model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		current.width = max(1, message.Width)
		current.height = max(1, message.Height)
		if dashboardMainInnerWidth(current.width, current.height) !=
			current.artifactRenderWidth {
			current.refreshArtifactRender()
		}
		return current, nil
	case pipelineEventMsg:
		return current.updatePipelineEvent(message.Event)
	case operationDoneMsg:
		return current.finishOperation(message)
	case artifactsLoadedMsg:
		if current.hasStatus &&
			message.runID == current.status.RunID &&
			message.key == current.artifactKey {
			current.artifactLoadingKey = ""
			if message.err != nil {
				current.appendSystemLog(
					runner.StreamStderr,
					fmt.Sprintf("artifact refresh: %v", message.err),
				)
			} else {
				current.artifacts = message.data
				current.artifactLoadedKey = message.key
				current.refreshArtifactRender()
			}
		}
		return current, nil
	case tea.PasteMsg:
		if current.prompt != promptNone {
			current.promptValue += message.Content
			current.promptError = ""
		}
		return current, nil
	case tea.KeyPressMsg:
		return current.updateKey(message)
	}
	return current, nil
}

func (current model) View() tea.View {
	view := tea.NewView(renderDashboard(current))
	view.AltScreen = !current.autoQuit
	view.WindowTitle = "Coterix"
	return view
}

func (current model) updatePipelineEvent(
	event pipeline.Event,
) (tea.Model, tea.Cmd) {
	switch event.Kind {
	case pipeline.EventStateSnapshot:
		if event.Status == nil {
			return current, nil
		}
		if event.RepoRoot != "" {
			current.repoRoot = event.RepoRoot
		}
		current.status = *event.Status
		current.hasStatus = true
		current.artifactKey = artifactStatusKey(current.status)
		if current.artifactKey == current.artifactLoadedKey ||
			current.artifactKey == current.artifactLoadingKey {
			return current, nil
		}
		current.artifactLoadingKey = current.artifactKey
		return current, loadArtifactsCommand(
			current.ctx,
			current.repoRoot,
			current.status,
			current.artifactKey,
		)
	case pipeline.EventStepStarted:
		current.activeStep = event.Step
		current.activeRole = event.Role
		current.activeCLI = event.CLI
		current.appendSystemLog(
			runner.StreamStdout,
			fmt.Sprintf("%s · %s started", displayStep(event), event.CLI),
		)
	case pipeline.EventStepLog:
		if event.Line != nil {
			current.appendLog(logEntry{
				Step:    event.Step,
				Role:    event.Role,
				CLI:     event.CLI,
				Stream:  event.Line.Stream,
				Text:    event.Line.Text,
				Attempt: event.Line.Attempt,
			})
		}
	case pipeline.EventAttemptFinished:
		current.appendAttempt(event)
	case pipeline.EventStepFinished:
		current.appendStepFinished(event)
		if current.activeStep == event.Step &&
			current.activeRole == event.Role {
			current.activeStep = ""
			current.activeRole = ""
			current.activeCLI = ""
		}
	}
	return current, nil
}

func (current model) finishOperation(
	result operationDoneMsg,
) (tea.Model, tea.Cmd) {
	current.operation = ""
	if result.status.RunID != "" {
		current.status = result.status
		current.hasStatus = true
	}
	current.operationErr = result.err
	if result.err != nil {
		current.appendSystemLog(runner.StreamStderr, result.err.Error())
	}
	if current.stopping {
		return current, tea.Quit
	}
	if current.autoQuit ||
		current.status.Phase == state.PhaseDone ||
		current.status.Phase == state.PhaseFailed {
		return current, tea.Quit
	}
	return current, nil
}

func (current model) updateKey(
	key tea.KeyPressMsg,
) (tea.Model, tea.Cmd) {
	if current.stopping {
		return current, nil
	}
	if current.prompt != promptNone {
		return current.updatePromptKey(key)
	}

	switch key.String() {
	case "ctrl+c", "q":
		return current.requestStop()
	case "up", "k":
		current.scroll++
	case "down", "j":
		if current.scroll > 0 {
			current.scroll--
		}
	case "home":
		current.scroll = len(current.logs) + 100000
	case "end":
		current.scroll = 0
	}

	if !current.hasStatus || current.operation != "" {
		return current, nil
	}
	switch current.status.Phase {
	case state.PhaseAwaitingApproval:
		switch key.String() {
		case "a":
			return current.beginOperation(operationApprove, nil)
		case "r":
			current.prompt = promptReject
			current.promptValue = ""
			current.promptError = ""
		}
	case state.PhasePausedForInput:
		if key.String() != "enter" || current.status.PendingAction == nil {
			return current, nil
		}
		if current.status.PendingAction.Kind == state.PendingAuth {
			return current.beginOperation(operationResume, nil)
		}
		current.prompt = promptResume
		current.promptValue = ""
		if current.status.PendingAction.Kind == state.PendingTaskCap {
			current.promptValue = "retry"
		}
		current.promptError = ""
	}
	return current, nil
}

func (current model) updatePromptKey(
	key tea.KeyPressMsg,
) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		return current.requestStop()
	case "esc":
		current.clearPrompt()
		return current, nil
	case "enter":
		response := strings.TrimSpace(current.promptValue)
		if response == "" {
			current.promptError = "Response cannot be empty."
			return current, nil
		}
		if current.prompt == promptResume &&
			current.status.PendingAction != nil &&
			current.status.PendingAction.Kind == state.PendingTaskCap &&
			response != "retry" && response != "abort" {
			current.promptError = "Task cap response must be retry or abort."
			return current, nil
		}
		mode := current.prompt
		current.clearPrompt()
		if mode == promptReject {
			return current.beginOperation(operationReject, &response)
		}
		return current.beginOperation(operationResume, &response)
	case "backspace":
		if current.promptValue != "" {
			_, size := utf8.DecodeLastRuneInString(current.promptValue)
			current.promptValue = current.promptValue[:len(current.promptValue)-size]
		}
		current.promptError = ""
		return current, nil
	}

	text := key.Key().Text
	if text != "" && !key.Key().Mod.Contains(tea.ModCtrl) {
		current.promptValue += text
		current.promptError = ""
	}
	return current, nil
}

func (current model) beginOperation(
	kind operationKind,
	response *string,
) (tea.Model, tea.Cmd) {
	current.operation = kind
	current.operationErr = nil
	return current, runOperation(
		current.ctx,
		current.control,
		current.tracker,
		kind,
		current.repoRoot,
		current.request,
		current.status.RunID,
		response,
	)
}

func (current model) requestStop() (tea.Model, tea.Cmd) {
	if current.cancel != nil {
		current.cancel()
	}
	current.clearPrompt()
	if current.operation == "" {
		return current, tea.Quit
	}
	current.stopping = true
	return current, nil
}

func (current *model) clearPrompt() {
	current.prompt = promptNone
	current.promptValue = ""
	current.promptError = ""
}

func (current *model) refreshArtifactRender() {
	width := dashboardMainInnerWidth(current.width, current.height)
	current.artifactRenderWidth = width
	current.artifactRender = ""
	current.artifactRenderErr = nil
	artifacts := markdownArtifacts(current.artifacts)
	if len(artifacts) == 0 {
		return
	}
	current.artifactRender, current.artifactRenderErr = renderMarkdown(
		current.theme,
		width,
		artifacts,
	)
}

func (current *model) appendLog(entry logEntry) {
	current.logs = append(current.logs, entry)
	if overflow := len(current.logs) - maxLogLines; overflow > 0 {
		copy(current.logs, current.logs[overflow:])
		current.logs = current.logs[:maxLogLines]
	}
}

func (current *model) appendSystemLog(stream runner.Stream, text string) {
	current.appendLog(logEntry{
		Step:   "coterix",
		Role:   "control",
		CLI:    "coterix",
		Stream: stream,
		Text:   text,
	})
}

func (current *model) appendAttempt(event pipeline.Event) {
	if event.Result == nil {
		return
	}
	message := fmt.Sprintf(
		"%s · attempt %d exited %d",
		displayStep(event),
		event.Attempt,
		event.Result.Exit,
	)
	if event.Result.TimedOut {
		message += " · timed out"
	}
	if event.Err != nil {
		message += " · " + event.Err.Error()
	}
	stream := runner.StreamStdout
	if event.Err != nil || event.Result.Exit != 0 || event.Result.TimedOut {
		stream = runner.StreamStderr
	}
	current.appendSystemLog(stream, message)
}

func (current *model) appendStepFinished(event pipeline.Event) {
	message := displayStep(event) + " · finished"
	stream := runner.StreamStdout
	if event.Err != nil {
		message += " · " + event.Err.Error()
		stream = runner.StreamStderr
	}
	current.appendSystemLog(stream, message)
}

func displayStep(event pipeline.Event) string {
	if event.Role != "" {
		return event.Role
	}
	if event.Step != "" {
		return event.Step
	}
	return "pipeline"
}

func loadArtifactsCommand(
	ctx context.Context,
	repoRoot string,
	status pipeline.RunStatus,
	key string,
) tea.Cmd {
	return func() tea.Msg {
		data, err := loadArtifactData(ctx, repoRoot, status)
		return artifactsLoadedMsg{
			runID: status.RunID,
			key:   key,
			data:  data,
			err:   err,
		}
	}
}

func artifactStatusKey(status pipeline.RunStatus) string {
	var key strings.Builder
	key.WriteString(status.RunID)
	key.WriteByte(0)
	key.WriteString(string(status.Phase))
	key.WriteByte(0)
	key.WriteString(strconv.Itoa(status.PlanRound))
	writeOptionalKey(&key, status.PlanHash)
	writeOptionalKey(&key, status.CurrentTaskID)
	if status.CurrentTaskID != nil {
		if task, exists := status.Tasks[*status.CurrentTaskID]; exists {
			writeOptionalKey(&key, task.BaseSHA)
			writeOptionalKey(&key, task.CandidateSHA)
			writeOptionalKey(&key, task.GateResult)
			writeOptionalKey(&key, task.ReviewResult)
		}
	}
	return key.String()
}

func writeOptionalKey(builder *strings.Builder, value *string) {
	builder.WriteByte(0)
	if value != nil {
		builder.WriteString(*value)
	}
}
