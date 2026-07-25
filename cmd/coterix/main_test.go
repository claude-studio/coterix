package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

type controlCall struct {
	command  string
	repoRoot string
	runID    string
	request  string
	response *string
}

type fakeControlPlane struct {
	calls        []controlCall
	status       pipeline.RunStatus
	statuses     []pipeline.RunStatus
	err          error
	resumeResult pipeline.RunStatus
}

type fakeRunDashboard struct {
	calls            []controlCall
	status           pipeline.RunStatus
	err              error
	interactive      bool
	interactiveCalls int
}

func (fake *fakeRunDashboard) Run(
	_ context.Context,
	repoRoot string,
	request string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, controlCall{
		command:  "run",
		repoRoot: repoRoot,
		request:  request,
	})
	return fake.status, fake.err
}

func copyResponseForTest(response *string) *string {
	if response == nil {
		return nil
	}
	copied := *response
	return &copied
}

func (fake *fakeRunDashboard) Open(
	_ context.Context,
	repoRoot string,
	runID string,
	command string,
	response *string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, controlCall{
		command:  command,
		repoRoot: repoRoot,
		runID:    runID,
		response: copyResponseForTest(response),
	})
	return fake.status, fake.err
}

func (fake *fakeRunDashboard) Interactive() bool {
	fake.interactiveCalls++
	return fake.interactive
}

func (fake *fakeControlPlane) Run(
	_ context.Context,
	repoRoot string,
	request string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, controlCall{
		command:  "run",
		repoRoot: repoRoot,
		request:  request,
	})
	return fake.status, fake.err
}

func (fake *fakeControlPlane) Approve(
	_ context.Context,
	repoRoot string,
	runID string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, controlCall{
		command:  "approve",
		repoRoot: repoRoot,
		runID:    runID,
	})
	return fake.status, fake.err
}

func (fake *fakeControlPlane) Reject(
	_ context.Context,
	repoRoot string,
	runID string,
	response string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, controlCall{
		command:  "reject",
		repoRoot: repoRoot,
		runID:    runID,
		response: stringPointer(response),
	})
	return fake.status, fake.err
}

func (fake *fakeControlPlane) Resume(
	_ context.Context,
	repoRoot string,
	runID string,
	response *string,
) (pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, controlCall{
		command:  "resume",
		repoRoot: repoRoot,
		runID:    runID,
		response: cloneString(response),
	})
	return fake.resumeResult, fake.err
}

func (fake *fakeControlPlane) Status(
	_ context.Context,
	repoRoot string,
	runID string,
) ([]pipeline.RunStatus, error) {
	fake.calls = append(fake.calls, controlCall{
		command:  "status",
		repoRoot: repoRoot,
		runID:    runID,
	})
	return fake.statuses, fake.err
}

func TestExecuteRunDistinguishesLiteralAndFileRequests(t *testing.T) {
	t.Run("existing filename remains literal", func(t *testing.T) {
		requestPath := filepath.Join(t.TempDir(), "request.txt")
		if err := os.WriteFile(requestPath, []byte("file content"), 0o600); err != nil {
			t.Fatal(err)
		}

		fake := &fakeControlPlane{}
		code, stdout, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"run",
			requestPath,
		)
		if code != 0 || stderr != "" {
			t.Fatalf("execute() code=%d stderr=%q", code, stderr)
		}
		assertSingleCall(t, fake, controlCall{
			command:  "run",
			repoRoot: "repo-root",
			request:  requestPath,
		})
		assertJSONObject(t, stdout)
	})

	t.Run("request file is explicit", func(t *testing.T) {
		requestPath := filepath.Join(t.TempDir(), "request.txt")
		const request = "build this\nwithout trimming\n"
		if err := os.WriteFile(requestPath, []byte(request), 0o600); err != nil {
			t.Fatal(err)
		}

		fake := &fakeControlPlane{}
		code, _, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"run",
			"--request-file",
			requestPath,
		)
		if code != 0 || stderr != "" {
			t.Fatalf("execute() code=%d stderr=%q", code, stderr)
		}
		assertSingleCall(t, fake, controlCall{
			command:  "run",
			repoRoot: "repo-root",
			request:  request,
		})
	})
}

