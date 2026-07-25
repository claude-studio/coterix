package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/ridenow/coterix/internal/cli"
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
	// boxSidebar takes part in the focus cycle but has no scroll offset of its
	// own — it always shows the full pipeline/status cards (T14 W2).
	boxSidebar
	mainBoxCount
)

// toastDuration is how long an acknowledgement stays in the status bar (T14 W8).
// Expiry is decided against the injected clock so the tests are deterministic; the
// tick only wakes the view up.
const toastDuration = 3 * time.Second

// maxScrollSentinel parks a box at its oldest line; visibleLines clamps it.
const maxScrollSentinel = 1 << 20

// artifactTab selects which artifact the FEED box shows. Stacking plan, diff and
// verdicts vertically made the box a scrolling wall, so they became tabs (T14 W3).
type artifactTab uint8

const (
	tabPlan artifactTab = iota
	tabDiff
	// tabVerdict holds *every* verdict. A run can produce several, and one tab per
	// verdict would make the count dynamic — `1/2/3` could no longer name them.
	tabVerdict
	artifactTabCount
)

type promptMode uint8

const (
	promptNone promptMode = iota
	promptReject
	promptResume
	// promptApproveConfirm is a one-step confirmation in front of `a`, so approving
	// a plan is never a single stray keypress (T14 W5). It carries no input.
	promptApproveConfirm
)

// taskCapChoices are the only valid task_cap responses. W5 turns them from typed
// text into a left/right pick, but the submitted value stays the same string the
// validator has always checked (T14 W5).
var taskCapChoices = [2]string{"retry", "abort"}

