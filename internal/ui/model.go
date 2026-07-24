package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// Activity tail limits (plan T13 W2). The buffer always retains
// maxActivityLines so a step that turns out to have failed can widen its tail
// after the fact — the failure signal arrives only once the lines are already
// in. Rendering truncates to the wide/compact limit while the step looks
// healthy, and to the full buffer once it failed.
const (
	maxActivityLines    = 15
	activityTailWide    = 5
	activityTailCompact = 3
)

// mainBox identifies a scrollable box in the main pane. Each box keeps its own
// scroll offset and `tab` moves focus between them, so scrolling the artifacts
// no longer displaces the live activity and vice versa (T14 W1/W2).
// boxFeed is deliberately the zero value: it is the default focus and PENDING
// only exists while the run is blocked.
type mainBox uint8

const (
	boxFeed mainBox = iota
	boxLiveOutput
	boxActivity
	boxPending
	mainBoxCount
)

// maxScrollSentinel parks a box at its oldest line; visibleLines clamps it.
const maxScrollSentinel = 1 << 20

type promptMode uint8

const (
	promptNone promptMode = iota
	promptReject
	promptResume
)

// logIcon is a deterministic event-kind hint for the feed icon column. It is
// injected where the event is appended instead of being inferred from text.
type logIcon uint8

const (
	logIconNone logIcon = iota
	logIconStart
	logIconDone
	logIconFail
)

type logEntry struct {
	Step    string
	Role    string
	CLI     string
	Stream  runner.Stream
	Text    string
	Attempt int
	At      time.Time
	Icon    logIcon
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
	now                 func() time.Time
	stages              stageClock
	logs                []logEntry
	activity            []logEntry
	activityFailed      bool
	artifacts           artifactData
	artifactKey         string
	artifactLoadingKey  string
	artifactLoadedKey   string
	artifactRender      string
	artifactRenderErr   error
	artifactRenderWidth int
	focus               mainBox
	boxScroll           [mainBoxCount]int
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
	activity.Spinner = workingSpinner(theme.tokens)
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
		now:       time.Now,
		logs:      make([]logEntry, 0, maxLogLines),
		spinner:   activity,
		operation: operationRun,
	}
}

func (current model) Init() tea.Cmd {
	operation := runOperation(
		current.ctx,
		current.control,
		current.tracker,
		operationRun,
		current.repoRoot,
		current.request,
		"",
		nil,
	)
	return tea.Batch(operation, current.spinnerTickCommand())
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
					logIconFail,
					fmt.Sprintf("artifact refresh: %v", message.err),
				)
			} else {
				current.artifacts = message.data
				current.artifactLoadedKey = message.key
				current.refreshArtifactRender()
			}
		}
		return current, nil
	case spinner.TickMsg:
		if !current.isWorking() {
			return current, nil
		}
		var command tea.Cmd
		current.spinner, command = current.spinner.Update(message)
		return current, command
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
	wasWorking := current.isWorking()
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
		current.stages.observePhase(event.Status.Phase, current.now())
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
		current.stages.stepStarted(event.Step, current.now())
		current.resetActivity()
		current.appendSystemLog(
			runner.StreamStdout,
			logIconStart,
			fmt.Sprintf("%s · %s started", displayStep(event), event.CLI),
		)
	case pipeline.EventStepLog:
		if event.Line != nil {
			// Subprocess lines feed the pinned tail only — never the
			// scrolling lifecycle history (plan T13 W2).
			current.appendActivity(logEntry{
				Step:    event.Step,
				Role:    event.Role,
				CLI:     event.CLI,
				Stream:  event.Line.Stream,
				Text:    event.Line.Text,
				Attempt: event.Line.Attempt,
			})
		}
	case pipeline.EventAttemptFinished:
		if eventFailed(event) {
			current.activityFailed = true
		}
		current.appendAttempt(event)
	case pipeline.EventStepFinished:
		if eventFailed(event) {
			current.activityFailed = true
		}
		current.stages.stepFinished(event.Step, current.now())
		current.appendStepFinished(event)
		if current.activeStep == event.Step &&
			current.activeRole == event.Role {
			current.activeStep = ""
			current.activeRole = ""
			current.activeCLI = ""
		}
	}
	if !wasWorking && current.isWorking() {
		return current, current.spinnerTickCommand()
	}
	return current, nil
}

