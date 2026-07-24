package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const headlessPlan = `# Headless dashboard plan

## T1: Dashboard task
- [ ] Implement the dashboard task
Acceptance: The dashboard task is complete
Verify: go test ./internal/ui/...
`

type headlessPlanExecutor struct {
	calls int
}

func (executor *headlessPlanExecutor) Run(
	ctx context.Context,
	request runner.RunRequest,
) (runner.RunResult, error) {
	executor.calls++
	for _, path := range []string{request.StdoutLog, request.StderrLog} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return runner.RunResult{}, err
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return runner.RunResult{}, err
		}
	}
	if request.PrepareAttempt != nil {
		if err := request.PrepareAttempt(ctx, 1); err != nil {
			return runner.RunResult{}, err
		}
	}

	switch request.Effect {
	case runner.EffectArtifactOnly:
		if len(request.OutputPaths) != 2 {
			return runner.RunResult{}, fmt.Errorf(
				"planner output paths=%d",
				len(request.OutputPaths),
			)
		}
		if err := os.WriteFile(
			request.OutputPaths[0],
			[]byte(headlessPlan),
			0o600,
		); err != nil {
			return runner.RunResult{}, err
		}
	case runner.EffectReadOnly:
		if len(request.OutputPaths) != 1 ||
			len(request.CanonicalPaths) == 0 {
			return runner.RunResult{}, fmt.Errorf("review paths are incomplete")
		}
		plan, err := os.ReadFile(request.CanonicalPaths[0])
		if err != nil {
			return runner.RunResult{}, err
		}
		hash := sha256.Sum256(plan)
		review := fmt.Sprintf(
			`{"schema_version":1,"target_plan_hash":"%x","clean":true,"findings":[]}`,
			hash,
		)
		if err := os.WriteFile(
			request.OutputPaths[0],
			[]byte(review),
			0o600,
		); err != nil {
			return runner.RunResult{}, err
		}
	default:
		return runner.RunResult{}, fmt.Errorf(
			"unexpected effect %d during planning",
			request.Effect,
		)
	}

	if request.OnLine != nil {
		request.OnLine(runner.Line{
			Attempt: 1,
			Stream:  runner.StreamStdout,
			Text:    "streamed from fake CLI",
		})
	}
	result := runner.RunResult{
		Exit:      0,
		StdoutLog: request.StdoutLog,
		StderrLog: request.StderrLog,
	}
	if request.ValidateResult != nil {
		if err := request.ValidateResult(ctx, result); err != nil {
			return result, err
		}
	}
	if request.OnAttemptDone != nil {
		request.OnAttemptDone(runner.AttemptDone{
			Attempt: 1,
			Result:  result,
		})
	}
	return result, nil
}

func TestRunHeadlessDrivesPipelineThroughDashboardModel(t *testing.T) {
	repoRoot := newHeadlessUIRepository(t)
	executor := &headlessPlanExecutor{}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := Run(
		ctx,
		executor,
		repoRoot,
		"build the dashboard",
		RunOptions{
			Output:      &output,
			Interactive: false,
			Width:       100,
			Height:      24,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if status.RunID == "" ||
		status.Phase != state.PhaseAwaitingApproval ||
		executor.calls != 2 {
		t.Fatalf(
			"headless dashboard status=%#v executor calls=%d",
			status,
			executor.calls,
		)
	}
	if len(status.TaskOrder) != 1 || status.TaskOrder[0] != "T1" {
		t.Fatalf("materialized tasks=%#v", status.TaskOrder)
	}
}

func newHeadlessUIRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "ui-test@example.com")
	runGit(t, root, "config", "user.name", "UI Test")

	if err := os.Mkdir(filepath.Join(root, ".coterix"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := cli.Config{
		CLIs: map[string]cli.CliConfig{
			"claude": {
				Command: "fake-claude",
				Args:    []string{},
				Stdin:   false,
				Env:     map[string]string{},
			},
			"codex": {
				Command: "fake-codex",
				Args:    []string{},
				Stdin:   true,
				Env:     map[string]string{},
			},
		},
		Roles: map[cli.Role]string{
			cli.RolePlanWriter:   "claude",
			cli.RolePlanReviewer: "codex",
			cli.RolePlanReviser:  "claude",
			cli.RoleImplWriter:   "codex",
			cli.RoleImplReviewer: "claude",
			cli.RoleFixer:        "codex",
		},
		IdleTimeoutSecs: 10,
		MaxRetries:      0,
		MaxPlanRounds:   2,
		MaxTaskAttempts: 2,
		GateCommand:     []string{"true"},
		GateCWD:         ".",
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(
		filepath.Join(root, ".coterix", "config.json"),
		encoded,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".gitignore"),
		[]byte(".coterix/runs/\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "tracked.txt"),
		[]byte("baseline\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "test: baseline")
	return root
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