// promptRows is the status-bar budget while a prompt is open: label, framed input,
// footer. The multiline textarea needs three content rows instead of one, and the
// main pane gives them up for the duration (T14 W5).
const (
	promptRowsSingle = 4
	promptRowsArea   = 7
	promptAreaRows   = 3
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

// initialOperation is the single operation `ui.Open` dispatches on an existing run
// (T15 W1). Response has to travel with the kind: `resume --response retry` and
// `reject --response …` carry a parsed answer that the kind alone cannot express.
type initialOperation struct {
	Kind     operationKind
	Response *string
}

// runLoadedMsg reports the outcome of the pre-flight status load that `ui.Open`
// performs before dispatching (T15 W1).
type runLoadedMsg struct {
	err error
}

// toastExpiredMsg is the scheduled redraw that lets an expired acknowledgement
// disappear (T14 W8).
type toastExpiredMsg struct{}

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
	artifactTab         artifactTab
	// selectedEntry is the lifecycle entry `j/k` moves while LIVE OUTPUT has the
	// focus (T14 W4). hasSelection is the zero-value-friendly "none" flag: a fresh
	// model follows the live edge with no cursor showing.
	selectedEntry int
	hasSelection  bool
	entryExpanded bool
	// expandScroll walks the expanded block row by row. The block can be taller than
	// the rows LIVE OUTPUT is given — PENDING outranks it — so bounding the block was
	// not enough on its own to keep its head reachable (review T14c-r2 f2).
	expandScroll int
	// helpOpen shows the key overlay (T14 W6). It is the only overlay — there is no
	// dialog stack, so a bool is the whole state.
	helpOpen bool
	// wrapTail hard-wraps the activity tail instead of truncating it (T13 W9). Off by
	// default: one row per line is what keeps the tail readable at a glance.
	wrapTail bool
	// toast is a transient acknowledgement in the status bar (T14 W8). It says a
	// keypress was *accepted*, which is otherwise invisible while the operation runs.
	toast      string
	toastUntil time.Time
	focus      mainBox
	// boxScroll is each box's distance from its newest line. 0 means "follow the
	// live edge". A paused box (>0) is nudged by preserveReading when content
	// arrives, which keeps the same absolute lines on screen — without that the
	// view drifted one row per new line (T14 W1 · review T14a f2).
	boxScroll [mainBoxCount]int
	spinner   spinner.Model

	// openRunID and pendingOperation drive `ui.Open`: rather than starting a new
	// request, the model loads an existing run and then dispatches one operation on
	// it. The dispatch waits for the first state snapshot so the artifacts are on
	// screen — a reject prompt without the plan in view is unanswerable (T15 W1).
	openRunID        string
	pendingOperation *initialOperation

	prompt      promptMode
	promptValue string
	promptError string
	// promptArea is the multiline editor for reject/resume text (T14 W5). task_cap
	// and the approve confirmation do not use it — they have no free text.
	promptArea textarea.Model

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

// seedPendingOperation dispatches the `ui.Open` operation exactly once, on the first
// snapshot that actually carries a run (T15 W1). It returns a nil command when there
// is nothing pending so callers can keep their own.
func (current model) seedPendingOperation() (model, tea.Cmd) {
	if current.pendingOperation == nil || !current.hasStatus {
		return current, nil
	}
	initial := *current.pendingOperation
	current.pendingOperation = nil
	updated, command := current.dispatchInitialOperation(initial)
	seeded, ok := updated.(model)
	if !ok {
		return current, command
	}
	if command == nil {
		// A prompt was opened rather than an operation started; the state change still
		// has to be kept, so hand back a no-op command to signal "handled".
		return seeded, func() tea.Msg { return nil }
	}
	return seeded, command
}

// loadRunCommand asks the control plane for the run's current state. The controller
// emits a state snapshot through the observer as a side effect, so the model and its
// artifacts are seeded by the normal event path — seeding the fields directly would
// leave the artifact pane blank, because loadArtifactsCommand only fires from
// EventStateSnapshot (T15 W1).
func (current model) loadRunCommand() tea.Cmd {
	return func() tea.Msg {
		_, err := current.control.Status(
			current.ctx,
			current.repoRoot,
			current.openRunID,
		)
		return runLoadedMsg{err: err}
	}
}

// dispatchInitialOperation runs the operation `ui.Open` was asked for, once the run
// is on screen. Whether it dispatches immediately or asks first is decided by the
// same rule the keyboard uses: a response that was supplied goes straight through, a
// missing one is prompted for (T15 W1).
func (current model) dispatchInitialOperation(
	initial initialOperation,
) (tea.Model, tea.Cmd) {
	switch initial.Kind {
	case operationApprove:
		// approve takes no response, and the CLI entry is exempt from T14 W5's
		// confirmation step: `coterix approve <id>` already *is* the confirmation, and
		// the non-TTY path approves immediately (T15-R6 · CLI parity).
		return current.beginOperation(operationApprove, nil)
	case operationReject:
		if initial.Response != nil {
			return current.beginOperation(operationReject, initial.Response)
		}
		current.beginTextPrompt(promptReject)
		return current, nil
	case operationResume:
		if initial.Response != nil {
			return current.beginOperation(operationResume, initial.Response)
		}
		return current.promptForPendingAction()
	}
	return current, nil
}

// promptForPendingAction opens whichever answer the pause is waiting for. auth needs
// no answer — the operator logs in outside the dashboard and the run just resumes.
func (current model) promptForPendingAction() (tea.Model, tea.Cmd) {
	if current.status.PendingAction == nil {
		// Nothing is waiting for an answer, so there is nothing to prompt for — but
		// swallowing the request left `coterix resume <id>` on a done run sitting in a
		// blank dashboard that then exited 0, while the headless command exits 1.
		// Dispatching lets the controller return its own validation error, which the
		// feed shows and ui.Open returns (review T15 f1).
		return current.beginOperation(operationResume, nil)
	}
	if current.status.PendingAction.Kind == state.PendingAuth {
		return current.beginOperation(operationResume, nil)
	}
	if current.status.PendingAction.Kind == state.PendingTaskCap {
		current.collapseForPrompt()
		current.prompt = promptResume
		current.promptValue = taskCapChoices[0]
		current.promptError = ""
		return current, nil
	}
	current.beginTextPrompt(promptResume)
	return current, nil
}

// newOpenModel starts from an existing run instead of a new request (T15 W1).
func newOpenModel(
	ctx context.Context,
	cancel context.CancelFunc,
	controller controlPlane,
	repoRoot string,
	runID string,
	initial initialOperation,
	theme theme,
	autoQuit bool,
	tracker *operationTracker,
) model {
	current := newModel(
		ctx,
		cancel,
		controller,
		repoRoot,
		"",
		theme,
		autoQuit,
		tracker,
	)
	current.openRunID = runID
	current.pendingOperation = &initial
	// Nothing is running yet: Init loads the run first.
	current.operation = ""
	return current
}

func (current model) Init() tea.Cmd {
	if current.openRunID != "" {
		// Load the run first. controller.Status emits a state snapshot through the
		// observer, which is what seeds the model and its artifacts; the message this
		// command returns only exists to catch a *failed* load.
		return tea.Batch(current.loadRunCommand(), current.spinnerTickCommand())
	}
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
					"",
					"",
					fmt.Sprintf("artifact refresh: %v", message.err),
				)
			} else {
				current.artifacts = message.data
				current.artifactLoadedKey = message.key
				current.refreshArtifactRender()
			}
		}
		return current, nil
	case runLoadedMsg:
		if message.err != nil {
			// A run that cannot be loaded is a dead dashboard: without this the model
			// hangs with hasStatus=false and the interactive path exits 0, breaking
			// parity with the non-TTY exit 1 (T15-3R-1).
			current.appendSystemLog(
				runner.StreamStderr,
				logIconFail,
				"",
				"",
				message.err.Error(),
			)
			current.operationErr = message.err
			return current, tea.Quit
		}
		return current, nil
	case toastExpiredMsg:
		if !current.toastLive() {
			current.toast = ""
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
		if current.usesTextarea() {
			area, cmd := current.promptArea.Update(message)
			current.promptArea = area
			current.promptError = ""
			return current, cmd
		}
		// task_cap is a pick and the approve confirmation takes no input, so a paste
		// has nowhere to land (T14 W5).
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
			// approve re-emits the snapshot, so this path is reached too — the pending
			// operation must not be stranded by the artifact dedup (T15 W1).
			if seeded, command := current.seedPendingOperation(); command != nil {
				return seeded, command
			}
			return current, nil
		}
		current.artifactLoadingKey = current.artifactKey
		artifacts := loadArtifactsCommand(
			current.ctx,
			current.repoRoot,
			current.status,
			current.artifactKey,
		)
		if seeded, command := current.seedPendingOperation(); command != nil {
			return seeded, tea.Batch(artifacts, command)
		}
		return current, artifacts
	case pipeline.EventStepStarted:
		current.activeStep = event.Step
		current.activeRole = event.Role
		current.activeCLI = event.CLI
		current.stages.stepStarted(event.Step, current.now())
		current.resetActivity()
		current.appendSystemLog(
			runner.StreamStdout,
			logIconStart,
			event.Step,
			displayStep(event),
			event.CLI+" started",
		)
	case pipeline.EventStepLog:
		if event.Line != nil {
			// Subprocess lines feed the pinned tail only — never the
			// scrolling lifecycle history (plan T13 W2).
			//
			// Under `--output-format stream-json` most of what claude prints is
			// session bookkeeping: a one-word answer produced 10 lines of which 7 were
			// `system/*` or `rate_limit_event` (measured, T13a-2). The decoder drops
			// those and summarises the rest to one row; a non-JSON line passes through
			// untouched, so codex and plain-text mode are unaffected.
			// Only claude's stdout is a JSON stream. codex writes plain progress lines
			// (on stderr), and claude in plain-text mode does too — decoding those would
			// silently drop any line that happens to be a JSON object, which breaks the
			// "codex는 원문 진행 라인" contract (review T13a-2 f2).
			decoded := cli.StreamLine{Text: event.Line.Text}
			if isStreamJSONSource(event.CLI, event.Line.Stream) {
				candidate, ok := cli.DecodeStreamLine(event.Line.Text)
				if !ok {
					break
				}
				decoded = candidate
			}
			entry := logEntry{
				Step:    event.Step,
				Role:    event.Role,
				CLI:     event.CLI,
				Stream:  event.Line.Stream,
				Text:    decoded.Text,
				Attempt: event.Line.Attempt,
			}
			if decoded.Failed {
				// Severity comes from the payload's own is_error, not from which
				// stream carried it (T13 R5 · T13a-2).
				entry.Icon = logIconFail
			}
			current.appendActivity(entry)
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
			"",
			"",
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
	// The overlay is modal: while it is up it swallows everything except its own
	// close keys and a safe stop. Letting `a`/`r`/`enter` through opened a prompt
	// *behind* the overlay, after which `?` was consumed by the prompt and the first
	// `esc` only cancelled it — two surfaces at once, which the single-overlay
	// contract forbids (review T14c f4).
	if current.helpOpen {
		switch key.String() {
		case "ctrl+c", "q":
			return current.requestStop()
		case "?", "esc":
			current.helpOpen = false
		}
		return current, nil
	}

	switch key.String() {
	case "ctrl+c", "q":
		return current.requestStop()
	case "up", "k":
		// Scrolling drives the focused box only (T14 W2) — unless the focused box is
		// LIVE OUTPUT, where the same keys move the entry cursor (T14 W4).
		if current.selectionDrivesKeys() {
			if current.expandedCursor() {
				// Walk up through the expanded block before leaving it.
				current.expandScroll = min(current.expandScroll+1, maxExpandedBlockRows)
				current.syncCursorViewport()
				break
			}
			current.moveSelection(-1)
			break
		}
		for _, target := range current.scrollTargets() {
			current.boxScroll[target]++
		}
	case "down", "j":
		if current.selectionDrivesKeys() {
			if current.expandedCursor() && current.expandScroll > 0 {
				current.expandScroll--
				current.syncCursorViewport()
				break
			}
			current.moveSelection(1)
			break
		}
		for _, target := range current.scrollTargets() {
			if current.boxScroll[target] > 0 {
				current.boxScroll[target]--
			}
		}
	case "y":
		// Measured: bubbletea v2 ships SetClipboard (clipboard.go, OSC52) so there is
		// no need for a hand-rolled sequence or a new dependency (T13 W4).
		if current.hasStatus && current.status.RunID != "" {
			// OSC52 is fire-and-forget: tmux and some terminals drop it silently, so
			// the acknowledgement says the copy was *sent*, not that it landed (R10).
			current.showToast("⧉ run id copy sent")
			return current, tea.Batch(
				tea.SetClipboard(current.status.RunID),
				toastExpiryCommand(),
			)
		}
		return current, nil
	case "w":
		// Long tail lines are truncated by default; `w` wraps them instead (T13 W9).
		current.wrapTail = !current.wrapTail
	case "?":
		current.helpOpen = true
	case "esc":
		// Drop the cursor and follow again (T14 W4). The overlay is handled by the
		// modal branch above, so it cannot be open here.
		current.clearSelection()
	case "home":
		if current.selectionDrivesKeys() && current.hasSelection {
			// A cursor and a raw offset cannot both own the viewport: park the cursor
			// on the oldest entry instead of scrolling out from under it
			// (review T14c-r2 f1).
			current.moveSelectionTo(0)
			break
		}
		for _, target := range current.scrollTargets() {
			current.boxScroll[target] = maxScrollSentinel
		}
	case "end":
		// Back to the live edge, which also clears the `↓ new` indicator. With a
		// cursor up, following again means letting the cursor go.
		if current.selectionDrivesKeys() && current.hasSelection {
			current.clearSelection()
			break
		}
		for _, target := range current.scrollTargets() {
			current.boxScroll[target] = 0
		}
	case "1":
		current.selectArtifactTab(tabPlan)
	case "2":
		current.selectArtifactTab(tabDiff)
	case "3":
		current.selectArtifactTab(tabVerdict)
	case "]":
		current.selectArtifactTab((current.artifactTab + 1) % artifactTabCount)
	case "[":
		current.selectArtifactTab(
			(current.artifactTab + artifactTabCount - 1) % artifactTabCount,
		)
	case "tab":
		// compact has no focus concept (T14 W2) — the whole column scrolls as one.
		if isWide(current.width, current.height) {
			current.focus = shiftMainFocus(current, 1)
		}
	case "shift+tab":
		if isWide(current.width, current.height) {
			current.focus = shiftMainFocus(current, -1)
		}
	}

	// `enter` expands the selected entry, but only while LIVE OUTPUT is focused: it
	// is also the pending-action submit key, and moving focus away has to hand it
	// back to the run rather than keep it captured by a stale cursor (T14 W4).
	if key.String() == "enter" && current.selectionDrivesKeys() &&
		current.hasSelection {
		current.entryExpanded = !current.entryExpanded
		current.expandScroll = 0
		current.syncCursorViewport()
		if current.entryExpanded {
			// Expanding a step entry also opens the artifact that step produced, so
			// the evidence is one keypress away instead of a tab hunt (T14 W4).
			if tab, ok := evidenceTab(current.logs[current.selectedEntry].Step); ok {
				current.selectArtifactTab(tab)
			}
		}
		return current, nil
	}

	if !current.hasStatus || current.operation != "" {
		return current, nil
	}
	switch current.status.Phase {
	case state.PhaseAwaitingApproval:
		switch key.String() {
		case "a":
			// One confirmation step in front of approval (T14 W5). This gate is on
			// the *key* path only: T15's `coterix approve <id>` seeds the same
			// operation through ui.Open and stays exempt, matching the non-TTY CLI
			// (T15-R6 · design-plan W5).
			current.collapseForPrompt()
			current.prompt = promptApproveConfirm
			current.promptValue = ""
			current.promptError = ""
		case "r":
			current.beginTextPrompt(promptReject)
		}
	case state.PhasePausedForInput:
		if key.String() != "enter" || current.status.PendingAction == nil {
			return current, nil
		}
		if current.status.PendingAction.Kind == state.PendingAuth {
			return current.beginOperation(operationResume, nil)
		}
		if current.status.PendingAction.Kind == state.PendingTaskCap {
			// A pick, not free text — so no editor.
			current.collapseForPrompt()
			current.prompt = promptResume
			current.promptValue = taskCapChoices[0]
			current.promptError = ""
			return current, nil
		}
		current.beginTextPrompt(promptResume)
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
		// `enter` is taken here, ahead of the textarea, so it stays the submit key
		// (T14 W5 — newlines are ctrl+j).
		if current.prompt == promptApproveConfirm {
			current.clearPrompt()
			return current.beginOperation(operationApprove, nil)
		}
		response := strings.TrimSpace(current.promptResponse())
		if response == "" {
			current.promptError = "Response cannot be empty."
			return current, nil
		}
		if current.isTaskCapPrompt() &&
			response != taskCapChoices[0] && response != taskCapChoices[1] {
			current.promptError = "Task cap response must be retry or abort."
			return current, nil
		}
		mode := current.prompt
		current.clearPrompt()
		if mode == promptReject {
			return current.beginOperation(operationReject, &response)
		}
		return current.beginOperation(operationResume, &response)
	}

	if current.isTaskCapPrompt() {
		// Pick, don't type: the validator still receives "retry"/"abort" (T14 W5).
		switch key.String() {
		case "left", "h", "right", "l", "tab":
			current.promptValue = otherTaskCapChoice(current.promptValue)
			current.promptError = ""
		}
		return current, nil
	}
	if current.prompt == promptApproveConfirm {
		return current, nil
	}

	area, cmd := current.promptArea.Update(key)
	current.promptArea = area
	current.promptError = ""
	return current, cmd
}

