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

const (
	sidebarWidth    = 32
	topBarHeight    = 2
	wordmarkText    = "C O T E R I X"
	logTagCellWidth = 6
)

// THESIS: Make a synchronous multi-agent pipeline readable as one live
// instrument panel, without turning it into chat or session chrome.
// OWN-WORLD: COTERIX ink surfaces, token-driven Claude→Codex accents, compact
// evidence marks, and a single right-hand telemetry rail.
// STORY: Follow agent output, verify hard/soft gates, then answer the one human
// action that is actually blocking progress.
// FIRST VIEWPORT: Wide terminals show a brand top bar, a bordered feed card,
// and a 32-cell rail of PIPELINE/STATUS cards; compact terminals fold that
// rail into a two-line header above the same feed and status bar.
// FORM: Operate-mode dashboard, fixed by docs/spec/ui.md
// (north star: assets/coterix-color-system.png).

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
		contentHeight := max(1, height-topBarHeight-statusHeight)
		topBar := renderTopBar(current, width, topBarHeight)
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
				Width:   width,
				Height:  topBarHeight,
				Content: topBar,
			},
			{
				X:       0,
				Y:       topBarHeight,
				Width:   mainWidth,
				Height:  contentHeight,
				Content: main,
			},
			{
				X:       mainWidth,
				Y:       topBarHeight,
				Width:   sidebarWidth,
				Height:  contentHeight,
				Content: sidebar,
			},
			{
				X:       0,
				Y:       topBarHeight + contentHeight,
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
	innerHeight := max(1, height-3)
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
	body := renderMainHeader(current, innerWidth) + "\n" + visible
	return current.theme.styles.MainCard.
		Width(max(1, width)).
		Height(max(1, height)).
		Padding(0, 1).
		Render(body)
}

