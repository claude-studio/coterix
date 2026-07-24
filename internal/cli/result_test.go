package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePlannerResultSelectsExactlyOneArtifact(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		adapter, runDir := newTestOutputAdapter(t)
		paths := ResultPaths{
			PlanOutput: filepath.Join(runDir, "plan.md"),
			Questions:  filepath.Join(runDir, "questions.md"),
		}
		if err := os.WriteFile(paths.PlanOutput, []byte("# plan"), 0o600); err != nil {
			t.Fatal(err)
		}
		validatorCalls := 0
		result, err := adapter.NewAttempt().ValidatePlannerResult(
			RolePlanWriter,
			paths,
			func(content []byte) error {
				validatorCalls++
				if string(content) != "# plan" {
					return errors.New("unexpected content")
				}
				return nil
			},
		)
		if err != nil {
			t.Fatalf("ValidatePlannerResult() error = %v", err)
		}
		plan, ok := result.(PlanFile)
		if !ok {
			t.Fatalf("result = %T, want PlanFile", result)
		}
		if string(plan.Content) != "# plan" || validatorCalls != 1 {
			t.Fatalf("plan = %#v, validator calls = %d", plan, validatorCalls)
		}
	})

	t.Run("questions", func(t *testing.T) {
		adapter, runDir := newTestOutputAdapter(t)
		paths := ResultPaths{
			PlanOutput: filepath.Join(runDir, "plan.md"),
			Questions:  filepath.Join(runDir, "questions.md"),
		}
		if err := os.WriteFile(paths.Questions, []byte("Which target?\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := adapter.NewAttempt().ValidatePlannerResult(
			RolePlanWriter,
			paths,
			func([]byte) error { return nil },
		)
		if err != nil {
			t.Fatalf("ValidatePlannerResult() error = %v", err)
		}
		if _, ok := result.(Questions); !ok {
			t.Fatalf("result = %T, want Questions", result)
		}
	})

	t.Run("both are ambiguous", func(t *testing.T) {
		adapter, runDir := newTestOutputAdapter(t)
		paths := ResultPaths{
			PlanOutput: filepath.Join(runDir, "plan.md"),
			Questions:  filepath.Join(runDir, "questions.md"),
		}
		for _, path := range []string{paths.PlanOutput, paths.Questions} {
			if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := adapter.NewAttempt().ValidatePlannerResult(
			RolePlanWriter,
			paths,
			func([]byte) error { return nil },
		); err == nil {
			t.Fatal("ValidatePlannerResult() accepted both artifacts")
		}
	})

	t.Run("neither is missing result", func(t *testing.T) {
		adapter, runDir := newTestOutputAdapter(t)
		paths := ResultPaths{
			PlanOutput: filepath.Join(runDir, "plan.md"),
			Questions:  filepath.Join(runDir, "questions.md"),
		}
		if _, err := adapter.NewAttempt().ValidatePlannerResult(
			RolePlanWriter,
			paths,
			func([]byte) error { return nil },
		); err == nil {
			t.Fatal("ValidatePlannerResult() accepted missing artifacts")
		}
	})
}

func TestValidateReviewResultAcceptsFixedSchemas(t *testing.T) {
	tests := []struct {
		name    string
		role    Role
		content string
		kind    ReviewKind
		clean   bool
	}{
		{
			name: "clean plan review",
			role: RolePlanReviewer,
			content: `{
				"schema_version": 1,
				"target_plan_hash": "plan-hash",
				"clean": true,
				"findings": []
			}`,
			kind:  ReviewKindPlan,
			clean: true,
		},
		{
			name: "minor plan finding remains clean",
			role: RolePlanReviewer,
			content: `{
				"schema_version": 1,
				"target_plan_hash": "plan-hash",
				"clean": true,
				"findings": [{
					"id": "f1",
					"severity": "minor",
					"task_id": null,
					"issue": "wording",
					"requested_change": "clarify"
				}]
			}`,
			kind:  ReviewKindPlan,
			clean: true,
		},
		{
			name: "blocking implementation finding is dirty",
			role: RoleImplReviewer,
			content: `{
				"schema_version": 1,
				"plan_hash": "plan-hash",
				"task_id": "T3",
				"candidate_sha": "candidate",
				"clean": false,
				"findings": [{
					"id": "f1",
					"severity": "major",
					"location": "internal/cli/result.go:1",
					"issue": "missing check",
					"requested_change": "add the check"
				}]
			}`,
			kind:  ReviewKindImplementation,
			clean: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, runDir := newTestOutputAdapter(t)
			path := filepath.Join(runDir, "review.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := adapter.NewAttempt().ValidateReviewResult(test.role, path)
			if err != nil {
				t.Fatalf("ValidateReviewResult() error = %v", err)
			}
			if result.Verdict.Kind != test.kind || result.Verdict.Clean != test.clean {
				t.Fatalf("verdict = %#v", result.Verdict)
			}
		})
	}
}

func TestValidateReviewResultRejectsSchemaAndConsistencyViolations(t *testing.T) {
	tests := []struct {
		name    string
		role    Role
		content string
	}{
		{
			name: "unknown field",
			role: RolePlanReviewer,
			content: `{
				"schema_version": 1,
				"target_plan_hash": "hash",
				"clean": true,
				"findings": [],
				"prose": "not allowed"
			}`,
		},
		{
			name: "clean with blocking finding",
			role: RolePlanReviewer,
			content: `{
				"schema_version": 1,
				"target_plan_hash": "hash",
				"clean": true,
				"findings": [{
					"id": "f1",
					"severity": "critical",
					"task_id": "T1",
					"issue": "broken",
					"requested_change": "fix"
				}]
			}`,
		},
		{
			name: "dirty without blocking finding",
			role: RoleImplReviewer,
			content: `{
				"schema_version": 1,
				"plan_hash": "hash",
				"task_id": "T1",
				"candidate_sha": "sha",
				"clean": false,
				"findings": [{
					"id": "f1",
					"severity": "minor",
					"location": null,
					"issue": "style",
					"requested_change": "polish"
				}]
			}`,
		},
		{
			name: "invalid severity",
			role: RolePlanReviewer,
			content: `{
				"schema_version": 1,
				"target_plan_hash": "hash",
				"clean": false,
				"findings": [{
					"id": "f1",
					"severity": "blocker",
					"task_id": null,
					"issue": "broken",
					"requested_change": "fix"
				}]
			}`,
		},
		{
			name: "wrong schema",
			role: RolePlanReviewer,
			content: `{
				"schema_version": 2,
				"target_plan_hash": "hash",
				"clean": true,
				"findings": []
			}`,
		},
		{
			name: "missing nullable field",
			role: RoleImplReviewer,
			content: `{
				"schema_version": 1,
				"plan_hash": "hash",
				"task_id": "T1",
				"candidate_sha": "sha",
				"clean": false,
				"findings": [{
					"id": "f1",
					"severity": "major",
					"issue": "broken",
					"requested_change": "fix"
				}]
			}`,
		},
		{
			name: "invalid implementation location",
			role: RoleImplReviewer,
			content: `{
				"schema_version": 1,
				"plan_hash": "hash",
				"task_id": "T1",
				"candidate_sha": "sha",
				"clean": false,
				"findings": [{
					"id": "f1",
					"severity": "major",
					"location": "garbage",
					"issue": "broken",
					"requested_change": "fix"
				}]
			}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, runDir := newTestOutputAdapter(t)
			path := filepath.Join(runDir, "review.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.NewAttempt().ValidateReviewResult(test.role, path); err == nil {
				t.Fatal("ValidateReviewResult() accepted an invalid verdict")
			}
		})
	}
}

func TestValidateCommittedCandidate(t *testing.T) {
	repo := t.TempDir()
	runTestGit(t, repo, "init", "-q")
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "tracked.txt")
	commitTestGit(t, repo, "base")
	base := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))

	if _, err := ValidateCommittedCandidate(
		context.Background(),
		RoleImplReviewer,
		repo,
		base,
	); err == nil {
		t.Fatal("ValidateCommittedCandidate() accepted a non-mutating role")
	}
	if _, err := ValidateCommittedCandidate(
		context.Background(),
		RoleImplWriter,
		repo,
		base,
	); err == nil {
		t.Fatal("ValidateCommittedCandidate() accepted unchanged HEAD")
	}

	if err := os.WriteFile(tracked, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "tracked.txt")
	commitTestGit(t, repo, "candidate")
	wantCandidate := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))

	result, err := ValidateCommittedCandidate(
		context.Background(),
		RoleImplWriter,
		repo,
		base,
	)
	if err != nil {
		t.Fatalf("ValidateCommittedCandidate() error = %v", err)
	}
	if result.BaseSHA != base || result.CandidateSHA != wantCandidate {
		t.Fatalf("candidate = %#v", result)
	}

	if err := os.WriteFile(tracked, []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCommittedCandidate(
		context.Background(),
		RoleFixer,
		repo,
		base,
	); err == nil {
		t.Fatal("ValidateCommittedCandidate() accepted a dirty worktree")
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func commitTestGit(t *testing.T, dir, message string) {
	t.Helper()
	runTestGit(
		t,
		dir,
		"-c",
		"user.name=Coterix Test",
		"-c",
		"user.email=coterix@example.invalid",
		"commit",
		"-qm",
		message,
	)
}
