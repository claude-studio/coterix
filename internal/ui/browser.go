package ui

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

// browseAction is what a keypress in the browser asks for. The browser itself is
// read-only: an action is a *request to leave it* and enter the live dashboard, which
// is where every mutation happens (T16 W1 — actions launch T15).
type browseAction struct {
	RunID string
	// Command is the CLI command name the dashboard should open with, or "" when the
	// operator just quit.
	Command string
}

// Browse shows an existing run — or the list of them — and returns the action the
// operator chose (T16 W1/W2).
//
// The controller passed in must be **observer-less**. `controller.Status` calls
// observeRun for every run it reads, so an observing controller would push N state
// snapshots into a model built for one, overwriting the status N times and firing the
// artifact chain N times (control.go, model.go). Read-only browsing needs none of
// that: it takes the values and renders them.
func Browse(
	ctx context.Context,
	controller browseControl,
	repoRoot string,
	runID string,
	options RunOptions,
) (pipeline.RunStatus, string, error) {
	if controller == nil {
		return pipeline.RunStatus{}, "", fmt.Errorf(
			"ui: a control plane is required",
		)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Output == nil {
		options.Output = os.Stdout
	}
	if options.Input == nil && options.Interactive {
		options.Input = os.Stdin
	}
	if options.Width <= 0 {
		options.Width = wideBreakpointWidth
	}
	if options.Height <= 0 {
		options.Height = wideBreakpointHeight
	}

	statuses, err := controller.Status(ctx, repoRoot, runID)
	if err != nil {
		return pipeline.RunStatus{}, "", err
	}
	currentTheme, err := loadTheme()
	if err != nil {
		return pipeline.RunStatus{}, "", err
	}

	initial := browserModel{
		theme:    currentTheme,
		statuses: statuses,
		single:   runID != "",
		width:    options.Width,
		height:   options.Height,
	}
	if initial.single && len(statuses) != 1 {
		return pipeline.RunStatus{}, "", fmt.Errorf(
			"ui: browsing one run requires exactly one status, got %d",
			len(statuses),
		)
	}

	// Without a keyboard there is nothing to browse: the program would wait forever
	// for a key that cannot arrive. main.go routes non-TTY `status` to the snapshot
	// JSON instead, so this is a guard for direct callers rather than a live path.
	if !options.Interactive {
		return initial.chosenStatus(), "", nil
	}

	finalModel, runErr := tea.NewProgram(
		initial,
		tea.WithContext(ctx),
		tea.WithOutput(options.Output),
		tea.WithInput(options.Input),
		tea.WithWindowSize(options.Width, options.Height),
	).Run()
	if runErr != nil {
		return pipeline.RunStatus{}, "", runErr
	}
	final, ok := finalModel.(browserModel)
	if !ok {
		return pipeline.RunStatus{}, "", fmt.Errorf(
			"ui: Bubble Tea returned unexpected model %T",
			finalModel,
		)
	}
	return final.chosenStatus(), final.action.Command, nil
}

// browseControl is the read-only slice of the control plane the browser needs.
type browseControl interface {
	Status(
		ctx context.Context,
		repoRoot string,
		runID string,
	) ([]pipeline.RunStatus, error)
}

type browserModel struct {
	theme    theme
	statuses []pipeline.RunStatus
	// single is true for `status <run_id>`: the picker is skipped and the detail is
	// shown directly, so `esc` quits instead of going back to a list.
	single   bool
	selected int
	action   browseAction
	// detail is true while a run's detail is on screen. In single mode it starts true
	// and never goes back.
	detail bool
	width  int
	height int
	notice string
}

func (current browserModel) Init() tea.Cmd {
	return nil
}

func (current browserModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		current.width = max(1, message.Width)
		current.height = max(1, message.Height)
		return current, nil
	case tea.KeyPressMsg:
		return current.updateKey(message)
	}
	return current, nil
}

func (current browserModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	current.notice = ""
	switch key.String() {
	case "ctrl+c", "q":
		return current, tea.Quit
	case "esc":
		if current.showingDetail() && !current.single {
			// Back to the list rather than out of the tool.
			current.detail = false
			return current, nil
		}
		return current, tea.Quit
	case "up", "k":
		if !current.showingDetail() {
			current.selected = max(0, current.selected-1)
		}
		return current, nil
	case "down", "j":
		if !current.showingDetail() {
			current.selected = min(len(current.statuses)-1, current.selected+1)
		}
		return current, nil
	case "enter":
		if !current.showingDetail() && len(current.statuses) > 0 {
			current.detail = true
		}
		return current, nil
	case "a":
		return current.requestAction("approve", state.PhaseAwaitingApproval)
	case "r":
		return current.requestAction("reject", state.PhaseAwaitingApproval)
	case "e":
		return current.requestAction("resume", state.PhasePausedForInput)
	}
	return current, nil
}

