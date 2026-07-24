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

	"github.com/ridenow/coterix/internal/pipeline"
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
