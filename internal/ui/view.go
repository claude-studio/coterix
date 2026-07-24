package ui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const sidebarWidth = 32

// THESIS: Make a synchronous multi-agent pipeline readable as one live
// instrument panel, without turning it into chat or session chrome.
// OWN-WORLD: COTERIX ink surfaces, token-driven Claude→Codex accents, compact
// evidence marks, and a single right-hand telemetry rail.
// STORY: Follow agent output, verify hard/soft gates, then answer the one human
// action that is actually blocking progress.
// FIRST VIEWPORT: Wide terminals show feed + 32-cell rail; compact terminals
// fold that rail into a two-line header above the same feed and status bar.
// FORM: Operate-mode dashboard, fixed by docs/spec/ui.md.

type sidebarData struct {
	RunID            string
	Phase            state.Phase
	Role             string
	CLI              string
	TaskID           string
	TaskStatus       state.TaskStatus
	Attempt          int
	Gate             evidenceOutcome
	Review           evidenceOutcome
	PlanRound        int
	Confirmed        int
	Total            int
	AwaitingApproval bool
	PendingKind      state.PendingKind
	PendingPrompt    string
	LastError        string
}

func renderDashboard(current model) string {
	width := max(1, current.width)
	height := max(1, current.height)
	statusHeight := 2
	if current.prompt != promptNone {
		statusHeight = min(4, height)
	}

	if isWide(width, height) {
		mainWidth := max(1, width-sidebarWidth)
		contentHeight := max(1, height-statusHeight)
		main := renderMain(current, mainWidth, contentHeight)
		sidebar := renderSidebar(
			current,
			sidebarWidth,
			contentHeight,
		)
		statusBar := renderStatusBar(current, width, statusHeight)
		return composeUV(width, height, []uvRegion{
			{
				X:       0,
				Y:       0,
				Width:   mainWidth,
				Height:  contentHeight,
				Content: main,
			},
			{
				X:       mainWidth,
				Y:       0,
				Width:   sidebarWidth,
				Height:  contentHeight,
				Content: sidebar,
			},
			{
				X:       0,
				Y:       contentHeight,
				Width:   width,
				Height:  statusHeight,
				Content: statusBar,
			},
		})
	}

	headerHeight := min(2, height)
	contentHeight := max(1, height-headerHeight-statusHeight)
	header := renderCompactHeader(current, width, headerHeight)
	main := renderMain(current, width, contentHeight)
	statusBar := renderStatusBar(current, width, statusHeight)
	return composeUV(width, height, []uvRegion{
		{
			X:       0,
			Y:       0,
			Width:   width,
			Height:  headerHeight,
			Content: header,
		},
		{
			X:       0,
			Y:       headerHeight,
			Width:   width,
			Height:  contentHeight,
			Content: main,
		},
		{
			X:       0,
			Y:       headerHeight + contentHeight,
			Width:   width,
			Height:  statusHeight,
			Content: statusBar,
		},
	})
}

func isWide(width, height int) bool {
	return width >= wideBreakpointWidth && height >= wideBreakpointHeight
}

func dashboardMainInnerWidth(width, height int) int {
	paneWidth := max(1, width)
	if isWide(width, height) {
		paneWidth = max(1, width-sidebarWidth)
	}
	return max(1, paneWidth-4)
}

func renderMain(current model, width, height int) string {
	innerWidth := max(1, width-4)
	innerHeight := max(1, height-2)
	var content strings.Builder
	content.WriteString(
		current.theme.styles.SectionTitle.Render("╱╱╱ PIPELINE FEED"),
	)
	content.WriteString("\n")

	if current.artifactRenderErr != nil {
		content.WriteString(
			current.theme.styles.Error.Render(
				"× " + current.artifactRenderErr.Error(),
			),
		)
		content.WriteString("\n")
	} else if current.artifactRender != "" {
		content.WriteString(current.artifactRender)
	}

	content.WriteString(
		current.theme.styles.SectionTitle.Render("╱╱╱ LIVE OUTPUT"),
	)
	content.WriteString("\n")
	if len(current.logs) == 0 {
		content.WriteString(
			current.theme.styles.Muted.Render(
				"⋯ Waiting for the first subprocess line",
			),
		)
		content.WriteString("\n")
	} else {
		for _, line := range current.logs {
			content.WriteString(renderLogLine(current.theme, line, innerWidth))
			content.WriteString("\n")
		}
	}

	visible := visibleLines(content.String(), innerHeight, current.scroll)
	return current.theme.styles.Main.
		Width(innerWidth).
		Height(innerHeight).
		Padding(1, 2).
		Render(visible)
}

