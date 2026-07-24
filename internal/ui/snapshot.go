package ui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
)

// SnapshotPresentation fixes the output shape from CLI argv meaning rather
// than from the number of returned runs.
type SnapshotPresentation uint8

const (
	SnapshotPresentationDetail SnapshotPresentation = iota + 1
	SnapshotPresentationTable
)

var snapshotBannerRows = []string{
	"██████  ██████  ████████ ███████ ██████  ██ ██   ██",
	"██     ██    ██    ██    ██      ██   ██ ██  ██ ██",
	"██     ██    ██    ██    █████   ██████  ██   ███",
	"██     ██    ██    ██    ██      ██   ██ ██  ██ ██",
	"██████  ██████     ██    ███████ ██   ██ ██ ██   ██",
}

// RenderSnapshot renders one non-interactive terminal snapshot. It reads only
// the embedded theme and the supplied statuses; persisted artifacts and git
// state are deliberately outside this presentation layer.
func RenderSnapshot(
	statuses []pipeline.RunStatus,
	width int,
	presentation SnapshotPresentation,
) (string, error) {
	currentTheme, err := loadTheme()
	if err != nil {
		return "", err
	}
	effectiveWidth := snapshotWidth(width)

	var body string
	switch presentation {
	case SnapshotPresentationDetail:
		if len(statuses) != 1 {
			return "", fmt.Errorf(
				"ui: detail snapshot requires exactly one run, got %d",
				len(statuses),
			)
		}
		body = renderSnapshotDetail(
			currentTheme,
			statuses[0],
			effectiveWidth,
		)
	case SnapshotPresentationTable:
		body = renderSnapshotTable(
			currentTheme,
			statuses,
			effectiveWidth,
		)
	default:
		return "", fmt.Errorf(
			"ui: unknown snapshot presentation %d",
			presentation,
		)
	}

	rendered := renderSnapshotBanner(currentTheme, effectiveWidth) +
		"\n" + body
	return constrainSnapshotWidth(rendered, effectiveWidth), nil
}

func snapshotWidth(width int) int {
	if width <= 0 {
		return wideBreakpointWidth
	}
	return width
}

func renderSnapshotBanner(currentTheme theme, width int) string {
	for _, row := range snapshotBannerRows {
		if ansi.StringWidth(row) > width {
			return ansi.TruncateWc(
				gradientText(currentTheme, "COTERIX"),
				width,
				"…",
			)
		}
	}

	rows := make([]string, 0, len(snapshotBannerRows))
	for _, row := range snapshotBannerRows {
		rows = append(rows, gradientText(currentTheme, row))
	}
	return strings.Join(rows, "\n")
}

func renderSnapshotDetail(
	currentTheme theme,
	status pipeline.RunStatus,
	width int,
) string {
	data := deriveStatusFields(status)
	innerWidth := max(1, width-6)
	body := renderSidebarBody(
		currentTheme,
		data,
		innerWidth,
		false,
	)
	return currentTheme.styles.Sidebar.
		Width(max(1, width-3)).
		Padding(1).
		Render(body)
}

func renderSnapshotTable(
	currentTheme theme,
	statuses []pipeline.RunStatus,
	width int,
) string {
	rows := make([][]string, 0, len(statuses))
	rowData := make([]sidebarData, 0, len(statuses))
	for _, status := range statuses {
		data := deriveStatusFields(status)
		rowData = append(rowData, data)
		rows = append(rows, []string{
			snapshotCell(data.RunID),
			snapshotCell(string(data.Phase)),
			strconv.Itoa(data.PlanRound),
			fmt.Sprintf("%d/%d", data.Confirmed, data.Total),
			snapshotCell(data.TaskID),
			snapshotCell(string(data.TaskStatus)),
			strconv.Itoa(data.Attempt),
			plainOutcome(data.Gate),
			plainOutcome(data.Review),
			snapshotSignal(data),
		})
	}

	// One-shot status tables keep one physical row per run. The table owns
	// column contraction at the requested width and truncates data cells.
	rendered := table.New().
		Headers(
			"run_id",
			"phase",
			"plan_round",
			"confirmed",
			"current_task",
			"task_status",
			"attempt",
			"gate",
			"review",
			"pending/approval",
		).
		Rows(rows...).
		Width(width).
		Wrap(false).
		Border(lipgloss.NormalBorder()).
		BorderStyle(currentTheme.styles.Separator).
		StyleFunc(func(row, column int) lipgloss.Style {
			if row == table.HeaderRow {
				return currentTheme.styles.SectionTitle
			}
			if row < 0 || row >= len(rowData) {
				return currentTheme.styles.Value
			}
			switch column {
			case 7, 8:
				outcome := rowData[row].Gate
				if column == 8 {
					outcome = rowData[row].Review
				}
				if outcome == evidencePass {
					return currentTheme.styles.PhaseSuccess
				}
				return currentTheme.styles.PhaseInfo
			case 9:
				if rowData[row].AwaitingApproval ||
					rowData[row].PendingKind != "" {
					// Inline signal, not a chip: foreground only.
					return currentTheme.styles.PhaseWarning
				}
				return currentTheme.styles.Muted
			default:
				return currentTheme.styles.Value
			}
		}).
		Render()
	if len(statuses) == 0 {
		rendered += "\n" + currentTheme.styles.Muted.Render("no runs")
	}
	return constrainSnapshotWidth(rendered, width)
}

func snapshotSignal(data sidebarData) string {
	switch {
	case data.AwaitingApproval:
		return "APPROVAL NEEDED"
	case data.PendingKind != "":
		signal := string(data.PendingKind)
		if prompt := snapshotCell(data.PendingPrompt); prompt != "—" {
			signal += ": " + prompt
		}
		return signal
	default:
		return "—"
	}
}

func snapshotCell(value string) string {
	value = strings.NewReplacer(
		"\r\n", " ",
		"\r", " ",
		"\n", " ",
		"\t", " ",
	).Replace(value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "—"
	}
	return value
}

func constrainSnapshotWidth(rendered string, width int) string {
	width = max(1, width)
	lines := strings.Split(rendered, "\n")
	for index, line := range lines {
		if ansi.StringWidth(line) > width {
			lines[index] = ansi.TruncateWc(line, width, "…")
		}
	}
	return strings.Join(lines, "\n")
}
