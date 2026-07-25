package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

// pipelineStage is one row of the sidebar PIPELINE stepper. The five stages
// map one-for-one onto the pipeline flow: plan → plan_review → human approval
// → implementation/fix → gate/implementation_review.
type pipelineStage uint8

const (
	stagePlan pipelineStage = iota
	stageReview
	stageApprove
	stageImplement
	stageVerify
	stageCount
)

var stageLabels = [stageCount]string{
	"PLAN",
	"REVIEW",
	"APPROVE",
	"IMPLEMENT",
	"VERIFY",
}

func stageForStep(step string) (pipelineStage, bool) {
	switch step {
	case pipeline.StepPlan:
		return stagePlan, true
	case pipeline.StepPlanReview:
		return stageReview, true
	case pipeline.StepImplementation, pipeline.StepFix:
		return stageImplement, true
	case pipeline.StepGate, pipeline.StepImplementationReview:
		return stageVerify, true
	default:
		return stagePlan, false
	}
}

// stageClock accumulates wall time per stepper stage from UI-observed event
// arrival times. It never reaches into the pipeline: durations are a
// presentation-side measurement, injected through model.now for determinism.
type stageClock struct {
	elapsed     [stageCount]time.Duration
	activeStage pipelineStage
	hasActive   bool
	activeSince time.Time
	lastStage   pipelineStage
	hasLast     bool
}

func (clock *stageClock) stepStarted(step string, at time.Time) {
	stage, mapped := stageForStep(step)
	if !mapped {
		return
	}
	if clock.hasActive && clock.activeStage == stage {
		return
	}
	clock.closeActive(at)
	clock.activate(stage, at)
}

func (clock *stageClock) stepFinished(step string, at time.Time) {
	stage, mapped := stageForStep(step)
	if !mapped {
		return
	}
	if clock.hasActive && clock.activeStage == stage {
		clock.closeActive(at)
	}
}

// observePhase tracks the human approval window and closes the running
// measurement when the run reaches a terminal phase.
func (clock *stageClock) observePhase(phase state.Phase, at time.Time) {
	switch phase {
	case state.PhaseAwaitingApproval:
		if clock.hasActive && clock.activeStage == stageApprove {
			return
		}
		clock.closeActive(at)
		clock.activate(stageApprove, at)
	case state.PhaseDone, state.PhaseFailed:
		clock.closeActive(at)
	default:
		if clock.hasActive && clock.activeStage == stageApprove {
			clock.closeActive(at)
		}
	}
}

func (clock *stageClock) activate(stage pipelineStage, at time.Time) {
	clock.activeStage = stage
	clock.hasActive = true
	clock.activeSince = at
	clock.lastStage = stage
	clock.hasLast = true
}

func (clock *stageClock) closeActive(at time.Time) {
	if !clock.hasActive {
		return
	}
	if delta := at.Sub(clock.activeSince); delta > 0 {
		clock.elapsed[clock.activeStage] += delta
	}
	clock.hasActive = false
}

func (clock stageClock) elapsedAt(stage pipelineStage, now time.Time) time.Duration {
	total := clock.elapsed[stage]
	if clock.hasActive && clock.activeStage == stage {
		if delta := now.Sub(clock.activeSince); delta > 0 {
			total += delta
		}
	}
	return total
}

type stepperRowState uint8

const (
	stepperPending stepperRowState = iota
	stepperActive
	stepperDone
	stepperFailed
	stepperWaiting
)

type stepperRow struct {
	Label    string
	State    stepperRowState
	Duration time.Duration
}

// deriveStepper derives the five stage rows from state only: the run phase,
// the currently active step, and the pending-action kind. No artifacts, no
// git, no inference beyond what RunStatus already guarantees.
func deriveStepper(current model) [stageCount]stepperRow {
	now := current.now()
	rows := [stageCount]stepperRow{}
	for index := range rows {
		rows[index].Label = stageLabels[index]
		rows[index].State = stepperPending
		rows[index].Duration = current.stages.elapsedAt(
			pipelineStage(index),
			now,
		)
	}

	phase := state.PhasePlanning
	if current.hasStatus {
		phase = current.status.Phase
	}

	if phase == state.PhaseDone {
		for index := range rows {
			rows[index].State = stepperDone
		}
		return rows
	}

	position, positionState := stepperPosition(current, phase)
	for index := range rows {
		switch {
		case pipelineStage(index) < position:
			rows[index].State = stepperDone
		case pipelineStage(index) == position:
			rows[index].State = positionState
		}
	}
	return rows
}

func stepperPosition(
	current model,
	phase state.Phase,
) (pipelineStage, stepperRowState) {
	if stage, mapped := stageForStep(current.activeStep); mapped {
		return stage, stepperActive
	}
	switch phase {
	case state.PhaseAwaitingApproval:
		return stageApprove, stepperActive
	case state.PhaseImplementing:
		return stageImplement, stepperActive
	case state.PhasePausedForInput:
		return pausedStage(current), stepperWaiting
	case state.PhaseFailed:
		if current.stages.hasLast {
			return current.stages.lastStage, stepperFailed
		}
		return stagePlan, stepperFailed
	default:
		return stagePlan, stepperActive
	}
}

