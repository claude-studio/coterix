package ui

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

func TestLoadArtifactDataPlanVerdictsDiffAndPassingEvidence(t *testing.T) {
	repoRoot, runDir, runID := newArtifactTestRun(t)
	baseSHA, candidateSHA := artifactTestCommits(
		t,
		repoRoot,
		[]byte("before\n"),
		[]byte("after\n"),
	)

	plan := "# Plan\n\n## T1: Change tracked content\n"
	planVerdict := `{"schema_version":1,"target_plan_hash":"hash","clean":true,"findings":[]}`
	gateEvidence := `{"exit":0,"timed_out":false}`
	taskVerdict := `{"schema_version":1,"plan_hash":"hash","task_id":"T1","candidate_sha":"candidate","clean":true,"findings":[]}`
	writeArtifactTestFile(t, filepath.Join(runDir, "plan.md"), []byte(plan))
	writeArtifactTestFile(
		t,
		filepath.Join(runDir, "plan-review.json"),
		[]byte(planVerdict),
	)
	writeArtifactTestFile(
		t,
		filepath.Join(runDir, "tasks", "T1", "gate.json"),
		[]byte(gateEvidence),
	)
	writeArtifactTestFile(
		t,
		filepath.Join(runDir, "tasks", "T1", "review.json"),
		[]byte(taskVerdict),
	)

	taskID := "T1"
	gatePath := filepath.Join("tasks", taskID, "gate.json")
	reviewPath := filepath.Join("tasks", taskID, "review.json")
	status := pipeline.RunStatus{
		RunID:         runID,
		CurrentTaskID: &taskID,
		Tasks: map[string]state.TaskState{
			taskID: {
				Status:       state.TaskCandidate,
				BaseSHA:      &baseSHA,
				CandidateSHA: &candidateSHA,
				GateResult:   &gatePath,
				ReviewResult: &reviewPath,
			},
		},
	}

	artifacts, err := loadArtifactData(context.Background(), repoRoot, status)
	if err != nil {
		t.Fatalf("loadArtifactData() error = %v", err)
	}
	if artifacts.PlanMarkdown != plan {
		t.Fatalf("PlanMarkdown = %q, want %q", artifacts.PlanMarkdown, plan)
	}
	if artifacts.DiffContent == nil {
		t.Fatal("DiffContent = nil, want candidate diff")
	}
	if !strings.Contains(*artifacts.DiffContent, "-before") ||
		!strings.Contains(*artifacts.DiffContent, "+after") {
		t.Fatalf("DiffContent does not contain expected patch:\n%s", *artifacts.DiffContent)
	}
	if artifacts.GateOutcome != evidencePass ||
		artifacts.ReviewOutcome != evidencePass {
		t.Fatalf(
			"outcomes = gate %v review %v, want pass/pass",
			artifacts.GateOutcome,
			artifacts.ReviewOutcome,
		)
	}
	if len(artifacts.Verdicts) != 2 {
		t.Fatalf("len(Verdicts) = %d, want 2", len(artifacts.Verdicts))
	}
	if artifacts.Verdicts[0].Name != "plan-review.json" ||
		artifacts.Verdicts[0].JSON != planVerdict {
		t.Fatalf("plan verdict = %#v", artifacts.Verdicts[0])
	}
	if artifacts.Verdicts[1].Name != reviewPath ||
		artifacts.Verdicts[1].JSON != taskVerdict {
		t.Fatalf("task verdict = %#v", artifacts.Verdicts[1])
	}
}