func TestExecuteExactRunCommandReachesDashboard(t *testing.T) {
	controller := &fakeControlPlane{}
	dashboard := &fakeRunDashboard{
		status: pipeline.RunStatus{RunID: "run-from-dashboard"},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(
		context.Background(),
		controller,
		"repo-root",
		[]string{"run", "implement T9"},
		&stdout,
		&stderr,
		dashboard,
	)
	if code != 0 || stderr.String() != "" {
		t.Fatalf("execute() code=%d stderr=%q", code, stderr.String())
	}
	if len(controller.calls) != 0 {
		t.Fatalf("legacy JSON run path was called: %#v", controller.calls)
	}
	if len(dashboard.calls) != 1 {
		t.Fatalf("dashboard calls=%#v, want one", dashboard.calls)
	}
	got := dashboard.calls[0]
	if got.command != "run" ||
		got.repoRoot != "repo-root" ||
		got.request != "implement T9" {
		t.Fatalf("dashboard call=%#v", got)
	}
	var status pipeline.RunStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("headless dashboard output is not status JSON: %q: %v", stdout.String(), err)
	}
	if status.RunID != "run-from-dashboard" {
		t.Fatalf("dashboard status run_id=%q", status.RunID)
	}
	if dashboard.interactiveCalls != 1 {
		t.Fatalf(
			"headless run Interactive() calls=%d, want 1",
			dashboard.interactiveCalls,
		)
	}

	dashboard.interactive = true
	stdout.Reset()
	code = execute(
		context.Background(),
		controller,
		"repo-root",
		[]string{"run", "implement T9"},
		&stdout,
		&stderr,
		dashboard,
	)
	if code != 0 || stdout.Len() != 0 {
		t.Fatalf(
			"interactive dashboard code=%d stdout=%q",
			code,
			stdout.String(),
		)
	}
	if dashboard.interactiveCalls != 2 {
		t.Fatalf(
			"interactive run total Interactive() calls=%d, want 2",
			dashboard.interactiveCalls,
		)
	}
}

func TestExecuteRunRequiresExactlyOneRequestSource(t *testing.T) {
	requestPath := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(requestPath, []byte("request"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		args []string
		code int
	}{
		{name: "missing", args: []string{"run"}, code: 2},
		{
			name: "both",
			args: []string{"run", "--request-file", requestPath, "literal"},
			code: 2,
		},
		{
			name: "multiple positional",
			args: []string{"run", "one", "two"},
			code: 2,
		},
		{
			name: "missing file",
			args: []string{"run", "--request-file", filepath.Join(t.TempDir(), "missing")},
			code: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{}
			code, stdout, stderr := executeForTest(
				t,
				fake,
				"repo-root",
				test.args...,
			)
			if code != test.code || stdout != "" || stderr == "" {
				t.Fatalf(
					"execute() code=%d stdout=%q stderr=%q",
					code,
					stdout,
					stderr,
				)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("control plane was called: %#v", fake.calls)
			}
		})
	}
}

func TestExecuteRejectParsesRequiredResponseSources(t *testing.T) {
	responsePath := filepath.Join(t.TempDir(), "response.txt")
	const fileResponse = "revise the acceptance criteria\n"
	if err := os.WriteFile(responsePath, []byte(fileResponse), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		args     []string
		response string
	}{
		{
			name:     "inline",
			args:     []string{"reject", "run-1", "--response", "make it safer"},
			response: "make it safer",
		},
		{
			name:     "inline empty remains provided",
			args:     []string{"reject", "run-1", "--response", ""},
			response: "",
		},
		{
			name:     "file",
			args:     []string{"reject", "run-1", "--response-file", responsePath},
			response: fileResponse,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{}
			code, _, stderr := executeForTest(
				t,
				fake,
				"repo-root",
				test.args...,
			)
			if code != 0 || stderr != "" {
				t.Fatalf("execute() code=%d stderr=%q", code, stderr)
			}
			assertSingleCall(t, fake, controlCall{
				command:  "reject",
				repoRoot: "repo-root",
				runID:    "run-1",
				response: stringPointer(test.response),
			})
		})
	}
}