// pausedStage answers where the pause happened: the last stage this session
// observed, else a deterministic mapping from the pending-action kind.
func pausedStage(current model) pipelineStage {
	if current.stages.hasLast {
		return current.stages.lastStage
	}
	if current.hasStatus && current.status.PendingAction != nil {
		switch current.status.PendingAction.Kind {
		case state.PendingPlanCap:
			return stageReview
		case state.PendingTaskCap:
			return stageImplement
		}
	}
	return stagePlan
}

func renderStepper(
	currentTheme theme,
	rows [stageCount]stepperRow,
	width int,
) string {
	width = max(1, width)
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		icon, labelStyle := stepperRowStyle(currentTheme, row.State)
		left := icon + " " + labelStyle.Render(row.Label)
		duration := ""
		if row.Duration > 0 && row.State != stepperPending {
			duration = currentTheme.styles.Muted.Render(
				formatStageDuration(row.Duration),
			)
		}
		lines = append(lines, alignStatusLine(left, duration, width))
	}
	return strings.Join(lines, "\n")
}

func stepperRowStyle(
	currentTheme theme,
	rowState stepperRowState,
) (string, lipgloss.Style) {
	switch rowState {
	case stepperDone:
		return currentTheme.styles.PhaseSuccess.Render("✓"),
			currentTheme.styles.Value
	case stepperActive:
		return currentTheme.styles.Secondary.Bold(true).Render("●"),
			currentTheme.styles.Secondary.Bold(true)
	case stepperFailed:
		return currentTheme.styles.PhaseError.Render("×"),
			currentTheme.styles.PhaseError
	case stepperWaiting:
		return currentTheme.styles.PhaseWarning.Render("●"),
			currentTheme.styles.PhaseWarning
	default:
		return currentTheme.styles.Muted.Render("○"),
			currentTheme.styles.Muted
	}
}

func formatStageDuration(duration time.Duration) string {
	switch {
	case duration <= 0:
		return ""
	case duration < 10*time.Second:
		return fmt.Sprintf("%.1fs", duration.Seconds())
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	default:
		return fmt.Sprintf(
			"%dm%02ds",
			int(duration.Minutes()),
			int(duration.Seconds())%60,
		)
	}
}

// renderSidebarCard draws one bordered rail card with the title embedded in
// the top border, matching the north-star mock-up while spending exactly one
// row on the heading.
func renderSidebarCard(
	currentTheme theme,
	title string,
	body string,
	width int,
) string {
	return renderBoxCard(currentTheme, title, "", body, width, false)
}

// renderBoxCard is renderSidebarCard generalized over focus so the main pane can
// draw the same chrome and mark which box `j/k` currently drives (T14 W1/W2).
//
// suffix is already-styled trailing chrome for the heading — the artifact tab
// strip and the `↓ new` cue. It is separate from `title` so the two can lose room
// differently: the title is truncated to fit, but the suffix is dropped whole,
// because `ansi.TruncateWc` through a multi-colour strip cuts between a colour and
// its reset. (Passing it inside `title` would *not* flatten its colours —
// lipgloss keeps nested sequences; measured, T14 W3.)
func renderBoxCard(
	currentTheme theme,
	title string,
	suffix string,
	body string,
	width int,
	focused bool,
) string {
	width = max(8, width)
	inner := width - 2
	border := currentTheme.styles.Separator
	if focused {
		border = currentTheme.styles.BorderFocus
		// A non-color cue alongside the color: color-system.md forbids conveying
		// state by color alone (T14 W2 · review T14a f3).
		title = "▸ " + title
	}

	// Top border cells: "╭─ " + heading + " " + fill + "╮" == width, where the
	// heading is `title` plus, if it fits, "  " + suffix. The suffix is dropped
	// whole rather than truncated: cutting into its styling would leave a dangling
	// escape sequence, and the title is the part that must survive.
	heading := currentTheme.styles.SectionTitle.Render(title)
	titleWidth := ansi.StringWidth(title)
	if suffixWidth := ansi.StringWidth(suffix); suffixWidth > 0 &&
		width-5-titleWidth-2-suffixWidth >= 0 {
		heading += "  " + suffix
		titleWidth += 2 + suffixWidth
	}
	fill := width - 5 - titleWidth
	if fill < 0 {
		title = ansi.TruncateWc(title, max(1, ansi.StringWidth(title)+fill), "…")
		heading = currentTheme.styles.SectionTitle.Render(title)
		titleWidth = ansi.StringWidth(title)
		fill = max(0, width-5-titleWidth)
	}
	top := border.Render("╭─ ") +
		heading +
		border.Render(" "+strings.Repeat("─", fill)+"╮")

	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	rendered := make([]string, 0, len(lines)+2)
	rendered = append(rendered, top)
	textWidth := inner - 2
	for _, line := range lines {
		if ansi.StringWidth(line) > textWidth {
			line = ansi.TruncateWc(line, textWidth, "…")
		}
		pad := max(0, textWidth-ansi.StringWidth(line))
		rendered = append(
			rendered,
			border.Render("│")+" "+line+strings.Repeat(" ", pad)+" "+
				border.Render("│"),
		)
	}
	rendered = append(
		rendered,
		border.Render("╰"+strings.Repeat("─", inner)+"╯"),
	)
	return strings.Join(rendered, "\n")
}
