package pipeline

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	goalLinePattern    = regexp.MustCompile(`^# (.+)$`)
	taskHeadingPattern = regexp.MustCompile(`^## (T[0-9]+): (.+)$`)
)

// Plan is a structurally validated implementation plan.
type Plan struct {
	Goal  string
	Tasks []PlanTask
}

// PlanTask is one task section in document order. Body preserves the complete
// section with LF line endings for use by a later implementation prompt.
type PlanTask struct {
	ID         string
	Title      string
	Work       string
	Acceptance string
	Verify     string
	Body       string
}

// ParsePlan parses the exact line-oriented structure required by the planner
// prompt. Blank lines are permitted around the goal and between task sections,
// but not within a task section.
func ParsePlan(content []byte) (Plan, error) {
	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	lineIndex := skipBlankLines(lines, 0)

	if lineIndex == len(lines) {
		return Plan{}, fmt.Errorf("pipeline: plan is empty")
	}
	goalMatch := goalLinePattern.FindStringSubmatch(lines[lineIndex])
	if goalMatch == nil || strings.TrimSpace(goalMatch[1]) == "" {
		return Plan{}, fmt.Errorf(
			"pipeline: line %d must be a non-empty goal heading of the form \"# <goal>\"",
			lineIndex+1,
		)
	}
	plan := Plan{
		Goal:  strings.TrimSpace(goalMatch[1]),
		Tasks: make([]PlanTask, 0),
	}

	lineIndex = skipBlankLines(lines, lineIndex+1)
	taskIDs := make(map[string]struct{})
	for lineIndex < len(lines) {
		taskStart := lineIndex
		headingMatch := taskHeadingPattern.FindStringSubmatch(lines[lineIndex])
		if headingMatch == nil || strings.TrimSpace(headingMatch[2]) == "" {
			return Plan{}, fmt.Errorf(
				"pipeline: line %d must be a task heading of the form \"## T<n>: <title>\"",
				lineIndex+1,
			)
		}
		taskID := headingMatch[1]
		if _, duplicate := taskIDs[taskID]; duplicate {
			return Plan{}, fmt.Errorf(
				"pipeline: line %d duplicates task id %q",
				lineIndex+1,
				taskID,
			)
		}

		work, err := parseTaskField(
			lines,
			lineIndex+1,
			"- [ ] ",
			"an open checkbox work item",
		)
		if err != nil {
			return Plan{}, err
		}
		acceptance, err := parseTaskField(
			lines,
			lineIndex+2,
			"Acceptance: ",
			"Acceptance",
		)
		if err != nil {
			return Plan{}, err
		}
		verify, err := parseTaskField(
			lines,
			lineIndex+3,
			"Verify: ",
			"Verify",
		)
		if err != nil {
			return Plan{}, err
		}

		taskEnd := lineIndex + 4
		plan.Tasks = append(plan.Tasks, PlanTask{
			ID:         taskID,
			Title:      strings.TrimSpace(headingMatch[2]),
			Work:       work,
			Acceptance: acceptance,
			Verify:     verify,
			Body:       strings.Join(lines[taskStart:taskEnd], "\n"),
		})
		taskIDs[taskID] = struct{}{}
		lineIndex = skipBlankLines(lines, taskEnd)
	}

	if len(plan.Tasks) == 0 {
		return Plan{}, fmt.Errorf("pipeline: plan must contain at least one task")
	}
	return plan, nil
}

// ValidatePlan reports whether content satisfies the exact plan grammar.
func ValidatePlan(content []byte) error {
	_, err := ParsePlan(content)
	return err
}

func parseTaskField(
	lines []string,
	lineIndex int,
	prefix string,
	description string,
) (string, error) {
	if lineIndex >= len(lines) {
		return "", fmt.Errorf(
			"pipeline: line %d must be a non-empty %s line",
			lineIndex+1,
			description,
		)
	}
	line := lines[lineIndex]
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf(
			"pipeline: line %d must be a non-empty %s line",
			lineIndex+1,
			description,
		)
	}
	value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if value == "" {
		return "", fmt.Errorf(
			"pipeline: line %d must be a non-empty %s line",
			lineIndex+1,
			description,
		)
	}
	return value, nil
}

func skipBlankLines(lines []string, start int) int {
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	return start
}
