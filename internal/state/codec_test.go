package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validStateJSON = `{
  "schema_version": 1,
  "phase": "planning",
  "plan_hash": null,
  "approved_plan_hash": null,
  "plan_round": 0,
  "pending_action": null,
  "task_order": ["T1"],
  "current_task_id": null,
  "tasks": {
    "T1": {
      "status": "open",
      "attempt": 0,
      "base_sha": null,
      "candidate_sha": null,
      "gate_result": null,
      "review_result": null
    }
  },
  "last_error": null
}`

func TestStateJSONRoundTripPreservesSchemaAndNulls(t *testing.T) {
	decoded, err := Parse([]byte(validStateJSON))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if decoded.SchemaVersion != SchemaVersion ||
		decoded.Phase != PhasePlanning ||
		len(decoded.TaskOrder) != 1 ||
		decoded.Tasks["T1"].Status != TaskOpen {
		t.Fatalf("decoded state = %#v", decoded)
	}

	content, err := Encode(decoded)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	for _, field := range []string{
		`"plan_hash": null`,
		`"approved_plan_hash": null`,
		`"pending_action": null`,
		`"current_task_id": null`,
		`"base_sha": null`,
		`"candidate_sha": null`,
		`"gate_result": null`,
		`"review_result": null`,
		`"last_error": null`,
	} {
		if !strings.Contains(string(content), field) {
			t.Fatalf("encoded state does not contain %s:\n%s", field, content)
		}
	}
	roundTripped, err := Parse(content)
	if err != nil {
		t.Fatalf("Parse(round trip) error = %v", err)
	}
	if !reflect.DeepEqual(roundTripped, decoded) {
		t.Fatalf("round trip = %#v, want %#v", roundTripped, decoded)
	}
}

func TestPendingActionsRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		action PendingAction
		task   bool
	}{
		{
			name: "plan question",
			action: PendingAction{
				Kind:        PendingPlanQuestion,
				ResumePhase: PhasePlanning,
				Prompt:      "Answer the planner",
			},
		},
		{
			name: "plan cap",
			action: PendingAction{
				Kind:        PendingPlanCap,
				ResumePhase: PhasePlanning,
				Prompt:      "Retry planning?",
			},
		},
		{
			name: "task cap",
			action: PendingAction{
				Kind:        PendingTaskCap,
				ResumePhase: PhaseImplementing,
				TaskID:      testString("T1"),
				Prompt:      "retry or abort",
			},
			task: true,
		},
		{
			name: "auth response free",
			action: PendingAction{
				Kind:        PendingAuth,
				ResumePhase: PhasePlanning,
				Prompt:      "Log in, then resume without a response",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := New()
			if test.task {
				current.Phase = PhaseImplementing
				addTestTask(current, "T1", TaskRepairing, 2)
			}
			if err := current.Pause(test.action); err != nil {
				t.Fatalf("Pause() error = %v", err)
			}
			content, err := Encode(current)
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			decoded, err := Parse(content)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if !reflect.DeepEqual(decoded, current) {
				t.Fatalf("round trip = %#v, want %#v", decoded, current)
			}
			if decoded.PendingAction.Response != nil {
				t.Fatal("pending response did not round-trip as null")
			}
		})
	}
}

