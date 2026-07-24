package cli

import (
	"fmt"
	"time"
)

// Role is a fixed orchestration role.
type Role string

const (
	RolePlanWriter   Role = "plan_writer"
	RolePlanReviewer Role = "plan_reviewer"
	RolePlanReviser  Role = "plan_reviser"
	RoleImplWriter   Role = "impl_writer"
	RoleImplReviewer Role = "impl_reviewer"
	RoleFixer        Role = "fixer"
)

var roleOrder = []Role{
	RolePlanWriter,
	RolePlanReviewer,
	RolePlanReviser,
	RoleImplWriter,
	RoleImplReviewer,
	RoleFixer,
}

var requiredRoleCLI = map[Role]string{
	RolePlanWriter:   "claude",
	RolePlanReviewer: "codex",
	RolePlanReviser:  "claude",
	RoleImplWriter:   "codex",
	RoleImplReviewer: "claude",
	RoleFixer:        "codex",
}

// CliConfig contains only CLI execution settings. Step output contracts are
// intentionally modeled separately.
type CliConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Stdin   bool              `json:"stdin"`
	Env     map[string]string `json:"env"`
}

// Config is the validated, run-start configuration input.
type Config struct {
	CLIs            map[string]CliConfig `json:"clis"`
	Roles           map[Role]string      `json:"roles"`
	IdleTimeoutSecs int64                `json:"idle_timeout_secs"`
	MaxRetries      int                  `json:"max_retries"`
	MaxPlanRounds   int                  `json:"max_plan_rounds"`
	MaxTaskAttempts int                  `json:"max_task_attempts"`
	GateCommand     []string             `json:"gate_command"`
	GateCWD         string               `json:"gate_cwd"`
}

// IdleTimeout converts the validated seconds value to a duration.
func (config Config) IdleTimeout() time.Duration {
	return time.Duration(config.IdleTimeoutSecs) * time.Second
}

// CLIForRole returns a defensive copy of the resolved CLI configuration.
// Fixer resolution deliberately goes through impl_writer to enforce
// writer-routing rather than performing an independent lookup.
func (config Config) CLIForRole(role Role) (CliConfig, error) {
	if role == RoleFixer {
		role = RoleImplWriter
	}
	if !role.Valid() {
		return CliConfig{}, fmt.Errorf("cli: unknown role %q", role)
	}
	name, ok := config.Roles[role]
	if !ok {
		return CliConfig{}, fmt.Errorf("cli: role %q is not configured", role)
	}
	resolved, ok := config.CLIs[name]
	if !ok {
		return CliConfig{}, fmt.Errorf("cli: role %q references unknown CLI %q", role, name)
	}
	return cloneCLIConfig(resolved), nil
}

// Valid reports whether the role is part of the fixed orchestration contract.
func (role Role) Valid() bool {
	_, ok := requiredRoleCLI[role]
	return ok
}

func validateRoleMapping(config Config) error {
	for _, role := range roleOrder {
		want := requiredRoleCLI[role]
		got, ok := config.Roles[role]
		if !ok {
			return fmt.Errorf("cli: required role %q is missing", role)
		}
		if got != want {
			return fmt.Errorf("cli: role %q must resolve to %q, got %q", role, want, got)
		}
		if _, ok := config.CLIs[got]; !ok {
			return fmt.Errorf("cli: role %q references unknown CLI %q", role, got)
		}
	}

	return nil
}

func cloneCLIConfig(config CliConfig) CliConfig {
	clone := CliConfig{
		Command: config.Command,
		Args:    make([]string, len(config.Args)),
		Stdin:   config.Stdin,
	}
	copy(clone.Args, config.Args)
	if config.Env != nil {
		clone.Env = make(map[string]string, len(config.Env))
		for key, value := range config.Env {
			clone.Env[key] = value
		}
	}
	return clone
}