func markdownArtifacts(data artifactData) []markdownArtifact {
	artifacts := make([]markdownArtifact, 0, 2+len(data.Verdicts))
	if strings.TrimSpace(data.PlanMarkdown) != "" {
		artifacts = append(artifacts, markdownArtifact{
			Title:   "Plan",
			Content: data.PlanMarkdown,
		})
	}
	if data.DiffContent != nil && strings.TrimSpace(*data.DiffContent) != "" {
		artifacts = append(artifacts, markdownArtifact{
			Title:    "Current diff",
			Content:  *data.DiffContent,
			Language: "diff",
		})
	}
	for _, verdict := range data.Verdicts {
		artifacts = append(artifacts, markdownArtifact{
			Title:    "Verdict · " + verdict.Name,
			Content:  verdict.JSON,
			Language: "json",
		})
	}
	return artifacts
}

func renderLogLine(currentTheme theme, line logEntry, width int) string {
	label := line.Role
	if label == "" {
		label = line.Step
	}
	if label == "" {
		label = "process"
	}
	if line.CLI != "" {
		label += "→" + line.CLI
	}
	if line.Attempt > 0 {
		label += "#" + strconv.Itoa(line.Attempt)
	}
	prefix := currentTheme.styles.Muted.Render("[" + label + "]")
	text := remapANSI16(line.Text, currentTheme.tokens.ANSI)
	if line.Stream == runner.StreamStderr {
		text = currentTheme.styles.Error.Render(text)
	} else {
		text = currentTheme.styles.Value.Render(text)
	}
	return ansi.TruncateWc(prefix+" "+text, max(1, width), "…")
}

func renderSidebar(current model, width, height int) string {
	data := deriveSidebar(current)
	innerWidth := max(1, width-5)
	var content strings.Builder
	content.WriteString(gradientText(current.theme, "COTERIX"))
	content.WriteString("\n")
	if current.operation != "" || current.activeStep != "" {
		content.WriteString(current.spinner.View())
		content.WriteString(gradientText(current.theme, " WORKING"))
	} else {
		content.WriteString(current.theme.styles.Muted.Render("● CONTROL PLANE"))
	}
	content.WriteString("\n\n")
	content.WriteString(current.theme.styles.SectionTitle.Render("RUN"))
	content.WriteString("\n")
	writeSidebarField(
		&content,
		current.theme,
		"run",
		ansi.TruncateWc(data.RunID, innerWidth, "…"),
	)
	writeSidebarField(
		&content,
		current.theme,
		"phase",
		phaseValue(current.theme, data.Phase),
	)
	content.WriteString("\n")
	content.WriteString(current.theme.styles.SectionTitle.Render("ROUTING"))
	content.WriteString("\n")
	writeSidebarField(
		&content,
		current.theme,
		"role",
		ansi.TruncateWc(data.Role, innerWidth, "…"),
	)
	writeSidebarField(
		&content,
		current.theme,
		"cli",
		ansi.TruncateWc(data.CLI, innerWidth, "…"),
	)
	content.WriteString("\n")
	content.WriteString(current.theme.styles.SectionTitle.Render("TASK"))
	content.WriteString("\n")
	writeSidebarField(&content, current.theme, "current", data.TaskID)
	writeSidebarField(
		&content,
		current.theme,
		"status",
		string(data.TaskStatus),
	)
	writeSidebarField(
		&content,
		current.theme,
		"attempt",
		strconv.Itoa(data.Attempt),
	)
	writeSidebarField(
		&content,
		current.theme,
		"gate / review",
		outcomeIcon(current.theme, data.Gate)+"  "+
			outcomeIcon(current.theme, data.Review),
	)
	content.WriteString("\n")
	content.WriteString(current.theme.styles.SectionTitle.Render("PROGRESS"))
	content.WriteString("\n")
	writeSidebarField(
		&content,
		current.theme,
		"plan_round",
		strconv.Itoa(data.PlanRound),
	)
	writeSidebarField(
		&content,
		current.theme,
		"confirmed",
		fmt.Sprintf("%d/%d", data.Confirmed, data.Total),
	)

	if data.AwaitingApproval {
		content.WriteString("\n")
		content.WriteString(
			current.theme.styles.Pending.Render("! APPROVAL GATE"),
		)
		content.WriteString("\n")
		content.WriteString(
			current.theme.styles.Warning.Render("a approve · r reject"),
		)
		content.WriteString("\n")
	} else if data.PendingKind != "" {
		content.WriteString("\n")
		content.WriteString(
			current.theme.styles.Pending.Render(
				"! PENDING · " + string(data.PendingKind),
			),
		)
		content.WriteString("\n")
		content.WriteString(
			current.theme.styles.Warning.Render(
				ansi.HardwrapWc(data.PendingPrompt, innerWidth, false),
			),
		)
		content.WriteString("\n")
	}
	if data.LastError != "" {
		content.WriteString("\n")
		content.WriteString(
			current.theme.styles.Error.Render(
				"× " + ansi.HardwrapWc(data.LastError, innerWidth, false),
			),
		)
		content.WriteString("\n")
	}

	return current.theme.styles.Sidebar.
		Width(max(1, width-3)).
		Height(max(1, height-2)).
		Padding(1).
		Render(content.String())
}