// promptResponse is the text the open prompt would submit.
func (current model) promptResponse() string {
	if current.usesTextarea() {
		return current.promptArea.Value()
	}
	return current.promptValue
}

func otherTaskCapChoice(value string) string {
	if value == taskCapChoices[0] {
		return taskCapChoices[1]
	}
	return taskCapChoices[0]
}

func (current model) beginOperation(
	kind operationKind,
	response *string,
) (tea.Model, tea.Cmd) {
	wasWorking := current.isWorking()
	current.operation = kind
	current.operationErr = nil
	current.showToast(operationAcknowledgement(kind))
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
		return current, tea.Batch(operation, toastExpiryCommand())
	}
	return current, tea.Batch(
		operation,
		current.spinnerTickCommand(),
		toastExpiryCommand(),
	)
}

// showToast records an acknowledgement and when it stops being shown.
func (current *model) showToast(text string) {
	if text == "" {
		return
	}
	current.toast = text
	current.toastUntil = current.nowOrZero().Add(toastDuration)
}

// toastLive reports whether the acknowledgement is still within its window. A zero
// clock (tests that never inject one) means it stays until the next one replaces it.
func (current model) toastLive() bool {
	if current.toast == "" {
		return false
	}
	if current.toastUntil.IsZero() {
		return true
	}
	return current.nowOrZero().Before(current.toastUntil)
}