func TestExecuteRejectRejectsMissingAmbiguousAndUnreadableResponses(t *testing.T) {
	responsePath := filepath.Join(t.TempDir(), "response.txt")
	if err := os.WriteFile(responsePath, []byte("response"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		args []string
		code int
	}{
		{name: "missing run id", args: []string{"reject"}, code: 2},
		{name: "missing response", args: []string{"reject", "run-1"}, code: 2},
		{
			name: "both response sources",
			args: []string{
				"reject",
				"run-1",
				"--response",
				"inline",
				"--response-file",
				responsePath,
			},
			code: 2,
		},
		{
			name: "unreadable response file",
			args: []string{
				"reject",
				"run-1",
				"--response-file",
				filepath.Join(t.TempDir(), "missing"),
			},
			code: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{}
			code, stdout, stderr := executeForTest(
				t,
				fake,
				"repo-root",
				test.args...,
			)
			if code != test.code || stdout != "" || stderr == "" {
				t.Fatalf(
					"execute() code=%d stdout=%q stderr=%q",
					code,
					stdout,
					stderr,
				)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("control plane was called: %#v", fake.calls)
			}
		})
	}
}

func TestExecuteResumePreservesOptionalResponsePresence(t *testing.T) {
	responsePath := filepath.Join(t.TempDir(), "response.txt")
	const fileResponse = "retry\n"
	if err := os.WriteFile(responsePath, []byte(fileResponse), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		args     []string
		response *string
	}{
		{
			name: "absent remains nil",
			args: []string{"resume", "run-1"},
		},
		{
			name:     "inline empty remains non-nil",
			args:     []string{"resume", "run-1", "--response", ""},
			response: stringPointer(""),
		},
		{
			name:     "inline",
			args:     []string{"resume", "run-1", "--response", "retry"},
			response: stringPointer("retry"),
		},
		{
			name:     "file",
			args:     []string{"resume", "run-1", "--response-file", responsePath},
			response: stringPointer(fileResponse),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{}
			code, _, stderr := executeForTest(
				t,
				fake,
				"repo-root",
				test.args...,
			)
			if code != 0 || stderr != "" {
				t.Fatalf("execute() code=%d stderr=%q", code, stderr)
			}
			assertSingleCall(t, fake, controlCall{
				command:  "resume",
				repoRoot: "repo-root",
				runID:    "run-1",
				response: test.response,
			})
		})
	}
}

