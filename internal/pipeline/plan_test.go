package pipeline

import (
	"strings"
	"testing"
)

func TestParsePlanPreservesDocumentOrderAndTaskBody(t *testing.T) {
	content := []byte(`
# Ship the feature

## T3: Add the parser
- [ ] Parse the fixed task syntax
Acceptance: Valid tasks are returned in document order
Verify: go test ./internal/pipeline/...

## T1: Connect the parser
- [ ] Materialize parsed task ids
Acceptance: State contains both tasks
Verify: Inspect state.json
`)

	plan, err := ParsePlan(content)
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if plan.Goal != "Ship the feature" {
		t.Fatalf("Goal = %q", plan.Goal)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("len(Tasks) = %d, want 2", len(plan.Tasks))
	}
	first := plan.Tasks[0]
	if first.ID != "T3" ||
		first.Title != "Add the parser" ||
		first.Work != "Parse the fixed task syntax" ||
		first.Acceptance != "Valid tasks are returned in document order" ||
		first.Verify != "go test ./internal/pipeline/..." {
		t.Fatalf("first task = %#v", first)
	}
	wantBody := strings.Join([]string{
		"## T3: Add the parser",
		"- [ ] Parse the fixed task syntax",
		"Acceptance: Valid tasks are returned in document order",
		"Verify: go test ./internal/pipeline/...",
	}, "\n")
	if first.Body != wantBody {
		t.Fatalf("Body = %q, want %q", first.Body, wantBody)
	}
	if plan.Tasks[1].ID != "T1" {
		t.Fatalf("task order = %q, %q", plan.Tasks[0].ID, plan.Tasks[1].ID)
	}
}

func TestParsePlanAcceptsCRLFAndNoSectionSeparator(t *testing.T) {
	content := []byte(
		"\r\n# Goal\r\n" +
			"## T1: First\r\n" +
			"- [ ] Work\r\n" +
			"Acceptance: Done\r\n" +
			"Verify: Test\r\n" +
			"## T4: Fourth\r\n" +
			"- [ ] More work\r\n" +
			"Acceptance: Also done\r\n" +
			"Verify: Another test\r\n\r\n",
	)

	plan, err := ParsePlan(content)
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if len(plan.Tasks) != 2 ||
		plan.Tasks[0].ID != "T1" ||
		plan.Tasks[1].ID != "T4" {
		t.Fatalf("Tasks = %#v", plan.Tasks)
	}
	if strings.Contains(plan.Tasks[0].Body, "\r") {
		t.Fatalf("Body did not normalize CRLF: %q", plan.Tasks[0].Body)
	}
}

func TestParsePlanDoesNotHaveScannerLineLimit(t *testing.T) {
	longWork := strings.Repeat("x", 128*1024)
	content := []byte(
		"# Goal\n" +
			"## T1: Long task\n" +
			"- [ ] " + longWork + "\n" +
			"Acceptance: Done\n" +
			"Verify: Test\n",
	)

	plan, err := ParsePlan(content)
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if plan.Tasks[0].Work != longWork {
		t.Fatalf("len(Work) = %d, want %d", len(plan.Tasks[0].Work), len(longWork))
	}
}

func TestParsePlanRejectsStructuralViolations(t *testing.T) {
	valid := strings.Join([]string{
		"# Goal",
		"## T1: Task",
		"- [ ] Work",
		"Acceptance: Done",
		"Verify: Test",
	}, "\n")
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "missing goal", content: strings.TrimPrefix(valid, "# Goal\n")},
		{name: "empty goal", content: strings.Replace(valid, "# Goal", "#   ", 1)},
		{name: "no tasks", content: "# Goal\n"},
		{name: "prose before task", content: strings.Replace(valid, "## T1", "intro\n## T1", 1)},
		{name: "unknown global heading", content: strings.Replace(valid, "## T1", "## Overview\n## T1", 1)},
		{name: "malformed task heading", content: strings.Replace(valid, "## T1:", "## T1 :", 1)},
		{name: "non numeric task id", content: strings.Replace(valid, "T1", "Tone", 1)},
		{name: "empty task title", content: strings.Replace(valid, "## T1: Task", "## T1:   ", 1)},
		{name: "blank after heading", content: strings.Replace(valid, "## T1: Task\n", "## T1: Task\n\n", 1)},
		{name: "checked checkbox", content: strings.Replace(valid, "- [ ]", "- [x]", 1)},
		{name: "empty checkbox", content: strings.Replace(valid, "- [ ] Work", "- [ ]   ", 1)},
		{name: "second checkbox", content: strings.Replace(valid, "Acceptance:", "- [ ] Other\nAcceptance:", 1)},
		{name: "missing acceptance", content: strings.Replace(valid, "Acceptance: Done\n", "", 1)},
		{name: "empty acceptance", content: strings.Replace(valid, "Acceptance: Done", "Acceptance:   ", 1)},
		{name: "missing verify", content: strings.Replace(valid, "\nVerify: Test", "", 1)},
		{name: "empty verify", content: strings.Replace(valid, "Verify: Test", "Verify:   ", 1)},
		{
			name: "reordered acceptance and verify",
			content: strings.Replace(
				valid,
				"Acceptance: Done\nVerify: Test",
				"Verify: Test\nAcceptance: Done",
				1,
			),
		},
		{
			name:    "duplicate acceptance",
			content: strings.Replace(valid, "Verify: Test", "Acceptance: Again\nVerify: Test", 1),
		},
		{name: "extra prose after task", content: valid + "\nfooter"},
		{name: "nested heading after task", content: valid + "\n### Notes"},
		{
			name: "duplicate id",
			content: valid + "\n" + strings.Join([]string{
				"## T1: Other",
				"- [ ] Other work",
				"Acceptance: Other done",
				"Verify: Other test",
			}, "\n"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParsePlan([]byte(test.content)); err == nil {
				t.Fatal("ParsePlan() unexpectedly accepted invalid plan")
			}
			if err := ValidatePlan([]byte(test.content)); err == nil {
				t.Fatal("ValidatePlan() unexpectedly accepted invalid plan")
			}
		})
	}
}