func (current model) nowOrZero() time.Time {
	if current.now == nil {
		return time.Time{}
	}
	return current.now()
}

// toastExpiryCommand wakes the view up when the acknowledgement should disappear.
// The decision is still the clock's — this only schedules a redraw.
func toastExpiryCommand() tea.Cmd {
	return tea.Tick(toastDuration, func(time.Time) tea.Msg {
		return toastExpiredMsg{}
	})
}

func operationAcknowledgement(kind operationKind) string {
	switch kind {
	case operationApprove:
		return "✓ approve sent"
	case operationReject:
		return "✓ reject sent"
	case operationResume:
		return "✓ response sent"
	}
	return ""
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
	current.promptArea = textarea.Model{}
}

// beginTextPrompt opens the multiline editor for a free-text response (T14 W5).
//
// Measured: the textarea's default keymap binds InsertNewline to `enter`, which is
// the submit key here. Two independent pieces make R8's contract hold, and each was
// mutation-checked separately: binding InsertNewline to `ctrl+j` is what makes
// ctrl+j break a line, and taking `enter` in updatePromptKey before the textarea
// sees it is what makes enter submit. (Whether `enter` also stays in the binding
// turns out not to matter — the interception runs first.)
// collapseForPrompt folds the cursor's expanded block when a prompt takes the
// keyboard. While `prompt != promptNone` every key goes to the editor, so there is
// no way to walk the block — and the prompt shrinks LIVE OUTPUT at the same time, so
// the block would sit clipped with its head unreachable (review T14c-r3 f2). The
// cursor itself survives: only the reading mode ends.
func (current *model) collapseForPrompt() {
	current.entryExpanded = false
	current.expandScroll = 0
	current.syncCursorViewport()
}