func TestExecuteResumeRejectsAmbiguousSourcesButDefersKindRules(t *testing.T) {
	responsePath := filepath.Join(t.TempDir(), "response.txt")
	if err := os.WriteFile(responsePath, []byte("retry"), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &fakeControlPlane{}
	code, stdout, stderr := executeForTest(
		t,
		fake,
		"repo-root",
		"resume",
		"run-1",
		"--response",
		"retry",
		"--response-file",
		responsePath,
	)
	if code != 2 || stdout != "" || stderr == "" {
		t.Fatalf(
			"execute() code=%d stdout=%q stderr=%q",
			code,
			stdout,
			stderr,
		)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("control plane was called: %#v", fake.calls)
	}

	kindErr := errors.New("auth resume forbids a response")
	fake = &fakeControlPlane{err: kindErr}
	code, stdout, stderr = executeForTest(
		t,
		fake,
		"repo-root",
		"resume",
		"run-1",
		"--response",
		"present",
	)
	if code != 1 || stdout != "" || !strings.Contains(stderr, kindErr.Error()) {
		t.Fatalf(
			"execute() code=%d stdout=%q stderr=%q",
			code,
			stdout,
			stderr,
		)
	}
	if len(fake.calls) != 1 || fake.calls[0].response == nil {
		t.Fatalf("CLI did not delegate kind validation: %#v", fake.calls)
	}
}

func TestExecuteApproveAndStatusCardinality(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		fake := &fakeControlPlane{}
		code, stdout, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"approve",
			"run-1",
		)
		if code != 0 || stderr != "" {
			t.Fatalf("execute() code=%d stderr=%q", code, stderr)
		}
		assertSingleCall(t, fake, controlCall{
			command:  "approve",
			repoRoot: "repo-root",
			runID:    "run-1",
		})
		assertJSONObject(t, stdout)
	})

	for _, args := range [][]string{
		{"approve"},
		{"approve", "one", "two"},
	} {
		fake := &fakeControlPlane{}
		code, stdout, stderr := executeForTest(t, fake, "repo-root", args...)
		if code != 2 || stdout != "" || stderr == "" || len(fake.calls) != 0 {
			t.Fatalf(
				"execute(%q) code=%d stdout=%q stderr=%q calls=%#v",
				args,
				code,
				stdout,
				stderr,
				fake.calls,
			)
		}
	}

	t.Run("status all", func(t *testing.T) {
		fake := &fakeControlPlane{
			statuses: []pipeline.RunStatus{{}, {}},
		}
		code, stdout, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"status",
		)
		if code != 0 || stderr != "" {
			t.Fatalf("execute() code=%d stderr=%q", code, stderr)
		}
		assertSingleCall(t, fake, controlCall{
			command:  "status",
			repoRoot: "repo-root",
		})
		var statuses []json.RawMessage
		if err := json.Unmarshal([]byte(stdout), &statuses); err != nil {
			t.Fatalf("status output is not a JSON array: %q: %v", stdout, err)
		}
		if len(statuses) != 2 {
			t.Fatalf("status output length=%d want=2", len(statuses))
		}
	})

	t.Run("status one", func(t *testing.T) {
		fake := &fakeControlPlane{}
		code, _, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"status",
			"run-1",
		)
		if code != 0 || stderr != "" {
			t.Fatalf("execute() code=%d stderr=%q", code, stderr)
		}
		assertSingleCall(t, fake, controlCall{
			command:  "status",
			repoRoot: "repo-root",
			runID:    "run-1",
		})
	})

	t.Run("status too many", func(t *testing.T) {
		fake := &fakeControlPlane{}
		code, stdout, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"status",
			"one",
			"two",
		)
		if code != 2 || stdout != "" || stderr == "" || len(fake.calls) != 0 {
			t.Fatalf(
				"execute() code=%d stdout=%q stderr=%q calls=%#v",
				code,
				stdout,
				stderr,
				fake.calls,
			)
		}
	})
}

func TestExecuteJSONOutputAndExitCodes(t *testing.T) {
	t.Run("nil status slice is an empty JSON array", func(t *testing.T) {
		fake := &fakeControlPlane{}
		code, stdout, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"status",
		)
		if code != 0 || stdout != "[]\n" || stderr != "" {
			t.Fatalf(
				"execute() code=%d stdout=%q stderr=%q",
				code,
				stdout,
				stderr,
			)
		}
	})

	t.Run("core failure", func(t *testing.T) {
		coreErr := errors.New("core rejected the operation")
		fake := &fakeControlPlane{err: coreErr}
		code, stdout, stderr := executeForTest(
			t,
			fake,
			"repo-root",
			"approve",
			"run-1",
		)
		if code != 1 || stdout != "" || !strings.Contains(stderr, coreErr.Error()) {
			t.Fatalf(
				"execute() code=%d stdout=%q stderr=%q",
				code,
				stdout,
				stderr,
			)
		}
	})

	for _, args := range [][]string{
		nil,
		{"unknown"},
	} {
		fake := &fakeControlPlane{}
		code, stdout, stderr := executeForTest(t, fake, "repo-root", args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "Usage:") {
			t.Fatalf(
				"execute(%q) code=%d stdout=%q stderr=%q",
				args,
				code,
				stdout,
				stderr,
			)
		}
	}
}