func deriveSidebar(current model) sidebarData {
	data := sidebarData{
		RunID:       "starting…",
		Phase:       state.PhasePlanning,
		Role:        "—",
		CLI:         "—",
		TaskID:      "—",
		TaskStatus:  state.TaskOpen,
		Gate:        current.artifacts.GateOutcome,
		Review:      current.artifacts.ReviewOutcome,
		PendingKind: "",
	}
	if current.activeRole != "" || current.activeStep != "" {
		data.Role = current.activeRole
		if data.Role == "" {
			data.Role = current.activeStep
		}
		data.CLI = current.activeCLI
	}
	if !current.hasStatus {
		return data
	}

	status := current.status
	data.RunID = status.RunID
	data.Phase = status.Phase
	data.PlanRound = status.PlanRound
	data.Total = len(status.TaskOrder)
	for _, taskID := range status.TaskOrder {
		if task, exists := status.Tasks[taskID]; exists &&
			task.Status == state.TaskConfirmed {
			data.Confirmed++
		}
	}
	if status.CurrentTaskID != nil {
		data.TaskID = *status.CurrentTaskID
		if task, exists := status.Tasks[*status.CurrentTaskID]; exists {
			data.TaskStatus = task.Status
			data.Attempt = task.Attempt
			if task.Status == state.TaskConfirmed {
				data.Gate = evidencePass
				data.Review = evidencePass
			}
		}
	}
	data.AwaitingApproval = status.Phase == state.PhaseAwaitingApproval
	if status.PendingAction != nil {
		data.PendingKind = status.PendingAction.Kind
		data.PendingPrompt = status.PendingAction.Prompt
	}
	if status.LastError != nil {
		data.LastError = *status.LastError
	}
	if current.activeStep == "" {
		switch {
		case data.AwaitingApproval:
			data.Role = "human_gate"
			data.CLI = "operator"
		case data.PendingKind != "":
			data.Role = "pending_action"
			data.CLI = "operator"
		case status.Phase == state.PhaseDone || status.Phase == state.PhaseFailed:
			data.Role = "—"
			data.CLI = "—"
		}
	}
	return data
}

func writeSidebarField(
	builder *strings.Builder,
	currentTheme theme,
	label string,
	value string,
) {
	if value == "" {
		value = "—"
	}
	builder.WriteString(currentTheme.styles.Label.Render(label + ": "))
	builder.WriteString(currentTheme.styles.Value.Render(value))
	builder.WriteString("\n")
}

func phaseValue(currentTheme theme, phase state.Phase) string {
	switch phase {
	case state.PhaseDone:
		return currentTheme.styles.Success.Render("✓ " + string(phase))
	case state.PhaseFailed:
		return currentTheme.styles.Error.Render("× " + string(phase))
	case state.PhaseAwaitingApproval, state.PhasePausedForInput:
		return currentTheme.styles.Warning.Render("! " + string(phase))
	default:
		return currentTheme.styles.Busy.Render("● " + string(phase))
	}
}

func outcomeIcon(currentTheme theme, outcome evidenceOutcome) string {
	switch outcome {
	case evidencePass:
		return currentTheme.styles.Success.Render("✓")
	case evidenceFail:
		return currentTheme.styles.Error.Render("×")
	default:
		return currentTheme.styles.Busy.Render("⋯")
	}
}

func renderCompactHeader(current model, width, height int) string {
	data := deriveSidebar(current)
	task := data.TaskID
	if task == "" {
		task = "—"
	}
	left := gradientText(current.theme, "COTERIX")
	middle := phaseValue(current.theme, data.Phase)
	right := current.theme.styles.Value.Render(
		fmt.Sprintf("%s · %d/%d", task, data.Confirmed, data.Total),
	)
	line := ansi.TruncateWc(
		left+"  "+middle+"  "+right,
		max(1, width-2),
		"…",
	)
	second := current.theme.styles.Muted.Render(
		ansi.TruncateWc(
			data.Role+" → "+data.CLI+
				" · gate "+plainOutcome(data.Gate)+
				" · review "+plainOutcome(data.Review),
			max(1, width-2),
			"…",
		),
	)
	return current.theme.styles.Header.
		Width(max(1, width-2)).
		Height(max(1, height)).
		Padding(0, 1).
		Render(line + "\n" + second)
}