func (current *model) beginTextPrompt(mode promptMode) {
	area := textarea.New()
	area.ShowLineNumbers = false
	area.Prompt = ""
	area.SetHeight(promptAreaRows)
	area.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("ctrl+j", "alt+enter"),
		key.WithHelp("ctrl+j", "newline"),
	)
	area.Focus()
	current.collapseForPrompt()
	current.prompt = mode
	current.promptValue = ""
	current.promptError = ""
	current.promptArea = area
}

// usesTextarea reports whether the open prompt is the free-text editor. task_cap is
// a left/right pick and the approve confirmation takes no input, so both keep the
// single-row status bar (T14 W5).
func (current model) usesTextarea() bool {
	switch current.prompt {
	case promptReject:
		return true
	case promptResume:
		return !current.isTaskCapPrompt()
	}
	return false
}

func (current model) isTaskCapPrompt() bool {
	return current.prompt == promptResume &&
		current.status.PendingAction != nil &&
		current.status.PendingAction.Kind == state.PendingTaskCap
}

// promptRegionRows is the status bar's height while a prompt is open.
func promptRegionRows(current model) int {
	if current.usesTextarea() {
		return promptRowsArea
	}
	return promptRowsSingle
}

// selectArtifactTab switches which artifact the FEED box shows. The offset is
// reset because it measures a distance into a *different* document now — carrying
// it over would park the reader at an arbitrary place in the new one (T14 W3).
// An empty tab is still selectable: it renders its own "nothing here yet" body.
func (current *model) selectArtifactTab(tab artifactTab) {
	if tab == current.artifactTab {
		return
	}
	current.artifactTab = tab
	current.boxScroll[boxFeed] = 0
	current.refreshArtifactRender()
}

