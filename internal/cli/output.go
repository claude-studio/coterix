package cli

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ridenow/coterix/internal/runner"
)

// DefaultMaxOutputBytes bounds one agent-produced result artifact.
const DefaultMaxOutputBytes int64 = 1 << 20

// ResultPaths contains the role-specific paths injected into prompts.
type ResultPaths struct {
	PlanOutput  string
	Questions   string
	CurrentPlan string
	Review      string
}

// OutputPolicy maps a role to runner effect and path preparation metadata.
type OutputPolicy struct {
	Effect         runner.Effect
	OutputPaths    []string
	CanonicalPaths []string
}

// OutputAdapter derives and protects one .coterix/runs/<run_id> directory.
type OutputAdapter struct {
	repoRoot string
	runDir   string
	maxBytes int64
}

// OutputAttempt consumes each result path at most once. A retry must create a
// fresh attempt rather than rereading a failed result.
type OutputAttempt struct {
	adapter  *OutputAdapter
	mu       sync.Mutex
	consumed map[string]struct{}
}

// NewOutputAdapter constructs an adapter with the default size limit.
func NewOutputAdapter(repoRoot, runID string) (*OutputAdapter, error) {
	return NewOutputAdapterWithLimit(repoRoot, runID, DefaultMaxOutputBytes)
}

// NewOutputAdapterWithLimit constructs an adapter for a derived run directory.
func NewOutputAdapterWithLimit(repoRoot, runID string, maxBytes int64) (*OutputAdapter, error) {
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return nil, fmt.Errorf("cli: max output bytes must be between 1 and %d", int64(math.MaxInt64)-1)
	}
	if runID == "" || runID == "." || runID == ".." ||
		filepath.Base(runID) != runID || filepath.Clean(runID) != runID {
		return nil, fmt.Errorf("cli: run id must be one non-empty path component")
	}

	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("cli: resolve repository root: %w", err)
	}
	root = filepath.Clean(root)
	runDir := filepath.Join(root, ".coterix", "runs", runID)
	adapter := &OutputAdapter{
		repoRoot: root,
		runDir:   runDir,
		maxBytes: maxBytes,
	}
	if err := adapter.validateRootDirectories(); err != nil {
		return nil, err
	}

	return adapter, nil
}

// RunDir returns the absolute run directory owned by the adapter.
func (adapter *OutputAdapter) RunDir() string {
	return adapter.runDir
}

// NewAttempt starts a single-consumption result-reading scope.
func (adapter *OutputAdapter) NewAttempt() *OutputAttempt {
	return &OutputAttempt{
		adapter:  adapter,
		consumed: make(map[string]struct{}),
	}
}

// PolicyForRole validates prompt paths and returns runner preparation metadata.
// It does not use runner.PrepareAttempt; runner clears OutputPaths itself on
// every safe retry.
func (adapter *OutputAdapter) PolicyForRole(role Role, paths ResultPaths) (OutputPolicy, error) {
	switch role {
	case RolePlanWriter:
		if paths.CurrentPlan != "" || paths.Review != "" {
			return OutputPolicy{}, fmt.Errorf("cli: plan_writer received unexpected result paths")
		}
		planOutput, err := adapter.validatePath(paths.PlanOutput, ".md", false)
		if err != nil {
			return OutputPolicy{}, err
		}
		questions, err := adapter.validatePath(paths.Questions, ".md", false)
		if err != nil {
			return OutputPolicy{}, err
		}
		if planOutput == questions {
			return OutputPolicy{}, fmt.Errorf("cli: plan and questions outputs must be distinct")
		}
		return OutputPolicy{
			Effect:      runner.EffectArtifactOnly,
			OutputPaths: []string{planOutput, questions},
		}, nil

	case RolePlanReviser:
		if paths.Review != "" {
			return OutputPolicy{}, fmt.Errorf("cli: plan_reviser received an unexpected review path")
		}
		planOutput, err := adapter.validatePath(paths.PlanOutput, ".md", false)
		if err != nil {
			return OutputPolicy{}, err
		}
		questions, err := adapter.validatePath(paths.Questions, ".md", false)
		if err != nil {
			return OutputPolicy{}, err
		}
		currentPlan, err := adapter.validatePath(paths.CurrentPlan, ".md", true)
		if err != nil {
			return OutputPolicy{}, err
		}
		if planOutput == questions || planOutput == currentPlan || questions == currentPlan {
			return OutputPolicy{}, fmt.Errorf("cli: reviser input and outputs must be distinct")
		}
		return OutputPolicy{
			Effect:         runner.EffectArtifactOnly,
			OutputPaths:    []string{planOutput, questions},
			CanonicalPaths: []string{currentPlan},
		}, nil

	case RolePlanReviewer, RoleImplReviewer:
		if paths.PlanOutput != "" || paths.Questions != "" || paths.CurrentPlan != "" {
			return OutputPolicy{}, fmt.Errorf("cli: reviewer received unexpected planner paths")
		}
		review, err := adapter.validatePath(paths.Review, ".json", false)
		if err != nil {
			return OutputPolicy{}, err
		}
		return OutputPolicy{
			Effect:      runner.EffectReadOnly,
			OutputPaths: []string{review},
		}, nil

	case RoleImplWriter, RoleFixer:
		if paths.PlanOutput != "" || paths.Questions != "" ||
			paths.CurrentPlan != "" || paths.Review != "" {
			return OutputPolicy{}, fmt.Errorf("cli: mutating role %q cannot declare file results", role)
		}
		return OutputPolicy{Effect: runner.EffectMutating}, nil

	default:
		return OutputPolicy{}, fmt.Errorf("cli: unknown role %q", role)
	}
}

