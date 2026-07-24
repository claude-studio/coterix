package cli

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type configWire struct {
	CLIs            *clisWire  `json:"clis"`
	Roles           *rolesWire `json:"roles"`
	IdleTimeoutSecs *int64     `json:"idle_timeout_secs"`
	MaxRetries      *int       `json:"max_retries"`
	MaxPlanRounds   *int       `json:"max_plan_rounds"`
	MaxTaskAttempts *int       `json:"max_task_attempts"`
	GateCommand     *[]*string `json:"gate_command"`
	GateCWD         *string    `json:"gate_cwd"`
}

type clisWire struct {
	Claude *cliConfigWire `json:"claude"`
	Codex  *cliConfigWire `json:"codex"`
}

type cliConfigWire struct {
	Command *string             `json:"command"`
	Args    *[]*string          `json:"args"`
	Stdin   *bool               `json:"stdin"`
	Env     *map[string]*string `json:"env"`
}

type rolesWire struct {
	PlanWriter   *string `json:"plan_writer"`
	PlanReviewer *string `json:"plan_reviewer"`
	PlanReviser  *string `json:"plan_reviser"`
	ImplWriter   *string `json:"impl_writer"`
	ImplReviewer *string `json:"impl_reviewer"`
	Fixer        *string `json:"fixer"`
}

// LoadConfig reads and strictly validates a config.json file.
func LoadConfig(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("cli: read config %q: %w", path, err)
	}
	return ParseConfig(content)
}

// ParseConfig strictly decodes a config.json payload.
func ParseConfig(content []byte) (Config, error) {
	var wire *configWire
	if err := decodeStrictJSON(content, &wire); err != nil {
		return Config{}, fmt.Errorf("cli: decode config: %w", err)
	}
	if wire == nil {
		return Config{}, fmt.Errorf("cli: config must be a JSON object")
	}
	if err := requireConfigFields(wire); err != nil {
		return Config{}, err
	}

	claude, err := resolveCLIConfig("claude", wire.CLIs.Claude)
	if err != nil {
		return Config{}, err
	}
	codex, err := resolveCLIConfig("codex", wire.CLIs.Codex)
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(*wire.GateCWD) == "" {
		return Config{}, fmt.Errorf("cli: gate_cwd must not be empty")
	}

	config := Config{
		CLIs: map[string]CliConfig{
			"claude": claude,
			"codex":  codex,
		},
		Roles: map[Role]string{
			RolePlanWriter:   *wire.Roles.PlanWriter,
			RolePlanReviewer: *wire.Roles.PlanReviewer,
			RolePlanReviser:  *wire.Roles.PlanReviser,
			RoleImplWriter:   *wire.Roles.ImplWriter,
			RoleImplReviewer: *wire.Roles.ImplReviewer,
			RoleFixer:        *wire.Roles.Fixer,
		},
		IdleTimeoutSecs: *wire.IdleTimeoutSecs,
		MaxRetries:      *wire.MaxRetries,
		MaxPlanRounds:   *wire.MaxPlanRounds,
		MaxTaskAttempts: *wire.MaxTaskAttempts,
		GateCWD:         filepath.Clean(*wire.GateCWD),
	}
	config.GateCommand, err = resolveStringArray("gate_command", *wire.GateCommand)
	if err != nil {
		return Config{}, err
	}

	if err := validateConfig(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func requireConfigFields(wire *configWire) error {
	switch {
	case wire.CLIs == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "clis")
	case wire.Roles == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "roles")
	case wire.IdleTimeoutSecs == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "idle_timeout_secs")
	case wire.MaxRetries == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "max_retries")
	case wire.MaxPlanRounds == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "max_plan_rounds")
	case wire.MaxTaskAttempts == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "max_task_attempts")
	case wire.GateCommand == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "gate_command")
	case wire.GateCWD == nil:
		return fmt.Errorf("cli: required field %q is missing or null", "gate_cwd")
	case wire.CLIs.Claude == nil:
		return fmt.Errorf("cli: required CLI %q is missing or null", "claude")
	case wire.CLIs.Codex == nil:
		return fmt.Errorf("cli: required CLI %q is missing or null", "codex")
	}

	requiredRoles := map[string]*string{
		string(RolePlanWriter):   wire.Roles.PlanWriter,
		string(RolePlanReviewer): wire.Roles.PlanReviewer,
		string(RolePlanReviser):  wire.Roles.PlanReviser,
		string(RoleImplWriter):   wire.Roles.ImplWriter,
		string(RoleImplReviewer): wire.Roles.ImplReviewer,
		string(RoleFixer):        wire.Roles.Fixer,
	}
	for _, role := range roleOrder {
		if requiredRoles[string(role)] == nil {
			return fmt.Errorf("cli: required role %q is missing or null", role)
		}
	}
	return nil
}