func TestExecuteControlCommandsPreserveExactHeadlessJSON(t *testing.T) {
	responsePath := filepath.Join(t.TempDir(), "response.txt")
	const fileResponse = "response from file\n"
	if err := os.WriteFile(
		responsePath,
		[]byte(fileResponse),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	status := pipeline.RunStatus{
		RunID: "run-json",
		Phase: state.PhaseDone,
	}
	const statusJSON = "{\"run_id\":\"run-json\",\"phase\":\"done\"," +
		"\"plan_hash\":null,\"approved_plan_hash\":null,\"plan_round\":0," +
		"\"pending_action\":null,\"task_order\":null," +
		"\"current_task_id\":null,\"tasks\":null,\"last_error\":null}\n"
	const statusesJSON = "[{\"run_id\":\"run-json\",\"phase\":\"done\"," +
		"\"plan_hash\":null,\"approved_plan_hash\":null,\"plan_round\":0," +
		"\"pending_action\":null,\"task_order\":null," +
		"\"current_task_id\":null,\"tasks\":null,\"last_error\":null}]\n"

	tests := []struct {
		name       string
		args       []string
		wantOutput string
		wantCall   controlCall
	}{
		{
			name:       "approve",
			args:       []string{"approve", "run-json"},
			wantOutput: statusJSON,
			wantCall: controlCall{
				command:  "approve",
				repoRoot: "repo-root",
				runID:    "run-json",
			},
		},
		{
			name: "reject inline",
			args: []string{
				"reject",
				"run-json",
				"--response",
				"revise it",
			},
			wantOutput: statusJSON,
			wantCall: controlCall{
				command:  "reject",
				repoRoot: "repo-root",
				runID:    "run-json",
				response: stringPointer("revise it"),
			},
		},
		{
			name: "reject file",
			args: []string{
				"reject",
				"run-json",
				"--response-file",
				responsePath,
			},
			wantOutput: statusJSON,
			wantCall: controlCall{
				command:  "reject",
				repoRoot: "repo-root",
				runID:    "run-json",
				response: stringPointer(fileResponse),
			},
		},
		{
			name:       "status all",
			args:       []string{"status"},
			wantOutput: statusesJSON,
			wantCall: controlCall{
				command:  "status",
				repoRoot: "repo-root",
			},
		},
		{
			name:       "status one",
			args:       []string{"status", "run-json"},
			wantOutput: statusesJSON,
			wantCall: controlCall{
				command:  "status",
				repoRoot: "repo-root",
				runID:    "run-json",
			},
		},
		{
			name:       "resume without response",
			args:       []string{"resume", "run-json"},
			wantOutput: statusJSON,
			wantCall: controlCall{
				command:  "resume",
				repoRoot: "repo-root",
				runID:    "run-json",
			},
		},
		{
			name: "resume inline",
			args: []string{
				"resume",
				"run-json",
				"--response",
				"retry",
			},
			wantOutput: statusJSON,
			wantCall: controlCall{
				command:  "resume",
				repoRoot: "repo-root",
				runID:    "run-json",
				response: stringPointer("retry"),
			},
		},
		{
			name: "resume file",
			args: []string{
				"resume",
				"run-json",
				"--response-file",
				responsePath,
			},
			wantOutput: statusJSON,
			wantCall: controlCall{
				command:  "resume",
				repoRoot: "repo-root",
				runID:    "run-json",
				response: stringPointer(fileResponse),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{
				status:       status,
				statuses:     []pipeline.RunStatus{status},
				resumeResult: status,
			}
			dashboard := &fakeRunDashboard{interactive: false}
			code, stdout, stderr := executeWithDashboardForTest(
				t,
				fake,
				dashboard,
				"repo-root",
				test.args...,
			)
			if code != 0 ||
				stdout != test.wantOutput ||
				stderr != "" {
				t.Fatalf(
					"execute() code=%d stdout=%q stderr=%q",
					code,
					stdout,
					stderr,
				)
			}
			assertSingleCall(t, fake, test.wantCall)
			if len(dashboard.calls) != 0 {
				t.Fatalf("control command reached dashboard.Run: %#v", dashboard.calls)
			}
		})
	}
}

// Only `status` still renders a one-shot snapshot on a TTY: approve/reject/resume now
// open the live dashboard, because they run for minutes and used to print nothing at
// all until they finished (T15 W2). Their entry is covered separately.
func TestExecuteStatusRendersInteractiveSnapshots(t *testing.T) {
	status := pipeline.RunStatus{
		RunID: "run-tty",
		Phase: state.PhaseDone,
	}
	tests := []struct {
		name        string
		args        []string
		wantCall    controlCall
		wantTable   bool
		wantDetails bool
	}{
		{
			name: "bare status table with one run",
			args: []string{"status"},
			wantCall: controlCall{
				command:  "status",
				repoRoot: "repo-root",
			},
			wantTable: true,
		},
		{
			name: "status id detail",
			args: []string{"status", "run-tty"},
			wantCall: controlCall{
				command:  "status",
				repoRoot: "repo-root",
				runID:    "run-tty",
			},
			wantDetails: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{
				status:       status,
				statuses:     []pipeline.RunStatus{status},
				resumeResult: status,
			}
			dashboard := &fakeRunDashboard{interactive: true}
			code, stdout, stderr := executeWithDashboardForTest(
				t,
				fake,
				dashboard,
				"repo-root",
				test.args...,
			)
			if code != 0 || stderr != "" || stdout == "" {
				t.Fatalf(
					"execute() code=%d stdout=%q stderr=%q",
					code,
					stdout,
					stderr,
				)
			}
			if json.Valid([]byte(stdout)) {
				t.Fatalf("interactive output remained JSON: %q", stdout)
			}
			plain := ansi.Strip(stdout)
			if !strings.Contains(plain, "██████") {
				t.Fatalf("snapshot lacks block banner:\n%s", plain)
			}
			if test.wantTable && (!strings.Contains(plain, "run_id") ||
				strings.Contains(plain, "╱╱╱ RUN")) {
				t.Fatalf("bare status did not render a table:\n%s", plain)
			}
			if test.wantDetails && !strings.Contains(plain, "╱╱╱ RUN") {
				t.Fatalf("control result did not render details:\n%s", plain)
			}
			assertSingleCall(t, fake, test.wantCall)
			if len(dashboard.calls) != 0 {
				t.Fatalf("control command reached dashboard.Run: %#v", dashboard.calls)
			}
		})
	}
}

