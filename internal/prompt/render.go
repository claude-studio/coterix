package prompt

import (
	"embed"
	"fmt"
	"strings"
	"unicode"
)

// Template identifies one fixed agent-role contract.
type Template string

const (
	PlanTemplate           Template = "plan"
	PlanReviewTemplate     Template = "plan-review"
	ImplementationTemplate Template = "impl"
	ImplReviewTemplate     Template = "impl-review"
	FixTemplate            Template = "fix"
)

//go:embed templates/*.txt
var templateFiles embed.FS

var templatePaths = map[Template]string{
	PlanTemplate:           "templates/plan.txt",
	PlanReviewTemplate:     "templates/plan-review.txt",
	ImplementationTemplate: "templates/impl.txt",
	ImplReviewTemplate:     "templates/impl-review.txt",
	FixTemplate:            "templates/fix.txt",
}

// Values are inserted verbatim into a prompt. They are not reparsed as
// template syntax.
type Values map[string]string

// Names returns the fixed template names in deterministic order.
func Names() []Template {
	return []Template{
		PlanTemplate,
		PlanReviewTemplate,
		ImplementationTemplate,
		ImplReviewTemplate,
		FixTemplate,
	}
}

// Source returns the tracked embedded source for a template.
func Source(name Template) (string, error) {
	path, exists := templatePaths[name]
	if !exists {
		return "", fmt.Errorf("prompt: unknown template %q", name)
	}
	content, err := templateFiles.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("prompt: read embedded template %q: %w", name, err)
	}
	return string(content), nil
}

// Render expands {{VAR}} and non-nested {{#if VAR}} blocks from an embedded
// source. Missing active variables, unknown supplied variables, and malformed
// directives are rejected.
func Render(name Template, values Values) (string, error) {
	source, err := Source(name)
	if err != nil {
		return "", err
	}
	parsed, err := parseTemplate(source)
	if err != nil {
		return "", fmt.Errorf("prompt: parse template %q: %w", name, err)
	}
	for key := range values {
		if _, accepted := parsed.variables[key]; !accepted {
			return "", fmt.Errorf(
				"prompt: template %q does not accept variable %q",
				name,
				key,
			)
		}
	}

	var rendered strings.Builder
	if err := renderNodes(&rendered, parsed.nodes, values); err != nil {
		return "", fmt.Errorf("prompt: render template %q: %w", name, err)
	}
	return rendered.String(), nil
}

type parsedTemplate struct {
	nodes     []templateNode
	variables map[string]struct{}
}

type templateNode interface {
	render(*strings.Builder, Values) error
}

type textNode string

func (node textNode) render(output *strings.Builder, _ Values) error {
	output.WriteString(string(node))
	return nil
}

type variableNode string

func (node variableNode) render(output *strings.Builder, values Values) error {
	name := string(node)
	value, exists := values[name]
	if !exists {
		return fmt.Errorf("required variable %q is missing", name)
	}
	output.WriteString(value)
	return nil
}

type conditionalNode struct {
	name  string
	nodes []templateNode
}

func (node conditionalNode) render(output *strings.Builder, values Values) error {
	if strings.TrimSpace(values[node.name]) == "" {
		return nil
	}
	return renderNodes(output, node.nodes, values)
}

func renderNodes(output *strings.Builder, nodes []templateNode, values Values) error {
	for _, node := range nodes {
		if err := node.render(output, values); err != nil {
			return err
		}
	}
	return nil
}

func parseTemplate(source string) (parsedTemplate, error) {
	variables := make(map[string]struct{})
	nodes, position, closed, err := parseNodes(source, 0, false, variables)
	if err != nil {
		return parsedTemplate{}, err
	}
	if closed || position != len(source) {
		return parsedTemplate{}, fmt.Errorf("unexpected conditional terminator")
	}
	return parsedTemplate{nodes: nodes, variables: variables}, nil
}

func parseNodes(
	source string,
	position int,
	insideConditional bool,
	variables map[string]struct{},
) ([]templateNode, int, bool, error) {
	nodes := make([]templateNode, 0)
	for position < len(source) {
		relativeStart := strings.Index(source[position:], "{{")
		if relativeStart < 0 {
			remainder := source[position:]
			if strings.Contains(remainder, "}}") {
				return nil, position, false, fmt.Errorf("stray closing braces")
			}
			if insideConditional {
				return nil, position, false, fmt.Errorf("conditional block is not closed")
			}
			nodes = appendTextNode(nodes, remainder)
			return nodes, len(source), false, nil
		}
		start := position + relativeStart
		text := source[position:start]
		if strings.Contains(text, "}}") {
			return nil, position, false, fmt.Errorf("stray closing braces")
		}
		nodes = appendTextNode(nodes, text)

		relativeEnd := strings.Index(source[start+2:], "}}")
		if relativeEnd < 0 {
			return nil, position, false, fmt.Errorf("unterminated directive")
		}
		end := start + 2 + relativeEnd
		tag := source[start+2 : end]
		position = end + 2

		switch {
		case tag == "/if":
			if !insideConditional {
				return nil, position, false, fmt.Errorf("stray conditional terminator")
			}
			return nodes, position, true, nil

		case strings.HasPrefix(tag, "#if "):
			if insideConditional {
				return nil, position, false, fmt.Errorf("nested conditionals are not supported")
			}
			name := strings.TrimPrefix(tag, "#if ")
			if !validVariableName(name) {
				return nil, position, false, fmt.Errorf("invalid conditional %q", tag)
			}
			variables[name] = struct{}{}
			children, next, closed, err := parseNodes(source, position, true, variables)
			if err != nil {
				return nil, position, false, err
			}
			if !closed {
				return nil, position, false, fmt.Errorf("conditional %q is not closed", name)
			}
			nodes = append(nodes, conditionalNode{name: name, nodes: children})
			position = next

		case validVariableName(tag):
			variables[tag] = struct{}{}
			nodes = append(nodes, variableNode(tag))

		default:
			return nil, position, false, fmt.Errorf("invalid directive %q", tag)
		}
	}
	if insideConditional {
		return nil, position, false, fmt.Errorf("conditional block is not closed")
	}
	return nodes, position, false, nil
}

func appendTextNode(nodes []templateNode, text string) []templateNode {
	if text == "" {
		return nodes
	}
	return append(nodes, textNode(text))
}

func validVariableName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for _, character := range name[1:] {
		if character != '_' &&
			!unicode.IsUpper(character) &&
			!unicode.IsDigit(character) {
			return false
		}
	}
	return true
}