func resolveCLIConfig(name string, wire *cliConfigWire) (CliConfig, error) {
	switch {
	case wire.Command == nil:
		return CliConfig{}, fmt.Errorf("cli: %s.command is required", name)
	case wire.Args == nil:
		return CliConfig{}, fmt.Errorf("cli: %s.args is required", name)
	case wire.Stdin == nil:
		return CliConfig{}, fmt.Errorf("cli: %s.stdin is required", name)
	case wire.Env == nil:
		return CliConfig{}, fmt.Errorf("cli: %s.env is required", name)
	case strings.TrimSpace(*wire.Command) == "":
		return CliConfig{}, fmt.Errorf("cli: %s.command must not be empty", name)
	}
	args, err := resolveStringArray(name+".args", *wire.Args)
	if err != nil {
		return CliConfig{}, err
	}
	env := make(map[string]string, len(*wire.Env))
	for key, value := range *wire.Env {
		if value == nil {
			return CliConfig{}, fmt.Errorf("cli: %s.env[%q] must be a string, not null", name, key)
		}
		env[key] = *value
	}
	return CliConfig{
		Command: *wire.Command,
		Args:    args,
		Stdin:   *wire.Stdin,
		Env:     env,
	}, nil
}

func resolveStringArray(field string, values []*string) ([]string, error) {
	resolved := make([]string, len(values))
	for index, value := range values {
		if value == nil {
			return nil, fmt.Errorf("cli: %s[%d] must be a string, not null", field, index)
		}
		resolved[index] = *value
	}
	return resolved, nil
}

func validateConfig(config Config) error {
	maxIdleSeconds := int64(math.MaxInt64 / int64(time.Second))
	switch {
	case config.IdleTimeoutSecs <= 0:
		return fmt.Errorf("cli: idle_timeout_secs must be positive")
	case config.IdleTimeoutSecs > maxIdleSeconds:
		return fmt.Errorf("cli: idle_timeout_secs overflows time.Duration")
	case config.MaxRetries < 0:
		return fmt.Errorf("cli: max_retries must not be negative")
	case config.MaxPlanRounds <= 0:
		return fmt.Errorf("cli: max_plan_rounds must be positive")
	case config.MaxTaskAttempts <= 0:
		return fmt.Errorf("cli: max_task_attempts must be positive")
	case len(config.GateCommand) == 0:
		return fmt.Errorf("cli: gate_command must contain at least one argv element")
	case strings.TrimSpace(config.GateCommand[0]) == "":
		return fmt.Errorf("cli: gate_command executable must not be empty")
	case strings.TrimSpace(config.GateCWD) == "":
		return fmt.Errorf("cli: gate_cwd must not be empty")
	case filepath.IsAbs(config.GateCWD):
		return fmt.Errorf("cli: gate_cwd must be relative to the repository root")
	case config.GateCWD == ".." || strings.HasPrefix(config.GateCWD, ".."+string(filepath.Separator)):
		return fmt.Errorf("cli: gate_cwd must not escape the repository root")
	}
	return validateRoleMapping(config)
}