func TestLoadArtifactDataEvidenceOutcomes(t *testing.T) {
	t.Run("missing links are unknown", func(t *testing.T) {
		repoRoot, _, runID := newArtifactTestRun(t)
		taskID := "T1"
		status := pipeline.RunStatus{
			RunID:         runID,
			CurrentTaskID: &taskID,
			Tasks: map[string]state.TaskState{
				taskID: {Status: state.TaskOpen},
			},
		}

		artifacts, err := loadArtifactData(context.Background(), repoRoot, status)
		if err != nil {
			t.Fatalf("loadArtifactData() error = %v", err)
		}
		if artifacts.GateOutcome != evidenceUnknown ||
			artifacts.ReviewOutcome != evidenceUnknown {
			t.Fatalf(
				"outcomes = gate %v review %v, want unknown/unknown",
				artifacts.GateOutcome,
				artifacts.ReviewOutcome,
			)
		}
	})

	t.Run("failed gate and dirty review are fail", func(t *testing.T) {
		repoRoot, runDir, runID := newArtifactTestRun(t)
		taskID := "T1"
		gatePath := filepath.Join("tasks", taskID, "gate.json")
		reviewPath := filepath.Join("tasks", taskID, "review.json")
		writeArtifactTestFile(
			t,
			filepath.Join(runDir, gatePath),
			[]byte(`{"exit":1,"timed_out":false}`),
		)
		writeArtifactTestFile(
			t,
			filepath.Join(runDir, reviewPath),
			[]byte(`{"clean":false}`),
		)
		status := pipeline.RunStatus{
			RunID:         runID,
			CurrentTaskID: &taskID,
			Tasks: map[string]state.TaskState{
				taskID: {
					Status:       state.TaskRepairing,
					GateResult:   &gatePath,
					ReviewResult: &reviewPath,
				},
			},
		}

		artifacts, err := loadArtifactData(context.Background(), repoRoot, status)
		if err != nil {
			t.Fatalf("loadArtifactData() error = %v", err)
		}
		if artifacts.GateOutcome != evidenceFail ||
			artifacts.ReviewOutcome != evidenceFail {
			t.Fatalf(
				"outcomes = gate %v review %v, want fail/fail",
				artifacts.GateOutcome,
				artifacts.ReviewOutcome,
			)
		}
	})

	t.Run("timeout is a failed gate", func(t *testing.T) {
		repoRoot, runDir, runID := newArtifactTestRun(t)
		taskID := "T1"
		gatePath := filepath.Join("tasks", taskID, "gate.json")
		writeArtifactTestFile(
			t,
			filepath.Join(runDir, gatePath),
			[]byte(`{"exit":0,"timed_out":true}`),
		)
		status := pipeline.RunStatus{
			RunID:         runID,
			CurrentTaskID: &taskID,
			Tasks: map[string]state.TaskState{
				taskID: {
					Status:     state.TaskRepairing,
					GateResult: &gatePath,
				},
			},
		}

		artifacts, err := loadArtifactData(context.Background(), repoRoot, status)
		if err != nil {
			t.Fatalf("loadArtifactData() error = %v", err)
		}
		if artifacts.GateOutcome != evidenceFail {
			t.Fatalf("GateOutcome = %v, want fail", artifacts.GateOutcome)
		}
	})
}

func TestLoadArtifactDataRejectsUnsafeArtifacts(t *testing.T) {
	t.Run("unsafe run ids", func(t *testing.T) {
		repoRoot := t.TempDir()
		for _, runID := range []string{"", ".", "..", "../escape", "nested/run", `nested\run`} {
			t.Run(runID, func(t *testing.T) {
				_, err := loadArtifactData(
					context.Background(),
					repoRoot,
					pipeline.RunStatus{RunID: runID},
				)
				if err == nil {
					t.Fatalf("loadArtifactData() accepted unsafe run id %q", runID)
				}
			})
		}
	})

	t.Run("symlink plan", func(t *testing.T) {
		repoRoot, runDir, runID := newArtifactTestRun(t)
		outside := filepath.Join(t.TempDir(), "outside.md")
		writeArtifactTestFile(t, outside, []byte("# Outside\n"))
		if err := os.Symlink(outside, filepath.Join(runDir, "plan.md")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}

		_, err := loadArtifactData(
			context.Background(),
			repoRoot,
			pipeline.RunStatus{RunID: runID},
		)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("loadArtifactData() error = %v, want symlink rejection", err)
		}
	})

	t.Run("nonregular plan", func(t *testing.T) {
		repoRoot, runDir, runID := newArtifactTestRun(t)
		if err := os.Mkdir(filepath.Join(runDir, "plan.md"), 0o700); err != nil {
			t.Fatal(err)
		}

		_, err := loadArtifactData(
			context.Background(),
			repoRoot,
			pipeline.RunStatus{RunID: runID},
		)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("loadArtifactData() error = %v, want nonregular rejection", err)
		}
	})

	t.Run("oversized plan", func(t *testing.T) {
		repoRoot, runDir, runID := newArtifactTestRun(t)
		content := bytes.Repeat([]byte("x"), int(maxArtifactFileBytes)+1)
		writeArtifactTestFile(t, filepath.Join(runDir, "plan.md"), content)

		_, err := loadArtifactData(
			context.Background(),
			repoRoot,
			pipeline.RunStatus{RunID: runID},
		)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("loadArtifactData() error = %v, want size rejection", err)
		}
	})

	t.Run("unsafe linked evidence path", func(t *testing.T) {
		repoRoot, _, runID := newArtifactTestRun(t)
		taskID := "T1"
		unsafePath := filepath.Join("..", "gate.json")
		status := pipeline.RunStatus{
			RunID:         runID,
			CurrentTaskID: &taskID,
			Tasks: map[string]state.TaskState{
				taskID: {
					Status:     state.TaskCandidate,
					GateResult: &unsafePath,
				},
			},
		}

		_, err := loadArtifactData(context.Background(), repoRoot, status)
		if err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("loadArtifactData() error = %v, want unsafe path rejection", err)
		}
	})
}