func TestExecuteInteractiveControlErrorsAreNotIntercepted(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "reject unknown flag",
			args:       []string{"reject", "run-tty", "--unknown"},
			wantStderr: "coterix: reject: flag provided but not defined: -unknown\n" + usageText,
		},
		{
			name:       "unknown command",
			args:       []string{"unknown"},
			wantStderr: "coterix: unknown command \"unknown\"\n" + usageText,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{}
			dashboard := &fakeRunDashboard{interactive: true}
			code, stdout, stderr := executeWithDashboardForTest(
				t,
				fake,
				dashboard,
				"repo-root",
				test.args...,
			)
			if code != 2 ||
				stdout != "" ||
				stderr != test.wantStderr {
				t.Fatalf(
					"execute() code=%d stdout=%q stderr=%q",
					code,
					stdout,
					stderr,
				)
			}
			if len(fake.calls) != 0 || len(dashboard.calls) != 0 {
				t.Fatalf(
					"invalid argv reached a control path: control=%#v dashboard=%#v",
					fake.calls,
					dashboard.calls,
				)
			}
		})
	}

	// The dashboard now owns approve on a TTY, so a failure surfaces from `ui.Open`
	// rather than from the controller — it must still exit 1 with the bare message and
	// no usage text (T15 W2/W4).
	t.Run("dashboard entry failure exits 1", func(t *testing.T) {
		openErr := errors.New("ui: open run: no such run")
		fake := &fakeControlPlane{}
		dashboard := &fakeRunDashboard{interactive: true, err: openErr}
		code, stdout, stderr := executeWithDashboardForTest(
			t,
			fake,
			dashboard,
			"repo-root",
			"approve",
			"run-tty",
		)
		if code != 1 || stdout != "" ||
			stderr != "coterix: "+openErr.Error()+"\n" {
			t.Fatalf("execute() code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if len(dashboard.calls) != 1 {
			t.Fatalf("approve did not reach the dashboard: %#v", dashboard.calls)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("approve also called the controller: %#v", fake.calls)
		}
	})
}

func executeForTest(
	t *testing.T,
	controller controlPlane,
	repoRoot string,
	args ...string,
) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(
		context.Background(),
		controller,
		repoRoot,
		args,
		&stdout,
		&stderr,
	)
	return code, stdout.String(), stderr.String()
}