func (current *model) refreshArtifactRender() {
	width := dashboardMainInnerWidth(current.width, current.height)
	previous := current.artifactRender
	current.artifactRenderWidth = width
	current.artifactRender = ""
	current.artifactRenderErr = nil
	artifacts := markdownArtifacts(current.artifacts, current.artifactTab)
	if len(artifacts) > 0 {
		current.artifactRender, current.artifactRenderErr = renderMarkdown(
			current.theme,
			width,
			artifacts,
		)
	}
	current.reanchorFeed(previous, current.artifactRender)
}

// reanchorFeed keeps a paused FEED on the lines it was showing when the artifact
// render is replaced wholesale. The body is rendered markdown with no per-line
// identity, so the only sound test is whether the new render *extends* the old
// one: an appended artifact keeps the reader in place, while anything else (a
// re-wrap at a new width, an edited artifact) returns the box to the live edge
// rather than leaving the old offset pointing at an arbitrary new position
// (review T14a-r2 f1).
func (current *model) reanchorFeed(previous, next string) {
	if current.boxScroll[boxFeed] == 0 {
		return
	}
	if previous == "" || next == "" || !strings.HasPrefix(next, previous) {
		current.boxScroll[boxFeed] = 0
		return
	}
	current.preserveReading(boxFeed, countRows(next)-countRows(previous))
}

