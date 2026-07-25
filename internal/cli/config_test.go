package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validConfigJSON = `{
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

func TestLoadVersionedConfig(t *testing.T) {
	path := filepath.Join("..", "..", ".coterix", "config.json")
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%q) error = %v", path, err)
	}
	if config.CLIs["claude"].Command != "claude" ||
		config.CLIs["codex"].Command != "codex" {
		t.Fatalf("unexpected CLI presets: %#v", config.CLIs)
	}
}

func TestParseConfigLoadsPresetsAndRoleMapping(t *testing.T) {
	config, err := ParseConfig([]byte(validConfigJSON))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if config.IdleTimeout() != 600*time.Second {
		t.Fatalf("IdleTimeout() = %v, want 600s", config.IdleTimeout())
	}
	if !reflect.DeepEqual(config.GateCommand, []string{"go", "test", "./..."}) {
		t.Fatalf("GateCommand = %q", config.GateCommand)
	}

	for _, role := range roleOrder {
		resolved, err := config.CLIForRole(role)
		if err != nil {
			t.Fatalf("CLIForRole(%q) error = %v", role, err)
		}
		wantCLI := requiredRoleCLI[role]
		if resolved.Command != wantCLI {
			t.Fatalf("CLIForRole(%q).Command = %q, want %q", role, resolved.Command, wantCLI)
		}
	}

	writer, err := config.CLIForRole(RoleImplWriter)
	if err != nil {
		t.Fatal(err)
	}
	fixer, err := config.CLIForRole(RoleFixer)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(writer, fixer) {
		t.Fatalf("fixer = %#v, want exact impl_writer config %#v", fixer, writer)
	}

	writer.Args[0] = "changed"
	writer.Env["NEW"] = "value"
	fresh, err := config.CLIForRole(RoleImplWriter)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Args[0] != "exec" {
		t.Fatalf("CLIForRole returned aliased args: %q", fresh.Args)
	}
	if _, exists := fresh.Env["NEW"]; exists {
		t.Fatal("CLIForRole returned an aliased env map")
	}
}

func TestParseConfigRejectsInvalidShapeAndValues(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "unknown root field",
			content: strings.TrimSuffix(validConfigJSON, "}") + `, "unknown": true}`,
		},
		{
			name: "unknown CLI field",
			content: strings.Replace(
				validConfigJSON,
				`"env": {}`,
				`"env": {}, "output": "stdout"`,
				1,
			),
		},
		{
			name: "unknown role field",
			content: strings.Replace(
				validConfigJSON,
				`"fixer": "codex"`,
				`"fixer": "codex", "other": "codex"`,
				1,
			),
		},
		{
			name: "missing false stdin",
			content: strings.Replace(
				validConfigJSON,
				`"stdin": false,`,
				``,
				1,
			),
		},
		{
			name: "null env",
			content: strings.Replace(
				validConfigJSON,
				`"env": {}`,
				`"env": null`,
				1,
			),
		},
		{
			name: "null args element",
			content: strings.Replace(
				validConfigJSON,
				`"args": ["-p", "--dangerously-skip-permissions", "--no-session-persistence"]`,
				`"args": ["-p", null]`,
				1,
			),
		},
		{
			name: "null env value",
			content: strings.Replace(
				validConfigJSON,
				`"env": {}`,
				`"env": {"TOKEN": null}`,
				1,
			),
		},
		{
			name: "wrong numeric type",
			content: strings.Replace(
				validConfigJSON,
				`"idle_timeout_secs": 600`,
				`"idle_timeout_secs": "600"`,
				1,
			),
		},
		{
			name: "gate command shell string",
			content: strings.Replace(
				validConfigJSON,
				`"gate_command": ["go", "test", "./..."]`,
				`"gate_command": "go test ./..."`,
				1,
			),
		},
		{
			name: "null gate command element",
			content: strings.Replace(
				validConfigJSON,
				`"gate_command": ["go", "test", "./..."]`,
				`"gate_command": ["go", null]`,
				1,
			),
		},
		{
			name: "writer routing mismatch",
			content: strings.Replace(
				validConfigJSON,
				`"fixer": "codex"`,
				`"fixer": "claude"`,
				1,
			),
		},
		{
			name: "absolute gate cwd",
			content: strings.Replace(
				validConfigJSON,
				`"gate_cwd": "."`,
				`"gate_cwd": "/tmp"`,
				1,
			),
		},
		{
			name: "escaping gate cwd",
			content: strings.Replace(
				validConfigJSON,
				`"gate_cwd": "."`,
				`"gate_cwd": "../outside"`,
				1,
			),
		},
		{
			name: "empty gate cwd",
			content: strings.Replace(
				validConfigJSON,
				`"gate_cwd": "."`,
				`"gate_cwd": ""`,
				1,
			),
		},
		{
			name:    "trailing JSON value",
			content: validConfigJSON + `{}`,
		},
		{
			name:    "null root",
			content: `null`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(test.content)); err == nil {
				t.Fatal("ParseConfig() accepted invalid config")
			}
		})
	}
}

func TestCLIForRoleRejectsUnknownRole(t *testing.T) {
	config, err := ParseConfig([]byte(validConfigJSON))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.CLIForRole(Role("unknown")); err == nil {
		t.Fatal("CLIForRole() accepted an unknown role")
	}
}

// The versioned config is what an actual run uses, so the flags that make claude
// stream have to be in it — documenting them in the spec was not enough, and their
// absence is what made the activity tail silent for whole steps (review T13a-2 f1).
// This test exists so that regression is caught by the gate rather than by a reviewer.
func TestVersionedConfigEnablesClaudeStreaming(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".coterix", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("the versioned config does not parse: %v", err)
	}
	claude, ok := config.CLIs["claude"]
	if !ok {
		t.Fatalf("the versioned config has no claude entry: %#v", config.CLIs)
	}
	args := strings.Join(claude.Args, " ")
	for _, flag := range []string{
		"-p",
		"--output-format stream-json",
		"--verbose",
	} {
		if !strings.Contains(args, flag) {
			t.Fatalf("claude args %q lack %q — the tail cannot stream without it",
				args, flag)
		}
	}
	// The prompt is appended as the final positional argument, so no flag may follow it.
	if claude.Stdin {
		t.Fatal("claude is configured to take the prompt on stdin; stream-json expects -p")
	}
}