// Consume opens and reads a regular run-scoped result exactly once for this
// attempt. Call it only after the subprocess has exited.
func (attempt *OutputAttempt) Consume(path, extension string) ([]byte, error) {
	if extension != ".json" && extension != ".md" {
		return nil, fmt.Errorf("cli: unsupported result extension %q", extension)
	}
	resolved, info, err := attempt.adapter.inspectPath(path, extension, true)
	if err != nil {
		return nil, err
	}

	attempt.mu.Lock()
	if _, alreadyRead := attempt.consumed[resolved]; alreadyRead {
		attempt.mu.Unlock()
		return nil, fmt.Errorf("cli: result %q was already consumed for this attempt", resolved)
	}
	attempt.consumed[resolved] = struct{}{}
	attempt.mu.Unlock()

	file, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("cli: open result %q: %w", resolved, err)
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("cli: stat opened result %q: %w", resolved, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("cli: result %q changed before it could be read", resolved)
	}

	content, err := io.ReadAll(io.LimitReader(file, attempt.adapter.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cli: read result %q: %w", resolved, err)
	}
	if int64(len(content)) > attempt.adapter.maxBytes {
		return nil, fmt.Errorf(
			"cli: result %q exceeds %d bytes",
			resolved,
			attempt.adapter.maxBytes,
		)
	}
	return content, nil
}

func (adapter *OutputAdapter) resultExists(path, extension string) (bool, error) {
	resolved, _, err := adapter.inspectPath(path, extension, false)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(resolved)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("cli: inspect result %q: %w", resolved, err)
	}
}

func (adapter *OutputAdapter) validatePath(path, extension string, mustExist bool) (string, error) {
	resolved, _, err := adapter.inspectPath(path, extension, mustExist)
	return resolved, err
}

func (adapter *OutputAdapter) inspectPath(
	path string,
	extension string,
	mustExist bool,
) (string, os.FileInfo, error) {
	if err := adapter.validateRootDirectories(); err != nil {
		return "", nil, err
	}
	if path == "" {
		return "", nil, fmt.Errorf("cli: result path is required")
	}
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("cli: result path must be absolute: %q", path)
	}
	if filepath.Clean(path) != path {
		return "", nil, fmt.Errorf("cli: result path must be clean: %q", path)
	}
	if filepath.Ext(path) != extension {
		return "", nil, fmt.Errorf("cli: result path %q must use %s", path, extension)
	}

	relative, err := filepath.Rel(adapter.runDir, path)
	if err != nil {
		return "", nil, fmt.Errorf("cli: compare result path %q: %w", path, err)
	}
	if relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", nil, fmt.Errorf("cli: result path %q is outside run directory %q", path, adapter.runDir)
	}

	parts := strings.Split(relative, string(filepath.Separator))
	current := adapter.runDir
	var finalInfo os.FileInfo
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		isFinal := index == len(parts)-1
		if errors.Is(statErr, os.ErrNotExist) {
			if !isFinal || mustExist {
				return "", nil, fmt.Errorf("cli: required path component %q does not exist", current)
			}
			return path, nil, nil
		}
		if statErr != nil {
			return "", nil, fmt.Errorf("cli: inspect path component %q: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("cli: symlink path component is not allowed: %q", current)
		}
		if !isFinal {
			if !info.IsDir() {
				return "", nil, fmt.Errorf("cli: parent path is not a directory: %q", current)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("cli: result path is not a regular file: %q", current)
		}
		finalInfo = info
	}
	return path, finalInfo, nil
}

func (adapter *OutputAdapter) validateRootDirectories() error {
	for _, directory := range []string{
		adapter.repoRoot,
		filepath.Join(adapter.repoRoot, ".coterix"),
		filepath.Join(adapter.repoRoot, ".coterix", "runs"),
		adapter.runDir,
	} {
		if err := requireRealDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cli: inspect required directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cli: required directory is not a real directory: %q", path)
	}
	return nil
}