func (current *model) appendLog(entry logEntry) {
	if entry.At.IsZero() && current.now != nil {
		entry.At = current.now()
	}
	current.logs = append(current.logs, entry)
	current.preserveReading(boxLiveOutput, 1)
	if overflow := len(current.logs) - maxLogLines; overflow > 0 {
		copy(current.logs, current.logs[overflow:])
		current.logs = current.logs[:maxLogLines]
		// Eviction renumbers the entries, so the cursor has to follow its own row
		// down — or let go when that row is the one being dropped (T14 W4).
		if current.hasSelection {
			if current.selectedEntry < overflow {
				current.clearSelection()
			} else {
				current.selectedEntry -= overflow
			}
		}
	}
	// A cursor on the newest entry sits at offset 0, which preserveReading reads as
	// "following the live edge" and leaves alone — so the viewport followed the new
	// line while the cursor stayed behind, and the marker drifted off the window
	// (review T14c f2). Deriving the offset from the cursor makes offset 0 an anchor
	// like any other.
	current.syncCursorViewport()
}

// syncCursorViewport puts the cursor's entry back on the window's last row. The
// offset counts the rows below it, and every entry below the cursor is one row, so
// expanding the cursor's own entry does not change it.
func (current *model) syncCursorViewport() {
	if !current.hasSelection || len(current.logs) == 0 {
		return
	}
	current.selectedEntry = min(max(current.selectedEntry, 0), len(current.logs)-1)
	if !current.entryExpanded {
		current.expandScroll = 0
	}
	// Rows below the cursor plus however far up its own block has been walked.
	// visibleLines clamps an offset that runs past the top, so the cap only has to
	// bound the walk, not match the box height.
	current.boxScroll[boxLiveOutput] =
		len(current.logs) - 1 - current.selectedEntry + current.expandScroll
}

// expandedCursor reports whether the keys should walk inside the cursor's own block
// rather than move between entries.
func (current model) expandedCursor() bool {
	return current.hasSelection && current.entryExpanded
}

// moveSelectionTo parks the cursor on a specific entry.
func (current *model) moveSelectionTo(index int) {
	if len(current.logs) == 0 {
		return
	}
	current.selectedEntry = min(max(index, 0), len(current.logs)-1)
	current.hasSelection = true
	current.entryExpanded = false
	current.expandScroll = 0
	current.syncCursorViewport()
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
	current.preserveReading(boxActivity, 1)
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
	// A fresh attempt starts a fresh viewport — a stale offset would point past
	// the (now empty) buffer.
	current.boxScroll[boxActivity] = 0
}

