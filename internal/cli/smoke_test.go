package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ridenow/coterix/internal/runner"
)

func TestInstalledCLIsWriteDesignatedJSON(t *testing.T) {
	config, err := ParseConfig([]byte(validConfigJSON))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		role Role
	}{
		{name: "claude", role: RolePlanWriter},
		{name: "codex", role: RolePlanReviewer},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executable, err := exec.LookPath(test.name)
			if errors.Is(err, exec.ErrNotFound) {
				t.Skipf("%s is not installed", test.name)
			}
			if err != nil {
				t.Fatalf("LookPath(%q) error = %v", test.name, err)
			}

			cliConfig, err := config.CLIForRole(test.role)
			if err != nil {
				t.Fatal(err)
			}
			repoRoot := t.TempDir()
			runID := "smoke-" + test.name
			runDir := filepath.Join(repoRoot, ".coterix", "runs", runID)
			if err := os.MkdirAll(runDir, 0o700); err != nil {
				t.Fatal(err)
			}
			adapter, err := NewOutputAdapter(repoRoot, runID)
			if err != nil {
				t.Fatal(err)
			}

			outputPath := filepath.Join(runDir, "smoke.json")
			prompt := fmt.Sprintf(
				"Adapter smoke test. Write exactly this JSON to the single absolute path %q: "+
					`{"schema_version":1,"adapter":%q,"ok":true}. `+
					"Do not modify any other file or path. Do not use a code fence. "+
					"Stop immediately after writing the file.",
				outputPath,
				test.name,
			)

			args := append([]string(nil), cliConfig.Args...)
			var stdin []byte
			if cliConfig.Stdin {
				stdin = []byte(prompt)
			} else {
				args = append(args, prompt)
			}

			var payload *smokePayload
			executor := runner.New()
			defer executor.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			result, runErr := executor.Run(ctx, runner.RunRequest{
				Command:     executable,
				Args:        args,
				Dir:         repoRoot,
				Env:         cliConfig.Env,
				Stdin:       stdin,
				IdleTimeout: 2 * time.Minute,
				Effect:      runner.EffectReadOnly,
				OutputPaths: []string{outputPath},
				StdoutLog:   filepath.Join(runDir, "stdout.log"),
				StderrLog:   filepath.Join(runDir, "stderr.log"),
				ValidateResult: func(context.Context, runner.RunResult) error {
					content, err := adapter.NewAttempt().Consume(outputPath, ".json")
					if err != nil {
						return fmt.Errorf("consume designated JSON: %w", err)
					}
					var decoded *smokePayload
					if err := decodeStrictJSON(content, &decoded); err != nil {
						return fmt.Errorf("decode designated JSON: %w", err)
					}
					if decoded == nil || decoded.SchemaVersion == nil ||
						decoded.Adapter == nil || decoded.OK == nil {
						return fmt.Errorf("designated JSON is missing required fields")
					}
					if *decoded.SchemaVersion != 1 ||
						*decoded.Adapter != test.name ||
						!*decoded.OK {
						return fmt.Errorf("designated JSON has unexpected values")
					}
					payload = decoded
					return nil
				},
			})
			if runErr != nil {
				stderr, _ := os.ReadFile(result.StderrLog)
				t.Fatalf(
					"%s smoke run failed: %T %v",
					test.name,
					ClassifyFailure(test.name, runErr, stderr),
					ClassifyFailure(test.name, runErr, stderr),
				)
			}

			if payload == nil {
				t.Fatal("runner did not validate the designated JSON result")
			}
		})
	}
}

type smokePayload struct {
	SchemaVersion *int    `json:"schema_version"`
	Adapter       *string `json:"adapter"`
	OK            *bool   `json:"ok"`
}
