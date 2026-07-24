package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ridenow/coterix/internal/runner"
)

func TestPolicyForRoleMapsPreparationRules(t *testing.T) {
	adapter, runDir := newTestOutputAdapter(t)
	plan := filepath.Join(runDir, "plan.md")
	next := filepath.Join(runDir, "plan.next.md")
	questions := filepath.Join(runDir, "questions.md")
	review := filepath.Join(runDir, "review.json")
	if err := os.WriteFile(plan, []byte("canonical"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		role  Role
		paths ResultPaths
		want  OutputPolicy
	}{
		{
			name: "initial planner clears both possible results",
			role: RolePlanWriter,
			paths: ResultPaths{
				PlanOutput: plan,
				Questions:  questions,
			},
			want: OutputPolicy{
				Effect:      runner.EffectArtifactOnly,
				OutputPaths: []string{plan, questions},
			},
		},
		{
			name: "reviser preserves canonical plan",
			role: RolePlanReviser,
			paths: ResultPaths{
				PlanOutput:  next,
				Questions:   questions,
				CurrentPlan: plan,
			},
			want: OutputPolicy{
				Effect:         runner.EffectArtifactOnly,
				OutputPaths:    []string{next, questions},
				CanonicalPaths: []string{plan},
			},
		},
		{
			name:  "reviewer clears JSON result",
			role:  RolePlanReviewer,
			paths: ResultPaths{Review: review},
			want: OutputPolicy{
				Effect:      runner.EffectReadOnly,
				OutputPaths: []string{review},
			},
		},
		{
			name: "mutating writer has no file result",
			role: RoleImplWriter,
			want: OutputPolicy{Effect: runner.EffectMutating},
		},
		{
			name: "fixer has no file result",
			role: RoleFixer,
			want: OutputPolicy{Effect: runner.EffectMutating},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := adapter.PolicyForRole(test.role, test.paths)
			if err != nil {
				t.Fatalf("PolicyForRole() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("PolicyForRole() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestOutputAdapterRejectsUnsafePaths(t *testing.T) {
	adapter, runDir := newTestOutputAdapter(t)
	repoRoot := filepath.Clean(filepath.Join(runDir, "..", "..", ".."))
	outside := filepath.Join(repoRoot, "outside.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prefixRun := filepath.Join(repoRoot, ".coterix", "runs", "run-12")
	if err := os.MkdirAll(prefixRun, 0o700); err != nil {
		t.Fatal(err)
	}
	prefixPath := filepath.Join(prefixRun, "review.json")

	wrongExtension := filepath.Join(runDir, "review.txt")
	relative := filepath.Join("relative", "review.json")
	tests := []struct {
		name string
		path string
	}{
		{name: "outside", path: outside},
		{name: "prefix trap", path: prefixPath},
		{name: "relative", path: relative},
		{name: "wrong extension", path: wrongExtension},
		{name: "unclean traversal", path: runDir + string(filepath.Separator) + "x" + string(filepath.Separator) + ".." + string(filepath.Separator) + "review.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := adapter.PolicyForRole(
				RolePlanReviewer,
				ResultPaths{Review: test.path},
			); err == nil {
				t.Fatal("PolicyForRole() accepted an unsafe path")
			}
		})
	}
}

func TestOutputAdapterRejectsSymlinkAndNonRegularResults(t *testing.T) {
	adapter, runDir := newTestOutputAdapter(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.json")
	if err := os.WriteFile(outsideFile, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	finalSymlink := filepath.Join(runDir, "final.json")
	if err := os.Symlink(outsideFile, finalSymlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := adapter.NewAttempt().Consume(finalSymlink, ".json"); err == nil {
		t.Fatal("Consume() accepted a final symlink")
	}

	parentSymlink := filepath.Join(runDir, "linked")
	if err := os.Symlink(outsideDir, parentSymlink); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.NewAttempt().Consume(
		filepath.Join(parentSymlink, "outside.json"),
		".json",
	); err == nil {
		t.Fatal("Consume() accepted a symlink parent")
	}

	directoryResult := filepath.Join(runDir, "directory.json")
	if err := os.Mkdir(directoryResult, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.NewAttempt().Consume(directoryResult, ".json"); err == nil {
		t.Fatal("Consume() accepted a directory")
	}
}

func TestOutputAdapterRejectsRunDirectoryReplacedBySymlink(t *testing.T) {
	adapter, runDir := newTestOutputAdapter(t)
	movedRunDir := runDir + ".moved"
	if err := os.Rename(runDir, movedRunDir); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outsideResult := filepath.Join(outsideDir, "review.json")
	if err := os.WriteFile(outsideResult, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, runDir); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}

	if _, err := adapter.NewAttempt().Consume(
		filepath.Join(runDir, "review.json"),
		".json",
	); err == nil {
		t.Fatal("Consume() accepted a result after run directory symlink replacement")
	}
}

func TestOutputAttemptEnforcesSizeAndSingleConsumption(t *testing.T) {
	adapter, runDir := newTestOutputAdapterWithLimit(t, 4)
	exact := filepath.Join(runDir, "exact.json")
	if err := os.WriteFile(exact, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	attempt := adapter.NewAttempt()
	content, err := attempt.Consume(exact, ".json")
	if err != nil {
		t.Fatalf("Consume() error = %v", err)
	}
	if string(content) != "1234" {
		t.Fatalf("Consume() = %q, want exact content", content)
	}
	if _, err := attempt.Consume(exact, ".json"); err == nil {
		t.Fatal("Consume() allowed a second read in one attempt")
	}

	oversized := filepath.Join(runDir, "oversized.json")
	if err := os.WriteFile(oversized, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.NewAttempt().Consume(oversized, ".json"); err == nil {
		t.Fatal("Consume() accepted an oversized result")
	}
}

func TestNewOutputAdapterRejectsInvalidRunRoot(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := NewOutputAdapter(repoRoot, "../escape"); err == nil {
		t.Fatal("NewOutputAdapter() accepted an invalid run id")
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, ".coterix"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repoRoot, ".coterix", "runs")); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := NewOutputAdapter(repoRoot, "run-1"); err == nil {
		t.Fatal("NewOutputAdapter() accepted a symlink run root")
	}
}

func newTestOutputAdapter(t *testing.T) (*OutputAdapter, string) {
	t.Helper()
	return newTestOutputAdapterWithLimit(t, DefaultMaxOutputBytes)
}

func newTestOutputAdapterWithLimit(
	t *testing.T,
	maxBytes int64,
) (*OutputAdapter, string) {
	t.Helper()
	repoRoot := t.TempDir()
	runID := "run-1"
	runDir := filepath.Join(repoRoot, ".coterix", "runs", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewOutputAdapterWithLimit(repoRoot, runID, maxBytes)
	if err != nil {
		t.Fatalf("NewOutputAdapterWithLimit() error = %v", err)
	}
	return adapter, runDir
}
