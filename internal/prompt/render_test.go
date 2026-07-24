package prompt

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestEmbeddedSourcesMatchCanonicalPrompts(t *testing.T) {
	t.Parallel()

	expected := map[Template]string{
		PlanTemplate:           "7714041e2b14a495237b529fad98e34c1833c9668dc56713f53f4fe859ab448d",
		PlanReviewTemplate:     "34980e365c6e5c0d41f212cf0a99d46aa96b88b39e371157c399a8c0b6867820",
		ImplementationTemplate: "2eb80ac83794343929e1b985e109d7afc196f6509035d3fcb77cf6dff10f55b4",
		ImplReviewTemplate:     "607dbd5d9f42426eeceb70d73aa9d858cb6ae8ede789ee5c2d8e18bba9920490",
		FixTemplate:            "b7839bd3839f6fa689363e9bcffbfaa3d03a5ef98d7ef38bff7c4ae3624ba91c",
	}

	names := Names()
	if len(names) != len(expected) {
		t.Fatalf("Names() returned %d templates; want %d", len(names), len(expected))
	}
	seen := make(map[Template]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("Names() contains duplicate template %q", name)
		}
		seen[name] = true

		source, err := Source(name)
		if err != nil {
			t.Fatalf("Source(%q): %v", name, err)
		}
		sum := sha256.Sum256([]byte(source))
		if got, want := fmt.Sprintf("%x", sum), expected[name]; got != want {
			t.Errorf("Source(%q) sha256 = %s; want canonical %s", name, got, want)
		}
	}
	for name := range expected {
		if !seen[name] {
			t.Errorf("Names() omits template %q", name)
		}
	}
}

func TestRenderPlanTemplate(t *testing.T) {
	t.Parallel()

	base := Values{
		"REQUEST":           "request-value",
		"CURRENT_PLAN_PATH": "",
		"PLAN_OUTPUT_PATH":  "plan-output-value",
		"QUESTIONS_PATH":    "questions-value",
	}
	rendered := mustRender(t, PlanTemplate, base)
	assertOccurrences(t, rendered, map[string]int{
		"request-value":     1,
		"plan-output-value": 5,
		"questions-value":   3,
	})
	assertAbsent(t, rendered, "This is a REVISION.", "current-plan-value", "{{")

	revision := cloneValues(base)
	revision["FEEDBACK"] = "feedback-value"
	revision["CURRENT_PLAN_PATH"] = "current-plan-value"
	rendered = mustRender(t, PlanTemplate, revision)
	assertOccurrences(t, rendered, map[string]int{
		"request-value":       1,
		"plan-output-value":   6,
		"questions-value":     4,
		"feedback-value":      1,
		"current-plan-value":  4,
		"This is a REVISION.": 1,
	})
	assertAbsent(t, rendered, "{{")

	whitespaceFeedback := cloneValues(base)
	whitespaceFeedback["FEEDBACK"] = " \t\n"
	rendered = mustRender(t, PlanTemplate, whitespaceFeedback)
	assertAbsent(t, rendered, "This is a REVISION.", "{{")
}

func TestRenderPlanReviewTemplate(t *testing.T) {
	t.Parallel()

	rendered := mustRender(t, PlanReviewTemplate, Values{
		"PLAN_PATH":   "plan-path-value",
		"PLAN_HASH":   "plan-hash-value",
		"REQUEST":     "request-value",
		"REVIEW_PATH": "review-path-value",
	})
	assertOccurrences(t, rendered, map[string]int{
		"plan-path-value":   2,
		"plan-hash-value":   3,
		"request-value":     1,
		"review-path-value": 2,
	})
	assertAbsent(t, rendered, "{{")
}