func TestLoadArtifactDataCapsDerivedDiff(t *testing.T) {
	repoRoot, _, runID := newArtifactTestRun(t)
	lineCount := int(maxArtifactDiffBytes / int64(len("before\n")))
	before := bytes.Repeat([]byte("before\n"), lineCount)
	after := bytes.Repeat([]byte("after\n"), lineCount)
	baseSHA, candidateSHA := artifactTestCommits(t, repoRoot, before, after)
	taskID := "T1"
	status := pipeline.RunStatus{
		RunID:         runID,
		CurrentTaskID: &taskID,
		Tasks: map[string]state.TaskState{
			taskID: {
				Status:       state.TaskCandidate,
				BaseSHA:      &baseSHA,
				CandidateSHA: &candidateSHA,
			},
		},
	}

	_, err := loadArtifactData(context.Background(), repoRoot, status)
	if err == nil || !strings.Contains(err.Error(), "diff exceeds") {
		t.Fatalf("loadArtifactData() error = %v, want bounded diff rejection", err)
	}
}

func newArtifactTestRun(t *testing.T) (string, string, string) {
	t.Helper()
	repoRoot := t.TempDir()
	runID := "run-1"
	runDir := filepath.Join(repoRoot, ".coterix", "runs", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return repoRoot, runDir, runID
}

func artifactTestCommits(
	t *testing.T,
	repoRoot string,
	before []byte,
	after []byte,
) (string, string) {
	t.Helper()
	artifactTestGit(t, repoRoot, "init", "--quiet")
	artifactTestGit(t, repoRoot, "config", "user.name", "Coterix Test")
	artifactTestGit(t, repoRoot, "config", "user.email", "test@example.com")

	tracked := filepath.Join(repoRoot, "tracked.txt")
	writeArtifactTestFile(t, tracked, before)
	artifactTestGit(t, repoRoot, "add", "tracked.txt")
	artifactTestGit(t, repoRoot, "commit", "--quiet", "-m", "base")
	baseSHA := artifactTestGit(t, repoRoot, "rev-parse", "HEAD")

	writeArtifactTestFile(t, tracked, after)
	artifactTestGit(t, repoRoot, "add", "tracked.txt")
	artifactTestGit(t, repoRoot, "commit", "--quiet", "-m", "candidate")
	candidateSHA := artifactTestGit(t, repoRoot, "rev-parse", "HEAD")
	return baseSHA, candidateSHA
}

func artifactTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeArtifactTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The changed-files list reads untrusted git output, so the three shapes `--numstat`
// actually produces are pinned here. Measured against real git (2026-07-25) rather than
// assumed: a text change gives `adds\tdels\tpath`, a **binary** change gives `-\t-\tpath`,
// and an **exact rename** gives `0\t0\told => new` — all three are three fields, so the
// parser never mistakes a field for a path (T13 W5).
func TestLoadChangedFilesParsesBinaryAndRenameShapes(t *testing.T) {
	repoRoot := t.TempDir()
	git := func(args ...string) string {
		return artifactTestGit(t, repoRoot, args...)
	}
	git("init", "--quiet")
	git("config", "user.name", "Coterix Test")
	git("config", "user.email", "test@example.com")

	writeArtifactTestFile(t, filepath.Join(repoRoot, "text.txt"), []byte("one\n"))
	writeArtifactTestFile(
		t,
		filepath.Join(repoRoot, "moved.txt"),
		[]byte(strings.Repeat("identical\n", 20)),
	)
	writeArtifactTestFile(
		t,
		filepath.Join(repoRoot, "blob.bin"),
		[]byte{0, 1, 2, 3, 4, 5, 6, 7},
	)
	git("add", ".")
	git("commit", "--quiet", "-m", "base")
	baseSHA := git("rev-parse", "HEAD")

	writeArtifactTestFile(t, filepath.Join(repoRoot, "text.txt"), []byte("one\ntwo\n"))
	if err := os.Rename(
		filepath.Join(repoRoot, "moved.txt"),
		filepath.Join(repoRoot, "renamed.txt"),
	); err != nil {
		t.Fatal(err)
	}
	writeArtifactTestFile(
		t,
		filepath.Join(repoRoot, "blob.bin"),
		[]byte{7, 6, 5, 4, 3, 2, 1, 0},
	)
	git("add", "-A")
	git("commit", "--quiet", "-m", "candidate")
	candidateSHA := git("rev-parse", "HEAD")

	files, err := loadChangedFiles(
		context.Background(),
		repoRoot,
		&baseSHA,
		&candidateSHA,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("changed files = %#v, want three entries", files)
	}
	byPath := make(map[string]changedFile, len(files))
	for _, file := range files {
		if file.Path == "" {
			t.Fatalf("an entry has no path: %#v", files)
		}
		byPath[file.Path] = file
	}
	if got, ok := byPath["text.txt"]; !ok || got.Additions != 1 || got.Deletions != 0 {
		t.Fatalf("text change = %#v", byPath)
	}
	// Binary counts are "-": the row survives with zero counts rather than being dropped.
	if got, ok := byPath["blob.bin"]; !ok || got.Additions != 0 || got.Deletions != 0 {
		t.Fatalf("binary change = %#v", byPath)
	}
	// The rename keeps git's own `old => new` rendering, which is what the operator wants
	// to read; it must not be split into two bogus entries.
	renamed := ""
	for path := range byPath {
		if strings.Contains(path, "=>") {
			renamed = path
		}
	}
	if renamed == "" ||
		!strings.Contains(renamed, "moved.txt") ||
		!strings.Contains(renamed, "renamed.txt") {
		t.Fatalf("rename entry = %#v", byPath)
	}
}

// Guards shared with the diff loader: no base/candidate, identical SHAs, or a malformed
// object id must not reach git (T13 W5).
func TestLoadChangedFilesRefusesUnsafeInput(t *testing.T) {
	repoRoot := t.TempDir()
	valid := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)

	// `wantErrContains` matters: a malformed id makes git fail too, so asserting merely
	// "an error happened" passes with or without the guard. The message is what proves the
	// input was rejected *before* exec (self-correction — the mutation passed until this).
	for _, test := range []struct {
		name            string
		base            *string
		candidate       *string
		wantErrContains string
	}{
		{name: "no base"},
		{name: "no candidate", base: &valid},
		{name: "identical", base: &valid, candidate: &valid},
		{
			name:            "malformed base",
			base:            pointerTo("../../etc/passwd"),
			candidate:       &other,
			wantErrContains: "invalid base_sha",
		},
		{
			name:            "malformed candidate",
			base:            &valid,
			candidate:       pointerTo("HEAD; rm -rf /"),
			wantErrContains: "invalid candidate_sha",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			files, err := loadChangedFiles(
				context.Background(),
				repoRoot,
				test.base,
				test.candidate,
			)
			if test.wantErrContains != "" {
				if err == nil {
					t.Fatal("an invalid object id was accepted")
				}
				if !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("err=%v, want it rejected by %q before reaching git",
						err, test.wantErrContains)
				}
				return
			}
			if err != nil || files != nil {
				t.Fatalf("files=%#v err=%v, want no work done", files, err)
			}
		})
	}
}