// isStreamJSONSource reports whether a line can be a stream-json event. Only claude
// emits them, and only on stdout (T13 W1 condition).
func isStreamJSONSource(cliName string, stream runner.Stream) bool {
	return strings.EqualFold(strings.TrimSpace(cliName), "claude") &&
		stream == runner.StreamStdout
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

// focusCycle is the tab order. It is deliberately *not* mainBoxOrder: the
// contract fixes FEED → LIVE OUTPUT → ACTIVITY → PENDING → sidebar (design-plan
// v3 W2 line 76) while PENDING *renders* first because the run is blocked on it.
// Reusing the render order put PENDING ahead of FEED, so `tab` from ACTIVITY
// jumped to the sidebar (review T14a-r2 f2). Only on-screen boxes take part.
func focusCycle(current model) []mainBox {
	cycle := []mainBox{boxFeed, boxLiveOutput, boxActivity}
	if hasPendingPrompt(current) {
		cycle = append(cycle, boxPending)
	}
	return append(cycle, boxSidebar)
}

// normalizedFocus keeps focus on something visible. PENDING disappears when the
// run resumes, and a box can be dropped for space — leaving focus on it would
// make j/k drive an off-screen offset and no box would draw a focused border
// (review T14a f3).
func (current model) normalizedFocus() mainBox {
	cycle := focusCycle(current)
	for _, box := range cycle {
		if box == current.focus {
			return box
		}
	}
	return cycle[0]
}

// scrollTargets are the boxes j/k drives. Wide layouts move the focused box
// alone (T14 W2). Compact has no focus concept, so one gesture moves *every*
// section of the single column: driving FEED alone left the rows that a squeezed
// LIVE OUTPUT or ACTIVITY hid unreachable, which is a regression from the T13
// single feed (review T14a-r2 f2). Each box keeps its own offset so a section
// that grows still preserves its own reading position.
func (current model) scrollTargets() []mainBox {
	if !isWide(current.width, current.height) {
		return mainBoxOrder(current)
	}
	return []mainBox{current.normalizedFocus()}
}

// preserveReading keeps a paused box on the same absolute lines when new content
// arrives. The offset is measured from the newest line, so it has to grow by the
// number of rows added; leaving it alone made the window slide one row per line
// and the user lost their place mid-read (T14 W1 · review T14a f2). A box that is
// following the live edge (offset 0) is untouched.
func (current *model) preserveReading(box mainBox, added int) {
	if added > 0 && current.boxScroll[box] > 0 {
		current.boxScroll[box] += added
	}
}

// selectionDrivesKeys reports whether `j/k` move the lifecycle cursor instead of a
// raw offset. Only LIVE OUTPUT gets the cursor: FEED holds a rendered document and
// ACTIVITY is a live tail, neither of which has entries to select. compact has no
// focus at all, so it keeps the single-scroll contract (T14 W2/W4 — this is the
// `j/k` reconciliation design-plan v2 deferred to D3).
func (current model) selectionDrivesKeys() bool {
	return isWide(current.width, current.height) &&
		current.normalizedFocus() == boxLiveOutput &&
		len(current.logs) > 0
}

// moveSelection walks the cursor and drags the viewport with it: selection *is*
// the scroll (T14 W4). The cursor parks on the window's last row, so the rows
// above it are the entry's history — the same shape as scrolling back, plus a
// marker. The first press only reveals the cursor on the newest entry.
func (current *model) moveSelection(delta int) {
	if len(current.logs) == 0 {
		return
	}
	next := len(current.logs) - 1
	if current.hasSelection {
		next = current.selectedEntry + delta
	}
	next = min(max(next, 0), len(current.logs)-1)
	if current.hasSelection && next != current.selectedEntry {
		// Walking off an entry collapses it; `enter` re-expands.
		current.entryExpanded = false
	}
	current.selectedEntry = next
	current.hasSelection = true
	current.syncCursorViewport()
}

// evidenceTab maps a pipeline step to the artifact it produces (T14 W4). Steps with
// no artifact of their own — gate, fix — report false and leave the tab alone
// rather than yanking the reader to an unrelated document.
func evidenceTab(step string) (artifactTab, bool) {
	switch step {
	case pipeline.StepPlan:
		return tabPlan, true
	case pipeline.StepImplementation:
		return tabDiff, true
	case pipeline.StepPlanReview, pipeline.StepImplementationReview:
		return tabVerdict, true
	}
	return tabPlan, false
}

func (current *model) clearSelection() {
	current.hasSelection = false
	current.selectedEntry = 0
	current.entryExpanded = false
	current.expandScroll = 0
	current.boxScroll[boxLiveOutput] = 0
}

// shiftMainFocus cycles focus through the boxes that are actually on screen.
func shiftMainFocus(current model, delta int) mainBox {
	cycle := focusCycle(current)
	from := current.normalizedFocus()
	for index, box := range cycle {
		if box == from {
			return cycle[(index+delta+len(cycle))%len(cycle)]
		}
	}
	return cycle[0]
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

// role names whose step the entry is about. It used to be the constant "control",
// which made the meta column repeat `control ·` on every row while the text
// carried the real role — the same information twice (T14 W11).
// appendSystemLog records one harness lifecycle entry. `step` is the *pipeline*
// step the event belongs to, kept separate from the display role: the entry's Step
// is what `enter` uses to open that step's artifact, and hardcoding it to "coterix"
// silently disabled the whole W4 evidence link (review T14c f1).
func (current *model) appendSystemLog(
	stream runner.Stream,
	icon logIcon,
	step string,
	role string,
	text string,
) {
	if role == "" {
		role = "coterix"
	}
	current.appendLog(logEntry{
		Step:   step,
		Role:   role,
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
		"attempt %d exited %d",
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
	current.appendSystemLog(stream, icon, event.Step, displayStep(event), message)
}

func (current *model) appendStepFinished(event pipeline.Event) {
	message := "finished"
	stream := runner.StreamStdout
	icon := logIconDone
	if event.Err != nil {
		message += " · " + event.Err.Error()
		stream = runner.StreamStderr
		icon = logIconFail
	}
	current.appendSystemLog(stream, icon, event.Step, displayStep(event), message)
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