func TestRenderImplementationTemplate(t *testing.T) {
	t.Parallel()

	base := Values{
		"PLAN_PATH":      "plan-path-value",
		"TASK_BODY":      "task-body-value",
		"COMMIT_HISTORY": "commit-history-value",
	}
	rendered := mustRender(t, ImplementationTemplate, base)
	assertOccurrences(t, rendered, map[string]int{
		"plan-path-value":      1,
		"task-body-value":      1,
		"commit-history-value": 1,
	})
	assertAbsent(t, rendered, "Note about the previous iteration:", "{{")

	withStall := cloneValues(base)
	withStall["STALL_NOTE"] = "stall-note-value"
	rendered = mustRender(t, ImplementationTemplate, withStall)
	assertOccurrences(t, rendered, map[string]int{
		"stall-note-value":                   1,
		"Note about the previous iteration:": 1,
	})
	assertAbsent(t, rendered, "{{")

	whitespaceStall := cloneValues(base)
	whitespaceStall["STALL_NOTE"] = "\n\t "
	rendered = mustRender(t, ImplementationTemplate, whitespaceStall)
	assertAbsent(t, rendered, "Note about the previous iteration:", "{{")
}

func TestRenderImplementationReviewTemplate(t *testing.T) {
	t.Parallel()

	rendered := mustRender(t, ImplReviewTemplate, Values{
		"PLAN_PATH":     "plan-path-value",
		"PLAN_HASH":     "plan-hash-value",
		"TASK_ID":       "task-id-value",
		"BASE_SHA":      "base-sha-value",
		"CANDIDATE_SHA": "candidate-sha-value",
		"REVIEW_PATH":   "review-path-value",
	})
	assertOccurrences(t, rendered, map[string]int{
		"plan-path-value":     1,
		"plan-hash-value":     3,
		"task-id-value":       3,
		"base-sha-value":      1,
		"candidate-sha-value": 3,
		"review-path-value":   2,
	})
	assertAbsent(t, rendered, "{{")
}

func TestRenderFixTemplate(t *testing.T) {
	t.Parallel()

	base := Values{
		"PLAN_PATH":     "plan-path-value",
		"TASK_ID":       "task-id-value",
		"CANDIDATE_SHA": "candidate-sha-value",
		"FINDINGS":      "findings-value",
	}
	rendered := mustRender(t, FixTemplate, base)
	assertOccurrences(t, rendered, map[string]int{
		"plan-path-value":     1,
		"task-id-value":       1,
		"candidate-sha-value": 1,
		"findings-value":      1,
	})
	assertAbsent(t, rendered, "Trusted gate failure", "{{")

	withGate := cloneValues(base)
	withGate["GATE_OUTPUT"] = "gate-output-value"
	rendered = mustRender(t, FixTemplate, withGate)
	assertOccurrences(t, rendered, map[string]int{
		"gate-output-value":    1,
		"Trusted gate failure": 1,
	})
	assertAbsent(t, rendered, "{{")

	whitespaceGate := cloneValues(base)
	whitespaceGate["GATE_OUTPUT"] = " \n"
	rendered = mustRender(t, FixTemplate, whitespaceGate)
	assertAbsent(t, rendered, "Trusted gate failure", "{{")
}

func TestRenderRejectsEveryMissingRequiredVariable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template Template
		values   Values
		required []string
	}{
		{
			name:     "plan",
			template: PlanTemplate,
			values: Values{
				"REQUEST":           "request",
				"CURRENT_PLAN_PATH": "",
				"PLAN_OUTPUT_PATH":  "plan",
				"QUESTIONS_PATH":    "questions",
			},
			required: []string{
				"REQUEST",
				"CURRENT_PLAN_PATH",
				"PLAN_OUTPUT_PATH",
				"QUESTIONS_PATH",
			},
		},
		{
			name:     "plan review",
			template: PlanReviewTemplate,
			values: Values{
				"PLAN_PATH":   "plan",
				"PLAN_HASH":   "hash",
				"REQUEST":     "request",
				"REVIEW_PATH": "review",
			},
			required: []string{"PLAN_PATH", "PLAN_HASH", "REQUEST", "REVIEW_PATH"},
		},
		{
			name:     "implementation",
			template: ImplementationTemplate,
			values: Values{
				"PLAN_PATH":      "plan",
				"TASK_BODY":      "task",
				"COMMIT_HISTORY": "history",
			},
			required: []string{"PLAN_PATH", "TASK_BODY", "COMMIT_HISTORY"},
		},
		{
			name:     "implementation review",
			template: ImplReviewTemplate,
			values: Values{
				"PLAN_PATH":     "plan",
				"PLAN_HASH":     "hash",
				"TASK_ID":       "task",
				"BASE_SHA":      "base",
				"CANDIDATE_SHA": "candidate",
				"REVIEW_PATH":   "review",
			},
			required: []string{
				"PLAN_PATH",
				"PLAN_HASH",
				"TASK_ID",
				"BASE_SHA",
				"CANDIDATE_SHA",
				"REVIEW_PATH",
			},
		},
		{
			name:     "fix",
			template: FixTemplate,
			values: Values{
				"PLAN_PATH":     "plan",
				"TASK_ID":       "task",
				"CANDIDATE_SHA": "candidate",
				"FINDINGS":      "findings",
			},
			required: []string{"PLAN_PATH", "TASK_ID", "CANDIDATE_SHA", "FINDINGS"},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, missing := range test.required {
				values := cloneValues(test.values)
				delete(values, missing)
				if _, err := Render(test.template, values); err == nil {
					t.Errorf("Render() accepted missing required variable %q", missing)
				} else if !strings.Contains(err.Error(), missing) {
					t.Errorf("Render() error %q does not identify missing %q", err, missing)
				}
			}
		})
	}
}

