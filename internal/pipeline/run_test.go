package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/state"
)

func TestCreateAndOpenRunUseImmutableSnapshots(t *testing.T) {
	repoRoot := t.TempDir()
	config := testConfig(t)
	created, err := CreateRun(repoRoot, "run-1", "build the requested feature\n", config)
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if created.ID != "run-1" || created.Request != "build the requested feature\n" {
		t.Fatalf("CreateRun() = %#v", created)
	}
	if created.State == nil || created.State.Phase != state.PhasePlanning {
		t.Fatalf("CreateRun() initial state = %#v", created.State)
	}
	if created.adapter == nil {
		t.Fatal("CreateRun() did not initialize an output adapter")
	}
	if info, err := os.Stat(filepath.Join(created.Dir, "logs")); err != nil || !info.IsDir() {
		t.Fatalf("logs directory: info=%v err=%v", info, err)
	}

	for _, name := range []string{requestFileName, configFileName} {
		info, err := os.Stat(filepath.Join(created.Dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s permissions = %o, want no group/other bits", name, info.Mode().Perm())
		}
	}

	snapshot, err := os.ReadFile(filepath.Join(created.Dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(snapshot) || snapshot[len(snapshot)-1] != '\n' {
		t.Fatalf("config snapshot is not canonical JSON: %q", snapshot)
	}
	snapshotConfig, err := cli.ParseConfig(snapshot)
	if err != nil {
		t.Fatalf("ParseConfig(snapshot) error = %v", err)
	}
	if !reflect.DeepEqual(snapshotConfig, created.Config) {
		t.Fatalf("snapshot config differs from run config")
	}

	statePointer := created.State
	created.State.PlanRound = 1
	if err := created.SaveState(); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	if created.State != statePointer {
		t.Fatal("SaveState() replaced the State pointer")
	}

	opened, err := OpenRun(repoRoot, created.ID)
	if err != nil {
		t.Fatalf("OpenRun() error = %v", err)
	}
	if opened.Request != created.Request || !reflect.DeepEqual(opened.Config, created.Config) {
		t.Fatalf("OpenRun() did not load immutable snapshots")
	}
	if opened.State.PlanRound != 1 {
		t.Fatalf("OpenRun() plan round = %d, want 1", opened.State.PlanRound)
	}
}

func TestCreateRunDoesNotOverwriteExistingRun(t *testing.T) {
	repoRoot := t.TempDir()
	config := testConfig(t)
	first, err := CreateRun(repoRoot, "fixed", "original request", config)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := CreateRun(repoRoot, "fixed", "replacement request", config); err == nil {
		t.Fatal("CreateRun() overwrote an existing run")
	}
	content, err := os.ReadFile(filepath.Join(first.Dir, requestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original request" {
		t.Fatalf("existing request = %q", content)
	}
}

func TestCreateRunGeneratesSafeID(t *testing.T) {
	run, err := CreateRun(t.TempDir(), "", "request", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if run.ID == "" {
		t.Fatal("CreateRun() generated an empty id")
	}
	if err := validateRunID(run.ID); err != nil {
		t.Fatalf("generated id is invalid: %v", err)
	}
	if filepath.Base(run.Dir) != run.ID {
		t.Fatalf("run directory %q does not end in id %q", run.Dir, run.ID)
	}
}

func TestCreateRunRejectsInvalidInputsBeforeCreatingRun(t *testing.T) {
	for _, runID := range []string{".", "..", "../escape", "nested/run", `nested\run`} {
		t.Run(strings.ReplaceAll(runID, "/", "_"), func(t *testing.T) {
			repoRoot := t.TempDir()
			if _, err := CreateRun(repoRoot, runID, "request", testConfig(t)); err == nil {
				t.Fatalf("CreateRun() accepted run id %q", runID)
			}
		})
	}

	repoRoot := t.TempDir()
	invalidConfig := testConfig(t)
	invalidConfig.MaxPlanRounds = 0
	if _, err := CreateRun(repoRoot, "invalid-config", "request", invalidConfig); err == nil {
		t.Fatal("CreateRun() accepted invalid config")
	}
	if _, err := os.Stat(
		filepath.Join(repoRoot, ".coterix", "runs", "invalid-config"),
	); !os.IsNotExist(err) {
		t.Fatalf("invalid config left a run directory: %v", err)
	}
	if _, err := CreateRun(repoRoot, "empty-request", " \n", testConfig(t)); err == nil {
		t.Fatal("CreateRun() accepted an empty request")
	}
}

func TestOpenRunStrictlyValidatesSnapshotAndCoreFiles(t *testing.T) {
	t.Run("unknown snapshot field", func(t *testing.T) {
		run, err := CreateRun(t.TempDir(), "run", "request", testConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(run.Dir, configFileName)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content = []byte(strings.Replace(
			string(content),
			"\"gate_cwd\": \".\"",
			"\"gate_cwd\": \".\", \"unknown\": true",
			1,
		))
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenRun(run.RepoRoot, run.ID); err == nil {
			t.Fatal("OpenRun() accepted an unknown snapshot field")
		}
	})

	t.Run("symlink core file", func(t *testing.T) {
		run, err := CreateRun(t.TempDir(), "run", "request", testConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		requestPath := filepath.Join(run.Dir, requestFileName)
		if err := os.Remove(requestPath); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.txt")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, requestPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := OpenRun(run.RepoRoot, run.ID); err == nil {
			t.Fatal("OpenRun() followed a symlink request")
		}
	})
}

func TestOpenRunRejectsSymlinkRunDirectory(t *testing.T) {
	repoRoot := t.TempDir()
	run, err := CreateRun(repoRoot, "run", "request", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	moved := run.Dir + ".moved"
	if err := os.Rename(run.Dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, run.Dir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := OpenRun(repoRoot, "run"); err == nil {
		t.Fatal("OpenRun() accepted a symlink run directory")
	}
}

func testConfig(t *testing.T) cli.Config {
	t.Helper()
	const fixture = `{
	  "clis": {
	    "claude": {
	      "command": "claude",
	      "args": ["-p", "--dangerously-skip-permissions", "--no-session-persistence"],
	      "stdin": false,
	      "env": {}
	    },
	    "codex": {
	      "command": "codex",
	      "args": ["exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"],
	      "stdin": true,
	      "env": {}
	    }
	  },
	  "roles": {
	    "plan_writer": "claude",
	    "plan_reviewer": "codex",
	    "plan_reviser": "claude",
	    "impl_writer": "codex",
	    "impl_reviewer": "claude",
	    "fixer": "codex"
	  },
	  "idle_timeout_secs": 600,
	  "max_retries": 3,
	  "max_plan_rounds": 5,
	  "max_task_attempts": 5,
	  "gate_command": ["go", "test", "./..."],
	  "gate_cwd": "."
	}`
	config, err := cli.ParseConfig([]byte(fixture))
	if err != nil {
		t.Fatalf("ParseConfig(test fixture) error = %v", err)
	}
	return config
}