func executeWithDashboardForTest(
	t *testing.T,
	controller controlPlane,
	dashboard runDashboard,
	repoRoot string,
	args ...string,
) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(
		context.Background(),
		controller,
		repoRoot,
		args,
		&stdout,
		&stderr,
		dashboard,
	)
	return code, stdout.String(), stderr.String()
}

func assertSingleCall(
	t *testing.T,
	fake *fakeControlPlane,
	want controlCall,
) {
	t.Helper()
	if len(fake.calls) != 1 {
		t.Fatalf("control calls=%#v, want one call", fake.calls)
	}
	got := fake.calls[0]
	if got.command != want.command ||
		got.repoRoot != want.repoRoot ||
		got.runID != want.runID ||
		got.request != want.request ||
		!equalStrings(got.response, want.response) {
		t.Fatalf("control call=%#v, want %#v", got, want)
	}
}

func assertJSONObject(t *testing.T, output string) {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("output is not a JSON object: %q: %v", output, err)
	}
}

func stringPointer(value string) *string {
	return &value
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func equalStrings(left, right *string) bool {
	switch {
	case left == nil || right == nil:
		return left == nil && right == nil
	default:
		return *left == *right
	}
}

// A help request is a question, not a mistake: stdout and exit 0. Before T15 W3
// `coterix --help` fell through to "unknown command" and left usage on stderr with
// exit 2, which reads as a failure to a shell script.
func TestExecuteTopLevelHelpGoesToStdoutWithExitZero(t *testing.T) {
	for _, argument := range []string{"--help", "-h", "help"} {
		t.Run(argument, func(t *testing.T) {
			code, stdout, stderr := executeForTest(
				t,
				&fakeControlPlane{},
				t.TempDir(),
				argument,
			)
			if code != 0 {
				t.Fatalf("exit=%d want 0 (stderr=%q)", code, stderr)
			}
			if stderr != "" {
				t.Fatalf("help wrote to stderr: %q", stderr)
			}
			if !strings.Contains(stdout, "Usage:") {
				t.Fatalf("usage did not reach stdout: %q", stdout)
			}
			for _, command := range []string{
				"run",
				"approve",
				"reject",
				"status",
				"resume",
			} {
				if !strings.Contains(stdout, command) {
					t.Fatalf("usage omits %q:\n%s", command, stdout)
				}
			}
		})
	}
}

// The neighbours of that decision, pinned so they cannot drift: bare `coterix` is a
// usage error, and a subcommand's own `--help` keeps its current stderr/exit 2
// behaviour (T15 W3 · R5 — unifying those would mean touching every flagset).
func TestHelpDoesNotChangeBareInvocationOrSubcommandHelp(t *testing.T) {
	t.Run("bare invocation stays a usage error", func(t *testing.T) {
		code, stdout, stderr := executeForTest(t, &fakeControlPlane{}, t.TempDir())
		if code != 2 {
			t.Fatalf("exit=%d want 2", code)
		}
		if stdout != "" {
			t.Fatalf("a usage error wrote to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "Usage:") {
			t.Fatalf("usage did not reach stderr: %q", stderr)
		}
	})

	t.Run("subcommand help stays on stderr", func(t *testing.T) {
		code, stdout, stderr := executeForTest(
			t,
			&fakeControlPlane{},
			t.TempDir(),
			"run",
			"--help",
		)
		if code != 2 {
			t.Fatalf("exit=%d want 2 (unchanged)", code)
		}
		if stdout != "" {
			t.Fatalf("subcommand help wrote to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "Usage:") {
			t.Fatalf("subcommand help lost its usage text: %q", stderr)
		}
	})
}

// approve/reject/resume enter the live dashboard on a TTY, carrying the parsed run id
// and response. Before T15 they ran to completion with no output and then printed one
// snapshot (T15 W2).
func TestExecuteControlCommandsEnterTheDashboardOnATTY(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want controlCall
	}{
		{
			name: "approve",
			args: []string{"approve", "run-tty"},
			want: controlCall{
				command:  "approve",
				repoRoot: "repo-root",
				runID:    "run-tty",
			},
		},
		{
			name: "reject with a response",
			args: []string{"reject", "run-tty", "--response", "revise"},
			want: controlCall{
				command:  "reject",
				repoRoot: "repo-root",
				runID:    "run-tty",
				response: stringPointer("revise"),
			},
		},
		{
			// The reversal of T11 f1's argv objection: a bare reject is legal on a TTY
			// and the dashboard asks for the feedback (R1 option a).
			name: "bare reject",
			args: []string{"reject", "run-tty"},
			want: controlCall{
				command:  "reject",
				repoRoot: "repo-root",
				runID:    "run-tty",
			},
		},
		{
			name: "resume with a response",
			args: []string{"resume", "run-tty", "--response", "retry"},
			want: controlCall{
				command:  "resume",
				repoRoot: "repo-root",
				runID:    "run-tty",
				response: stringPointer("retry"),
			},
		},
		{
			name: "bare resume",
			args: []string{"resume", "run-tty"},
			want: controlCall{
				command:  "resume",
				repoRoot: "repo-root",
				runID:    "run-tty",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeControlPlane{}
			dashboard := &fakeRunDashboard{
				interactive: true,
				status:      pipeline.RunStatus{RunID: "run-tty"},
			}
			code, _, stderr := executeWithDashboardForTest(
				t,
				fake,
				dashboard,
				"repo-root",
				test.args...,
			)
			if code != 0 || stderr != "" {
				t.Fatalf("execute() code=%d stderr=%q", code, stderr)
			}
			if len(dashboard.calls) != 1 {
				t.Fatalf("dashboard calls=%#v", dashboard.calls)
			}
			got := dashboard.calls[0]
			if got.command != test.want.command ||
				got.repoRoot != test.want.repoRoot ||
				got.runID != test.want.runID {
				t.Fatalf("dashboard call=%#v want=%#v", got, test.want)
			}
			if (got.response == nil) != (test.want.response == nil) ||
				(got.response != nil && *got.response != *test.want.response) {
				t.Fatalf("response=%v want=%v", got.response, test.want.response)
			}
			// The headless controller must not be touched on this path.
			if len(fake.calls) != 0 {
				t.Fatalf("the controller was also called: %#v", fake.calls)
			}
		})
	}
}

// The headless contract is untouched: without a TTY these commands keep the dispatch
// path, and a bare reject is still a usage error there (T15 W2).
func TestControlCommandsWithoutATTYKeepTheHeadlessContract(t *testing.T) {
	t.Run("approve dispatches to the controller", func(t *testing.T) {
		fake := &fakeControlPlane{status: pipeline.RunStatus{RunID: "run-h"}}
		dashboard := &fakeRunDashboard{interactive: false}
		code, stdout, stderr := executeWithDashboardForTest(
			t,
			fake,
			dashboard,
			"repo-root",
			"approve",
			"run-h",
		)
		if code != 0 || stderr != "" {
			t.Fatalf("execute() code=%d stderr=%q", code, stderr)
		}
		if !json.Valid([]byte(stdout)) {
			t.Fatalf("headless output is not JSON: %q", stdout)
		}
		if len(dashboard.calls) != 0 {
			t.Fatalf("headless approve reached the dashboard: %#v", dashboard.calls)
		}
		assertSingleCall(t, fake, controlCall{
			command:  "approve",
			repoRoot: "repo-root",
			runID:    "run-h",
		})
	})

	t.Run("bare reject is still a usage error", func(t *testing.T) {
		fake := &fakeControlPlane{}
		dashboard := &fakeRunDashboard{interactive: false}
		code, stdout, stderr := executeWithDashboardForTest(
			t,
			fake,
			dashboard,
			"repo-root",
			"reject",
			"run-h",
		)
		if code != 2 {
			t.Fatalf("exit=%d want 2", code)
		}
		if stdout != "" {
			t.Fatalf("a usage error wrote to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "exactly one of --response") {
			t.Fatalf("stderr=%q", stderr)
		}
		if len(dashboard.calls) != 0 || len(fake.calls) != 0 {
			t.Fatal("a rejected argv reached the dashboard or the controller")
		}
	})
}