// renderMainHeader is the feed card's command line: what this dashboard is
// running on the left, one live signal on the right.
func renderMainHeader(current model, width int) string {
	left := current.theme.styles.Secondary.Bold(true).Render("coterix run ") +
		current.theme.styles.Value.Render(
			ansi.TruncateWc(
				strings.Join(strings.Fields(current.request), " "),
				max(1, width-24),
				"…",
			),
		)
	right := current.theme.styles.Muted.Render("● idle")
	if current.isWorking() {
		right = current.theme.styles.PhaseBusy.Render("● real-time")
	}
	return alignStatusLine(left, right, max(1, width))
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

// renderLogLine renders one columnar feed row:
// `HH:MM:SS TAG    icon role#attempt message`. The tag column carries the
// cross-model color (claude/codex); role and attempt stay in the message
// prefix so no information from the old bracket prefix is lost.
func renderLogLine(currentTheme theme, line logEntry, width int) string {
	timestamp := strings.Repeat(" ", 8)
	if !line.At.IsZero() {
		timestamp = line.At.Format("15:04:05")
	}
	timeColumn := currentTheme.styles.Muted.Render(timestamp)
	tagColumn := renderLogTag(currentTheme, line)
	iconColumn := renderLogIcon(currentTheme, line)

	label := line.Role
	if label == "" {
		label = line.Step
	}
	if label == "" {
		label = "process"
	}
	if line.Attempt > 0 {
		label += "#" + strconv.Itoa(line.Attempt)
	}
	meta := currentTheme.styles.Label.Render(label)

	text := remapANSI16(line.Text, currentTheme.tokens.ANSI)
	if line.Stream == runner.StreamStderr {
		text = currentTheme.styles.Error.Render(text)
	} else {
		text = currentTheme.styles.Value.Render(text)
	}
	return ansi.TruncateWc(
		timeColumn+" "+tagColumn+" "+iconColumn+" "+meta+" "+text,
		max(1, width),
		"…",
	)
}

func renderLogTag(currentTheme theme, line logEntry) string {
	tag := strings.ToUpper(strings.TrimSpace(line.CLI))
	style := currentTheme.styles.Muted
	switch strings.ToLower(strings.TrimSpace(line.CLI)) {
	case "claude":
		style = currentTheme.styles.Claude
	case "codex":
		style = currentTheme.styles.Codex
	case "coterix", "":
		tag = "INFO"
	}
	tag = ansi.TruncateWc(tag, logTagCellWidth, "…")
	if pad := logTagCellWidth - ansi.StringWidth(tag); pad > 0 {
		tag += strings.Repeat(" ", pad)
	}
	return style.Render(tag)
}

func renderLogIcon(currentTheme theme, line logEntry) string {
	switch line.Icon {
	case logIconStart:
		return currentTheme.styles.PhaseInfo.Render("▸")
	case logIconDone:
		return currentTheme.styles.PhaseSuccess.Render("✓")
	case logIconFail:
		return currentTheme.styles.PhaseError.Render("×")
	default:
		if line.Stream == runner.StreamStderr {
			return currentTheme.styles.PhaseError.Render("×")
		}
		return currentTheme.styles.Muted.Render("·")
	}
}

func renderSidebar(current model, width, height int) string {
	cardWidth := max(8, width-2)
	innerWidth := max(1, cardWidth-4)
	pipelineCard := renderSidebarCard(
		current.theme,
		"PIPELINE",
		renderStepper(current.theme, deriveStepper(current), innerWidth),
		cardWidth,
	)
	statusCard := renderSidebarCard(
		current.theme,
		"STATUS",
		renderSidebarBody(
			current.theme,
			deriveSidebar(current),
			innerWidth,
			true,
		),
		cardWidth,
	)
	content := pipelineCard + "\n" + statusCard
	indented := make([]string, 0, strings.Count(content, "\n")+1)
	for _, line := range strings.Split(content, "\n") {
		indented = append(indented, " "+line)
	}
	return strings.Join(indented[:min(len(indented), max(1, height))], "\n")
}

func renderSidebarBody(
	currentTheme theme,
	data sidebarData,
	innerWidth int,
	showActionHints bool,
) string {
	innerWidth = max(1, innerWidth)
	var content strings.Builder
	content.WriteString(renderSidebarSectionTitle(
		currentTheme,
		"RUN",
		innerWidth,
	))
	content.WriteString("\n")
	writeSidebarField(
		&content,
		currentTheme,
		"run",
		ansi.TruncateWc(data.RunID, innerWidth, "…"),
		innerWidth,
	)
	writeSidebarStyledField(
		&content,
		currentTheme,
		"",
		phaseValue(currentTheme, data.Phase),
		innerWidth,
	)
	content.WriteString(renderSidebarSectionTitle(
		currentTheme,
		"ROUTING",
		innerWidth,
	))
	content.WriteString("\n")
	writeSidebarStyledField(
		&content,
		currentTheme,
		"",
		cliRoleStyle(
			currentTheme,
			data.CLI,
			currentTheme.styles.Value,
		).Render(data.Role+" → "+data.CLI),
		innerWidth,
	)
	content.WriteString(renderSidebarSectionTitle(
		currentTheme,
		"TASK",
		innerWidth,
	))
	content.WriteString("\n")
	task := data.TaskID
	if string(data.TaskStatus) != "" {
		task += " · " + string(data.TaskStatus)
	}
	task += " · #" + strconv.Itoa(data.Attempt)
	writeSidebarField(
		&content,
		currentTheme,
		"task",
		task,
		innerWidth,
	)
	writeSidebarStyledField(
		&content,
		currentTheme,
		"gate / review",
		outcomeIcon(currentTheme, data.Gate)+"  "+
			outcomeIcon(currentTheme, data.Review),
		innerWidth,
	)
	writeSidebarField(
		&content,
		currentTheme,
		"progress",
		fmt.Sprintf(
			"round %d · %d/%d",
			data.PlanRound,
			data.Confirmed,
			data.Total,
		),
		innerWidth,
	)

	if data.AwaitingApproval {
		content.WriteString(
			currentTheme.styles.Warning.Render("! APPROVAL NEEDED"),
		)
		content.WriteString("\n")
		if showActionHints {
			content.WriteString(
				currentTheme.styles.Warning.Render("a approve · r reject"),
			)
			content.WriteString("\n")
		}
	} else if data.PendingKind != "" {
		content.WriteString(
			currentTheme.styles.Warning.Render(
				pendingChipText(data.PendingKind),
			),
		)
		content.WriteString("\n")
		content.WriteString(
			currentTheme.styles.Warning.Render(
				ansi.HardwrapWc(data.PendingPrompt, innerWidth, false),
			),
		)
		content.WriteString("\n")
	}
	if data.LastError != "" {
		content.WriteString(
			currentTheme.styles.Error.Render(
				"× " + ansi.HardwrapWc(
					data.LastError,
					max(1, innerWidth-2),
					false,
				),
			),
		)
		content.WriteString("\n")
	}

	return content.String()
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

	statusData := deriveStatusFields(current.status)
	statusGate := statusData.Gate
	statusReview := statusData.Review
	statusData.Gate = current.artifacts.GateOutcome
	statusData.Review = current.artifacts.ReviewOutcome
	if statusGate == evidencePass {
		statusData.Gate = statusGate
		statusData.Review = statusReview
	}
	if current.activeRole != "" || current.activeStep != "" {
		statusData.Role = current.activeRole
		if statusData.Role == "" {
			statusData.Role = current.activeStep
		}
		statusData.CLI = current.activeCLI
	}
	if current.activeStep == "" {
		switch {
		case statusData.AwaitingApproval:
			statusData.Role = "human_gate"
			statusData.CLI = "operator"
		case statusData.PendingKind != "":
			statusData.Role = "pending_action"
			statusData.CLI = "operator"
		case current.status.Phase == state.PhaseDone ||
			current.status.Phase == state.PhaseFailed:
			statusData.Role = "—"
			statusData.CLI = "—"
		}
	}
	return statusData
}

func deriveStatusFields(status pipeline.RunStatus) sidebarData {
	data := sidebarData{
		RunID:       status.RunID,
		Phase:       status.Phase,
		Role:        "—",
		CLI:         "—",
		TaskID:      "—",
		TaskStatus:  state.TaskOpen,
		Gate:        evidenceUnknown,
		Review:      evidenceUnknown,
		PlanRound:   status.PlanRound,
		Total:       len(status.TaskOrder),
		PendingKind: "",
	}
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
	switch {
	case data.AwaitingApproval:
		data.Role = "human_gate"
		data.CLI = "operator"
	case data.PendingKind != "":
		data.Role = "pending_action"
		data.CLI = "operator"
	}
	return data
}

func writeSidebarField(
	builder *strings.Builder,
	currentTheme theme,
	label string,
	value string,
	width int,
) {
	if value == "" {
		value = "—"
	}
	writeSidebarStyledField(
		builder,
		currentTheme,
		label,
		currentTheme.styles.Value.Render(value),
		width,
	)
}

func writeSidebarStyledField(
	builder *strings.Builder,
	currentTheme theme,
	label string,
	value string,
	width int,
) {
	prefix := currentTheme.styles.SectionTitle.Render("▌") + " "
	if label != "" {
		prefix += currentTheme.styles.Label.Render(label + ": ")
	}
	builder.WriteString(prefix)
	available := max(1, width-ansi.StringWidth(prefix))
	builder.WriteString(ansi.TruncateWc(value, available, "…"))
	builder.WriteString("\n")
}

func phaseValue(currentTheme theme, phase state.Phase) string {
	return phaseDot(currentTheme, phase) + " " +
		currentTheme.styles.Value.Render(string(phase))
}

func phaseDot(currentTheme theme, phase state.Phase) string {
	switch phase {
	case state.PhaseDone:
		return currentTheme.styles.PhaseSuccess.Render("●")
	case state.PhaseFailed:
		return currentTheme.styles.PhaseError.Render("●")
	case state.PhaseAwaitingApproval, state.PhasePausedForInput:
		return currentTheme.styles.PhaseWarning.Render("●")
	default:
		return currentTheme.styles.PhaseInfo.Render("●")
	}
}

func outcomeIcon(currentTheme theme, outcome evidenceOutcome) string {
	switch outcome {
	case evidencePass:
		return currentTheme.styles.Success.Render("✓")
	case evidenceFail:
		return currentTheme.styles.Error.Render("×")
	default:
		return currentTheme.styles.PhaseInfo.Render("⋯")
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
		footer := current.theme.styles.Hint.Render(
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
	contentWidth := max(1, innerWidth-2)
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
	content := alignStatusLine(
		primary,
		current.theme.styles.Hint.Render(hints),
		contentWidth,
	)
	if detail := statusDetail(current, contentWidth); detail != "" {
		content += "\n" + detail
	}
	return current.theme.styles.StatusBar.
		Width(innerWidth).
		Height(max(1, height)).
		Padding(0, 1).
		Render(content)
}

func statusSignal(current model) string {
	if current.stopping {
		return statusChip(
			current.theme.styles.Warning,
			"⟳ STOPPING SAFELY",
		)
	}
	if current.isWorking() {
		step := current.activeRole
		if step == "" {
			step = current.activeStep
		}
		return workingStatusChip(
			current.theme,
			current.spinner.View()+workingGradientText(
				current.theme,
				" "+strings.ToUpper(step)+" WORKING",
			),
		)
	}
	if !current.hasStatus {
		return statusChip(
			current.theme.styles.Info,
			"◇ STARTING PIPELINE",
		)
	}
	if current.status.Phase == state.PhaseAwaitingApproval {
		return statusChip(
			current.theme.styles.Warning,
			"! APPROVAL NEEDED",
		)
	}
	if current.status.PendingAction != nil {
		return statusChip(
			current.theme.styles.Warning,
			pendingChipText(current.status.PendingAction.Kind),
		)
	}
	if current.operationErr != nil {
		return statusChip(
			current.theme.styles.Error,
			"× OPERATION FAILED",
		)
	}
	return statusChip(
		phaseChipStyle(current.theme, current.status.Phase),
		"● "+strings.ToUpper(
			strings.ReplaceAll(string(current.status.Phase), "_", " "),
		),
	)
}

// renderTopBar draws the brand bar from the north-star mock-up: gradient
// wordmark on the left, one orchestration-activity signal in the middle, the
// run identity on the right, and a gradient underline under the wordmark.
func renderTopBar(current model, width, height int) string {
	innerWidth := max(1, width-2)
	contentWidth := max(1, innerWidth-2)
	wordmark := gradientText(current.theme, wordmarkText)

	activity := current.theme.styles.Muted.Render("● orchestration idle")
	if current.isWorking() {
		activity = current.theme.styles.PhaseBusy.Render(
			"● orchestration active",
		)
	}

	runID := "starting…"
	if current.hasStatus && current.status.RunID != "" {
		runID = current.status.RunID
	}
	identity := current.theme.styles.Label.Render("run: ") +
		current.theme.styles.Value.Render(
			ansi.TruncateWc(runID, max(1, contentWidth/3), "…"),
		)

	first := alignTriple(wordmark, activity, identity, contentWidth)
	underline := gradientText(
		current.theme,
		strings.Repeat("─", ansi.StringWidth(wordmarkText)),
	)
	return current.theme.styles.Header.
		Width(innerWidth).
		Height(max(1, height)).
		Padding(0, 1).
		Render(first + "\n" + underline)
}

// alignTriple lays out left, center, and right segments on one line. When the
// budget is tight the center yields first, then the right.
func alignTriple(left, center, right string, width int) string {
	if width <= 0 {
		return ""
	}
	leftWidth := ansi.StringWidth(left)
	rightWidth := ansi.StringWidth(right)
	centerWidth := ansi.StringWidth(center)
	if leftWidth+centerWidth+rightWidth+2 > width {
		return alignStatusLine(left, right, width)
	}
	centerStart := max(leftWidth+1, (width-centerWidth)/2)
	if centerStart+centerWidth+1+rightWidth > width {
		centerStart = width - rightWidth - 1 - centerWidth
	}
	rightStart := width - rightWidth
	return left +
		strings.Repeat(" ", centerStart-leftWidth) +
		center +
		strings.Repeat(" ", max(1, rightStart-centerStart-centerWidth)) +
		right
}

func renderSidebarSectionTitle(
	currentTheme theme,
	label string,
	width int,
) string {
	title := brandEndpointGradient(currentTheme, "╱╱╱") + " " +
		currentTheme.styles.SectionTitle.Render(label)
	remaining := width - ansi.StringWidth(title) - 1
	if remaining <= 0 {
		return ansi.TruncateWc(title, max(1, width), "…")
	}
	return title + " " + currentTheme.styles.Separator.Render(
		strings.Repeat("─", remaining),
	)
}

func cliRoleStyle(
	currentTheme theme,
	cliName string,
	fallback lipgloss.Style,
) lipgloss.Style {
	switch strings.ToLower(strings.TrimSpace(cliName)) {
	case "claude":
		return currentTheme.styles.Claude
	case "codex":
		return currentTheme.styles.Codex
	default:
		return fallback
	}
}

func statusDetail(current model, width int) string {
	if width <= 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	if current.hasStatus && current.status.PendingAction != nil {
		if prompt := strings.TrimSpace(current.status.PendingAction.Prompt); prompt != "" {
			parts = append(
				parts,
				current.theme.styles.Warning.Render(prompt),
			)
		}
	}
	if current.hasStatus && current.status.LastError != nil {
		if message := strings.TrimSpace(*current.status.LastError); message != "" {
			parts = append(
				parts,
				current.theme.styles.Error.Render("× "+message),
			)
		}
	} else if current.operationErr != nil {
		parts = append(
			parts,
			current.theme.styles.Error.Render(
				"× "+current.operationErr.Error(),
			),
		)
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return ansi.TruncateWc(parts[0], width, "…")
	}
	separator := " · "
	available := width - ansi.StringWidth(separator)
	if available < 2 {
		return ansi.TruncateWc(strings.Join(parts, separator), width, "…")
	}
	leftWidth := available / 2
	rightWidth := available - leftWidth
	return ansi.TruncateWc(parts[0], leftWidth, "…") +
		separator +
		ansi.TruncateWc(parts[1], rightWidth, "…")
}

func alignStatusLine(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	leftWidth := ansi.StringWidth(left)
	if leftWidth >= width {
		return ansi.TruncateWc(left, width, "…")
	}
	available := width - leftWidth - 1
	if available <= 0 {
		return left
	}
	right = ansi.TruncateWc(right, available, "…")
	padding := max(1, width-leftWidth-ansi.StringWidth(right))
	return left + strings.Repeat(" ", padding) + right
}

func statusChip(style lipgloss.Style, content string) string {
	return style.Bold(true).Padding(0, 1).Render(content)
}

func workingStatusChip(currentTheme theme, content string) string {
	padding := currentTheme.styles.Busy.Render(" ")
	return padding + content + padding
}

func phaseChipStyle(currentTheme theme, phase state.Phase) lipgloss.Style {
	switch phase {
	case state.PhaseAwaitingApproval, state.PhasePausedForInput:
		return currentTheme.styles.Warning
	case state.PhaseDone:
		return currentTheme.styles.Success
	case state.PhaseFailed:
		return currentTheme.styles.Error
	default:
		return currentTheme.styles.Info
	}
}

func pendingChipText(kind state.PendingKind) string {
	switch kind {
	case state.PendingPlanQuestion:
		return "? PENDING · plan_question"
	case state.PendingPlanCap:
		return "⟳ PENDING · plan_cap"
	case state.PendingTaskCap:
		return "▶ PENDING · task_cap"
	case state.PendingAuth:
		return "◇ PENDING · auth"
	default:
		return "! PENDING"
	}
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

func workingGradientText(currentTheme theme, text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	stops := make(
		[]color.Color,
		0,
		len(currentTheme.tokens.Gradient.BrandLeftToRight),
	)
	for _, token := range currentTheme.tokens.Gradient.BrandLeftToRight {
		stops = append(stops, lipgloss.Color(token))
	}
	colors := lipgloss.Blend1D(len(runes), stops...)
	if len(colors) != len(runes) {
		return currentTheme.styles.Busy.Render(text)
	}
	background := lipgloss.Color(currentTheme.tokens.Status.Busy.BG)
	var output strings.Builder
	for index, character := range runes {
		output.WriteString(
			lipgloss.NewStyle().
				Foreground(colors[index]).
				Background(background).
				Bold(true).
				Render(string(character)),
		)
	}
	return output.String()
}

func brandEndpointGradient(currentTheme theme, text string) string {
	runes := []rune(text)
	stops := currentTheme.tokens.Gradient.BrandLeftToRight
	if len(runes) == 0 || len(stops) == 0 {
		return ""
	}
	colors := lipgloss.Blend1D(
		len(runes),
		lipgloss.Color(stops[0]),
		lipgloss.Color(stops[len(stops)-1]),
	)
	if len(colors) != len(runes) {
		return currentTheme.styles.SectionTitle.Render(text)
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