func renderStatusBar(current model, width, height int) string {
	innerWidth := max(1, width-2)
	if current.prompt != promptNone {
		label := "Reject feedback"
		if current.prompt == promptResume {
			kind := ""
			if current.status.PendingAction != nil {
				kind = string(current.status.PendingAction.Kind)
			}
			label = "Response · " + kind
		}
		input := current.theme.styles.Input.Render(
			ansi.TruncateLeftWc(
				current.promptValue,
				max(1, innerWidth-len(label)-6),
				"…",
			) + current.theme.styles.InputCursor.Render("▌"),
		)
		footer := current.theme.styles.Muted.Render(
			"enter confirm · esc cancel",
		)
		if current.promptError != "" {
			footer = current.theme.styles.Error.Render(
				"× " + current.promptError,
			)
		}
		return current.theme.styles.StatusBar.
			Width(innerWidth).
			Height(max(1, height)).
			Padding(0, 1).
			Render(
				current.theme.styles.Label.Render(label+": ") +
					input + "\n" + footer,
			)
	}

	primary := statusSignal(current)
	hints := "j/k scroll · home/end · q quit"
	if current.hasStatus {
		switch current.status.Phase {
		case state.PhaseAwaitingApproval:
			hints = "a approve · r reject · " + hints
		case state.PhasePausedForInput:
			if current.status.PendingAction != nil &&
				current.status.PendingAction.Kind == state.PendingAuth {
				hints = "enter resume after login · " + hints
			} else {
				hints = "enter respond · " + hints
			}
		}
	}
	content := ansi.TruncateWc(primary, innerWidth, "…") + "\n" +
		current.theme.styles.Muted.Render(
			ansi.TruncateWc(hints, innerWidth, "…"),
		)
	return current.theme.styles.StatusBar.
		Width(innerWidth).
		Height(max(1, height)).
		Padding(0, 1).
		Render(content)
}

func statusSignal(current model) string {
	if current.stopping {
		return current.spinner.View() +
			current.theme.styles.Warning.Render(" Stopping safely")
	}
	if current.operation != "" || current.activeStep != "" {
		step := current.activeRole
		if step == "" {
			step = current.activeStep
		}
		return current.spinner.View() + gradientText(
			current.theme,
			" "+strings.ToUpper(step)+" WORKING",
		)
	}
	if !current.hasStatus {
		return current.theme.styles.Busy.Render("⋯ Starting pipeline")
	}
	if current.status.Phase == state.PhaseAwaitingApproval {
		return current.theme.styles.Pending.Render(
			"! Approval required — this is the normal plan gate",
		)
	}
	if current.status.PendingAction != nil {
		return current.theme.styles.Pending.Render(
			"! Pending action — " +
				string(current.status.PendingAction.Kind) +
				": " + current.status.PendingAction.Prompt,
		)
	}
	if current.operationErr != nil {
		return current.theme.styles.Error.Render(
			"× " + current.operationErr.Error(),
		)
	}
	return phaseValue(current.theme, current.status.Phase)
}

func gradientText(currentTheme theme, text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	stops := make([]color.Color, 0, len(currentTheme.tokens.Gradient.BrandLeftToRight))
	for _, token := range currentTheme.tokens.Gradient.BrandLeftToRight {
		stops = append(stops, lipgloss.Color(token))
	}
	colors := lipgloss.Blend1D(len(runes), stops...)
	if len(colors) != len(runes) {
		return currentTheme.styles.Logo.Render(text)
	}
	var output strings.Builder
	for index, character := range runes {
		output.WriteString(
			lipgloss.NewStyle().
				Foreground(colors[index]).
				Bold(true).
				Render(string(character)),
		)
	}
	return output.String()
}

func visibleLines(content string, height, scroll int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) <= height {
		return strings.Join(lines, "\n")
	}
	maxStart := len(lines) - height
	start := maxStart - max(0, scroll)
	if start < 0 {
		start = 0
	}
	end := min(len(lines), start+height)
	return strings.Join(lines[start:end], "\n")
}

func plainOutcome(outcome evidenceOutcome) string {
	switch outcome {
	case evidencePass:
		return "✓"
	case evidenceFail:
		return "×"
	default:
		return "⋯"
	}
}

// Keep the status type visible in this file so layout tests can construct
// snapshots without importing internal model implementation details.
var _ = pipeline.RunStatus{}