func (current model) finishOperation(
	result operationDoneMsg,
) (tea.Model, tea.Cmd) {
	current.operation = ""
	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	if result.status.RunID != "" {
		current.status = result.status
		current.hasStatus = true
		current.stages.observePhase(result.status.Phase, current.now())
	}
	current.operationErr = result.err
	if result.err != nil {
		current.appendSystemLog(
			runner.StreamStderr,
			logIconFail,
			result.err.Error(),
		)
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
		// Scrolling drives the focused box only (T14 W2).
		current.boxScroll[current.focus]++
	case "down", "j":
		if current.boxScroll[current.focus] > 0 {
			current.boxScroll[current.focus]--
		}
	case "home":
		current.boxScroll[current.focus] = maxScrollSentinel
	case "end":
		current.boxScroll[current.focus] = 0
	case "tab":
		current.focus = shiftMainFocus(current, 1)
	case "shift+tab":
		current.focus = shiftMainFocus(current, -1)
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
	wasWorking := current.isWorking()
	current.operation = kind
	current.operationErr = nil
	operation := runOperation(
		current.ctx,
		current.control,
		current.tracker,
		kind,
		current.repoRoot,
		current.request,
		current.status.RunID,
		response,
	)
	if wasWorking {
		return current, operation
	}
	return current, tea.Batch(operation, current.spinnerTickCommand())
}

func (current model) isWorking() bool {
	return current.operation != "" || current.activeStep != ""
}

func (current model) spinnerTickCommand() tea.Cmd {
	return func() tea.Msg {
		return current.spinner.Tick()
	}
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
	if entry.At.IsZero() && current.now != nil {
		entry.At = current.now()
	}
	current.logs = append(current.logs, entry)
	if overflow := len(current.logs) - maxLogLines; overflow > 0 {
		copy(current.logs, current.logs[overflow:])
		current.logs = current.logs[:maxLogLines]
	}
}

// appendActivity feeds the pinned activity tail instead of the lifecycle feed:
// subprocess output must not accumulate in the scrolling history (plan T13 W2).
// The buffer keeps maxActivityLines even while only the wide/compact limit is
// rendered, so a step that later reports failure can widen its tail (R4).
func (current *model) appendActivity(entry logEntry) {
	if entry.At.IsZero() && current.now != nil {
		entry.At = current.now()
	}
	// A retry is a fresh subprocess, so its output must not be mixed with the
	// previous attempt's. The pipeline emits no attempt-started event, so the
	// attempt number on the line itself is the only boundary signal available.
	if last := len(current.activity) - 1; last >= 0 &&
		current.activity[last].Attempt != entry.Attempt {
		current.resetActivity()
	}
	current.activity = append(current.activity, entry)
	if overflow := len(current.activity) - maxActivityLines; overflow > 0 {
		copy(current.activity, current.activity[overflow:])
		current.activity = current.activity[:maxActivityLines]
	}
}

// resetActivity clears the tail at a step boundary. A finished step keeps its
// tail until the next step starts, so the context right before a gate failure
// stays on screen (plan T13 W2).
func (current *model) resetActivity() {
	current.activity = current.activity[:0]
	current.activityFailed = false
}

// hasPendingPrompt reports whether the run is blocked on a question that has
// text worth showing.
func hasPendingPrompt(current model) bool {
	return current.hasStatus && current.status.PendingAction != nil &&
		strings.TrimSpace(current.status.PendingAction.Prompt) != ""
}

// mainBoxOrder is the top-to-bottom render order. PENDING leads because the run
// is blocked on it; ACTIVITY trails because it is the live edge.
func mainBoxOrder(current model) []mainBox {
	order := make([]mainBox, 0, mainBoxCount)
	if hasPendingPrompt(current) {
		order = append(order, boxPending)
	}
	return append(order, boxFeed, boxLiveOutput, boxActivity)
}

// shiftMainFocus cycles focus through the boxes that are actually on screen.
func shiftMainFocus(current model, delta int) mainBox {
	order := mainBoxOrder(current)
	for index, box := range order {
		if box == current.focus {
			return order[(index+delta+len(order))%len(order)]
		}
	}
	return order[0]
}

// eventFailed reports whether a step/attempt event carries a failure, which is
// what widens the activity tail to the full buffer (plan T13 W2 · R4).
func eventFailed(event pipeline.Event) bool {
	if event.Err != nil {
		return true
	}
	if event.Result == nil {
		return false
	}
	return event.Result.Exit != 0 || event.Result.TimedOut
}

func (current *model) appendSystemLog(
	stream runner.Stream,
	icon logIcon,
	text string,
) {
	current.appendLog(logEntry{
		Step:   "coterix",
		Role:   "control",
		CLI:    "coterix",
		Stream: stream,
		Text:   text,
		Icon:   icon,
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
	icon := logIconDone
	if event.Err != nil || event.Result.Exit != 0 || event.Result.TimedOut {
		stream = runner.StreamStderr
		icon = logIconFail
	}
	current.appendSystemLog(stream, icon, message)
}

func (current *model) appendStepFinished(event pipeline.Event) {
	message := displayStep(event) + " · finished"
	stream := runner.StreamStdout
	icon := logIconDone
	if event.Err != nil {
		message += " · " + event.Err.Error()
		stream = runner.StreamStderr
		icon = logIconFail
	}
	current.appendSystemLog(stream, icon, message)
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