// requestAction leaves the browser so the live dashboard can take over. The browser
// mutates nothing itself; `phase` is the only phase the action is legal in, and a
// keypress in any other phase says so instead of being silently dropped (T16 W1).
func (current browserModel) requestAction(
	command string,
	phase state.Phase,
) (tea.Model, tea.Cmd) {
	if !current.showingDetail() {
		return current, nil
	}
	status := current.chosenStatus()
	if status.RunID == "" {
		return current, nil
	}
	if status.Phase != phase {
		current.notice = fmt.Sprintf(
			"%s needs phase %s — this run is %s",
			command,
			phase,
			status.Phase,
		)
		return current, nil
	}
	current.action = browseAction{RunID: status.RunID, Command: command}
	return current, tea.Quit
}

// showingDetail reports whether the detail view owns the keys. `status <run_id>` has
// no list to return to, so it is always in detail.
func (current browserModel) showingDetail() bool {
	return current.single || current.detail
}

func (current browserModel) chosenStatus() pipeline.RunStatus {
	if len(current.statuses) == 0 {
		return pipeline.RunStatus{}
	}
	index := min(max(current.selected, 0), len(current.statuses)-1)
	return current.statuses[index]
}

func (current browserModel) View() tea.View {
	view := tea.NewView(renderBrowser(current))
	view.WindowTitle = "Coterix"
	return view
}

func renderBrowser(current browserModel) string {
	width := snapshotWidth(current.width)
	rows := []string{renderSnapshotBanner(current.theme, width)}

	switch {
	case len(current.statuses) == 0:
		rows = append(rows, current.theme.styles.Muted.Render("no runs"))
	case current.showingDetail():
		rows = append(rows, renderSnapshotDetail(
			current.theme,
			current.chosenStatus(),
			width,
		))
	default:
		rows = append(rows, renderBrowserList(current, width))
	}

	if current.notice != "" {
		rows = append(rows, current.theme.styles.PhaseWarning.Render(
			ansi.TruncateWc(current.notice, width, "…"),
		))
	}
	rows = append(rows, current.theme.styles.Hint.Render(
		ansi.TruncateWc(browserHints(current), width, "…"),
	))
	return constrainSnapshotWidth(strings.Join(rows, "\n"), width)
}

// renderBrowserList draws one row per run with the cursor gutter attached **in the
// same loop as the data**, so the marker cannot drift off its run.
//
// Reusing `renderSnapshotTable` and adding gutters afterwards was wrong: the table
// carries a top border, a header and a separator, so counting non-blank lines put the
// marker two rows above the selected run — what was marked and what `enter` opened
// disagreed (review T16 f1). The picker therefore formats its own rows; the one-shot
// table stays exactly as it was for `RenderSnapshot`.
func renderBrowserList(current browserModel, width int) string {
	const gutterWidth = 2
	columns := []string{"run_id", "phase", "task", "confirmed", "signal"}
	cells := make([][]string, 0, len(current.statuses))
	for _, status := range current.statuses {
		data := deriveStatusFields(status)
		cells = append(cells, []string{
			snapshotCell(data.RunID),
			snapshotCell(string(data.Phase)),
			snapshotCell(data.TaskID),
			fmt.Sprintf("%d/%d", data.Confirmed, data.Total),
			snapshotSignal(data),
		})
	}

	widths := make([]int, len(columns))
	for index, header := range columns {
		widths[index] = ansi.StringWidth(header)
	}
	for _, row := range cells {
		for index, cell := range row {
			widths[index] = max(widths[index], ansi.StringWidth(cell))
		}
	}

	pad := func(text string, size int) string {
		gap := size - ansi.StringWidth(text)
		if gap <= 0 {
			return text
		}
		return text + strings.Repeat(" ", gap)
	}
	joinRow := func(row []string) string {
		parts := make([]string, 0, len(row))
		for index, cell := range row {
			parts = append(parts, pad(cell, widths[index]))
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}

	body := max(1, width-gutterWidth)
	lines := []string{
		strings.Repeat(" ", gutterWidth) + current.theme.styles.SectionTitle.Render(
			ansi.TruncateWc(joinRow(columns), body, "…"),
		),
	}
	for index, row := range cells {
		gutter := strings.Repeat(" ", gutterWidth)
		if index == current.selected {
			gutter = current.theme.styles.BorderFocus.Render("▌") +
				current.theme.styles.TabActive.Render("▸")
		}
		lines = append(lines, gutter+current.theme.styles.Value.Render(
			ansi.TruncateWc(joinRow(row), body, "…"),
		))
	}
	return strings.Join(lines, "\n")
}

func browserHints(current browserModel) string {
	if len(current.statuses) == 0 {
		return "q quit"
	}
	if !current.showingDetail() {
		return "j/k move · enter open · q quit"
	}
	hints := make([]string, 0, 4)
	switch current.chosenStatus().Phase {
	case state.PhaseAwaitingApproval:
		hints = append(hints, "a approve", "r reject")
	case state.PhasePausedForInput:
		hints = append(hints, "e respond")
	}
	if current.single {
		return strings.Join(append(hints, "q quit"), " · ")
	}
	return strings.Join(append(hints, "esc back", "q quit"), " · ")
}