func TestRenderRejectsUnknownInputsAndTemplates(t *testing.T) {
	t.Parallel()

	_, err := Render(PlanTemplate, Values{
		"REQUEST":           "request",
		"CURRENT_PLAN_PATH": "",
		"PLAN_OUTPUT_PATH":  "plan",
		"QUESTIONS_PATH":    "questions",
		"UNKNOWN":           "value",
	})
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("Render() unknown variable error = %v", err)
	}

	if _, err := Source(Template("unknown")); err == nil {
		t.Fatal("Source() accepted an unknown template")
	}
	if _, err := Render(Template("unknown"), nil); err == nil {
		t.Fatal("Render() accepted an unknown template")
	}
}

func TestParseTemplateRejectsMalformedDirectives(t *testing.T) {
	t.Parallel()

	tests := []string{
		"plain }}",
		"{{VARIABLE",
		"{{lowercase}}",
		"{{#if}}",
		"{{#if CONDITION EXTRA}}body{{/if}}",
		"{{/if}}",
		"{{#if CONDITION}}body",
		"{{#if OUTER}}{{#if INNER}}body{{/if}}{{/if}}",
	}
	for _, source := range tests {
		source := source
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			if _, err := parseTemplate(source); err == nil {
				t.Errorf("parseTemplate(%q) unexpectedly succeeded", source)
			}
		})
	}
}

func TestRenderDoesNotReparseInsertedValues(t *testing.T) {
	t.Parallel()

	inserted := "literal {{PLAN_PATH}} {{#if STALL_NOTE}}inside{{/if}}"
	rendered := mustRender(t, ImplementationTemplate, Values{
		"PLAN_PATH":      "real-plan-path",
		"TASK_BODY":      inserted,
		"COMMIT_HISTORY": "history",
	})
	if !strings.Contains(rendered, inserted) {
		t.Fatalf("rendered prompt changed inserted template-like value:\n%s", rendered)
	}
	if got := strings.Count(rendered, "real-plan-path"); got != 1 {
		t.Fatalf("real PLAN_PATH occurrence count = %d; want 1", got)
	}
}

func mustRender(t *testing.T, name Template, values Values) string {
	t.Helper()
	rendered, err := Render(name, values)
	if err != nil {
		t.Fatalf("Render(%q): %v", name, err)
	}
	return rendered
}

func cloneValues(values Values) Values {
	cloned := make(Values, len(values))
	for name, value := range values {
		cloned[name] = value
	}
	return cloned
}

func assertOccurrences(t *testing.T, text string, expected map[string]int) {
	t.Helper()
	for needle, want := range expected {
		if got := strings.Count(text, needle); got != want {
			t.Errorf("occurrences of %q = %d; want %d", needle, got, want)
		}
	}
}

func assertAbsent(t *testing.T, text string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			t.Errorf("rendered prompt unexpectedly contains %q", needle)
		}
	}
}