func TestParseRejectsSchemaViolations(t *testing.T) {
	pausedWithUnknown := `{
	  "schema_version": 1,
	  "phase": "paused_for_input",
	  "plan_hash": null,
	  "approved_plan_hash": null,
	  "plan_round": 0,
	  "pending_action": {
	    "kind": "auth",
	    "resume_phase": "planning",
	    "task_id": null,
	    "prompt": "log in",
	    "response": null,
	    "extra": true
	  },
	  "task_order": [],
	  "current_task_id": null,
	  "tasks": {},
	  "last_error": null
	}`
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "unknown root field",
			content: strings.TrimSuffix(validStateJSON, "}") + `, "extra": true}`,
		},
		{
			name: "unknown task field",
			content: strings.Replace(
				validStateJSON,
				`"review_result": null`,
				`"review_result": null, "extra": true`,
				1,
			),
		},
		{
			name:    "unknown pending field",
			content: pausedWithUnknown,
		},
		{
			name: "missing counter",
			content: strings.Replace(
				validStateJSON,
				`  "plan_round": 0,`+"\n",
				"",
				1,
			),
		},
		{
			name: "missing nullable field",
			content: strings.Replace(
				validStateJSON,
				`  "plan_hash": null,`+"\n",
				"",
				1,
			),
		},
		{
			name: "missing task nullable field",
			content: strings.Replace(
				validStateJSON,
				`      "base_sha": null,`+"\n",
				"",
				1,
			),
		},
		{
			name: "wrong counter type",
			content: strings.Replace(
				validStateJSON,
				`"plan_round": 0`,
				`"plan_round": "0"`,
				1,
			),
		},
		{
			name: "invalid phase",
			content: strings.Replace(
				validStateJSON,
				`"phase": "planning"`,
				`"phase": "unknown"`,
				1,
			),
		},
		{
			name: "negative attempt",
			content: strings.Replace(
				validStateJSON,
				`"attempt": 0`,
				`"attempt": -1`,
				1,
			),
		},
		{
			name: "null task order",
			content: strings.Replace(
				validStateJSON,
				`"task_order": ["T1"]`,
				`"task_order": null`,
				1,
			),
		},
		{
			name:    "trailing JSON value",
			content: validStateJSON + `{}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse([]byte(test.content)); err == nil {
				t.Fatal("Parse() accepted invalid state JSON")
			}
		})
	}
}

func TestStateValidateRejectsTaskIndexAndPendingInconsistency(t *testing.T) {
	tests := []struct {
		name  string
		state *State
	}{
		{
			name: "duplicate task order",
			state: func() *State {
				current := New()
				addTestTask(current, "T1", TaskOpen, 0)
				current.TaskOrder = append(current.TaskOrder, "T1")
				return current
			}(),
		},
		{
			name: "unordered task map entry",
			state: func() *State {
				current := New()
				current.Tasks["T1"] = &TaskState{Status: TaskOpen}
				return current
			}(),
		},
		{
			name: "missing current task",
			state: func() *State {
				current := New()
				current.CurrentTaskID = testString("T1")
				return current
			}(),
		},
		{
			name: "paused without action",
			state: func() *State {
				current := New()
				current.Phase = PhasePausedForInput
				return current
			}(),
		},
		{
			name: "action while not paused",
			state: func() *State {
				current := New()
				current.PendingAction = &PendingAction{
					Kind:        PendingAuth,
					ResumePhase: PhasePlanning,
					Prompt:      "log in",
				}
				return current
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.state.Validate(); err == nil {
				t.Fatal("Validate() accepted an inconsistent state")
			}
		})
	}
}

func TestSaveAtomicallyReplacesAndLoadsState(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	current := New()
	if err := Save(path, current); err != nil {
		t.Fatalf("Save(initial) error = %v", err)
	}

	current.PlanRound = 1
	current.PlanHash = testString(strings.Repeat("a", 64))
	if err := Save(path, current); err != nil {
		t.Fatalf("Save(replacement) error = %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, current) {
		t.Fatalf("Load() = %#v, want %#v", loaded, current)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode = %o, want 600", info.Mode().Perm())
	}
	assertNoTemporaryStateFiles(t, directory)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current.PlanRound = -1
	if err := Save(path, current); err == nil {
		t.Fatal("Save() accepted invalid state")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed Save() changed the previous state.json")
	}
	assertNoTemporaryStateFiles(t, directory)
}

func TestSaveCleansTemporaryFileWhenRenameFails(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "state.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(target, New()); err == nil {
		t.Fatal("Save() unexpectedly replaced a non-empty directory")
	}
	if content, err := os.ReadFile(filepath.Join(target, "keep")); err != nil ||
		string(content) != "keep" {
		t.Fatalf("rename failure did not preserve target: %q, %v", content, err)
	}
	assertNoTemporaryStateFiles(t, directory)
}

func assertNoTemporaryStateFiles(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary state files remain: %v", matches)
	}
}

func addTestTask(
	current *State,
	taskID string,
	status TaskStatus,
	attempt int,
) {
	current.TaskOrder = append(current.TaskOrder, taskID)
	current.Tasks[taskID] = &TaskState{
		Status:  status,
		Attempt: attempt,
	}
	if current.CurrentTaskID == nil {
		current.CurrentTaskID = testString(taskID)
	}
}

func testString(value string) *string {
	return &value
}
