package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ridenow/coterix/internal/cli"
	"github.com/ridenow/coterix/internal/state"
)

const (
	requestFileName = "request.txt"
	configFileName  = "config.snapshot.json"
	stateFileName   = "state.json"
)

// Run is one fixed orchestration run and its persisted source of truth.
type Run struct {
	ID       string
	RepoRoot string
	Dir      string
	Request  string
	Config   cli.Config
	State    *state.State

	adapter *cli.OutputAdapter
}

// CreateRun creates a new run without overwriting any existing run artifact.
// An empty runID is replaced with a time-and-random identifier.
func CreateRun(
	repoRoot string,
	runID string,
	request string,
	config cli.Config,
) (_ *Run, returnErr error) {
	if strings.TrimSpace(request) == "" {
		return nil, fmt.Errorf("pipeline: request must not be empty")
	}

	root, err := resolveRepositoryRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		runID, err = newRunID()
		if err != nil {
			return nil, err
		}
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}

	validatedConfig, snapshot, err := canonicalConfigSnapshot(config)
	if err != nil {
		return nil, err
	}

	coterixDir := filepath.Join(root, ".coterix")
	runsDir := filepath.Join(coterixDir, "runs")
	if err := ensureRealDirectory(coterixDir, 0o700); err != nil {
		return nil, err
	}
	if err := ensureRealDirectory(runsDir, 0o700); err != nil {
		return nil, err
	}

	runDir := filepath.Join(runsDir, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		return nil, fmt.Errorf("pipeline: create run directory %q: %w", runDir, err)
	}
	removeIncomplete := true
	defer func() {
		if returnErr != nil && removeIncomplete {
			returnErr = errors.Join(
				returnErr,
				removeNewRunDirectory(runDir),
			)
		}
	}()

	if err := os.Mkdir(filepath.Join(runDir, "logs"), 0o700); err != nil {
		return nil, fmt.Errorf("pipeline: create run logs directory: %w", err)
	}
	if err := writeExclusive(
		filepath.Join(runDir, requestFileName),
		[]byte(request),
		0o600,
	); err != nil {
		return nil, err
	}
	if err := writeExclusive(
		filepath.Join(runDir, configFileName),
		snapshot,
		0o600,
	); err != nil {
		return nil, err
	}

	current := state.New()
	if err := state.Save(filepath.Join(runDir, stateFileName), current); err != nil {
		return nil, fmt.Errorf("pipeline: save initial state: %w", err)
	}
	adapter, err := cli.NewOutputAdapter(root, runID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: initialize run output adapter: %w", err)
	}

	removeIncomplete = false
	return &Run{
		ID:       runID,
		RepoRoot: root,
		Dir:      runDir,
		Request:  request,
		Config:   validatedConfig,
		State:    current,
		adapter:  adapter,
	}, nil
}

// OpenRun loads only the immutable request/config snapshot and state belonging
// to runID. It never consults the current global config.json.
func OpenRun(repoRoot string, runID string) (*Run, error) {
	root, err := resolveRepositoryRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}

	adapter, err := cli.NewOutputAdapter(root, runID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: open run output adapter: %w", err)
	}
	runDir := adapter.RunDir()

	request, err := readRegularFile(filepath.Join(runDir, requestFileName))
	if err != nil {
		return nil, err
	}
	snapshot, err := readRegularFile(filepath.Join(runDir, configFileName))
	if err != nil {
		return nil, err
	}
	config, err := cli.ParseConfig(snapshot)
	if err != nil {
		return nil, fmt.Errorf("pipeline: parse config snapshot: %w", err)
	}
	encodedState, err := readRegularFile(filepath.Join(runDir, stateFileName))
	if err != nil {
		return nil, err
	}
	current, err := state.Parse(encodedState)
	if err != nil {
		return nil, fmt.Errorf("pipeline: parse state: %w", err)
	}

	return &Run{
		ID:       runID,
		RepoRoot: root,
		Dir:      runDir,
		Request:  string(request),
		Config:   config,
		State:    current,
		adapter:  adapter,
	}, nil
}

// SaveState atomically persists the existing State pointer.
func (run *Run) SaveState() error {
	if run == nil || run.State == nil {
		return fmt.Errorf("pipeline: run and state are required")
	}
	if err := validateRunID(run.ID); err != nil {
		return err
	}
	root, err := resolveRepositoryRoot(run.RepoRoot)
	if err != nil {
		return err
	}
	adapter, err := cli.NewOutputAdapter(root, run.ID)
	if err != nil {
		return fmt.Errorf("pipeline: validate run before saving state: %w", err)
	}
	if adapter.RunDir() != run.Dir {
		return fmt.Errorf("pipeline: run directory no longer matches repository and run id")
	}
	if err := state.Save(filepath.Join(run.Dir, stateFileName), run.State); err != nil {
		return fmt.Errorf("pipeline: save state: %w", err)
	}
	run.adapter = adapter
	return nil
}

func canonicalConfigSnapshot(config cli.Config) (cli.Config, []byte, error) {
	unvalidated, err := json.Marshal(config)
	if err != nil {
		return cli.Config{}, nil, fmt.Errorf("pipeline: encode config snapshot input: %w", err)
	}
	validated, err := cli.ParseConfig(unvalidated)
	if err != nil {
		return cli.Config{}, nil, fmt.Errorf("pipeline: validate config snapshot: %w", err)
	}
	snapshot, err := json.MarshalIndent(validated, "", "  ")
	if err != nil {
		return cli.Config{}, nil, fmt.Errorf("pipeline: encode config snapshot: %w", err)
	}
	return validated, append(snapshot, '\n'), nil
}

func newRunID() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("pipeline: generate run id: %w", err)
	}
	return time.Now().UTC().Format("20060102T150405.000000000Z") +
		"-" + hex.EncodeToString(suffix[:]), nil
}

func validateRunID(runID string) error {
	if runID == "" || runID == "." || runID == ".." ||
		filepath.Clean(runID) != runID || filepath.Base(runID) != runID ||
		strings.ContainsAny(runID, `/\`) {
		return fmt.Errorf("pipeline: run id must be one non-empty path component")
	}
	return nil
}

func resolveRepositoryRoot(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("pipeline: repository root is required")
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("pipeline: resolve repository root: %w", err)
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("pipeline: inspect repository root %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("pipeline: repository root must be a real directory")
	}
	return root, nil
}

func ensureRealDirectory(path string, mode os.FileMode) error {
	err := os.Mkdir(path, mode)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("pipeline: create directory %q: %w", path, err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return fmt.Errorf("pipeline: inspect directory %q: %w", path, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("pipeline: required path is not a real directory: %q", path)
	}
	return nil
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("pipeline: create immutable run file %q: %w", path, err)
	}
	written, writeErr := file.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("pipeline: write immutable run file %q: %w", path, err)
	}
	return nil
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("pipeline: inspect run file %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("pipeline: run file is not a regular file: %q", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pipeline: open run file %q: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("pipeline: inspect opened run file %q: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("pipeline: run file changed before it could be read: %q", path)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("pipeline: read run file %q: %w", path, err)
	}
	return content, nil
}

func removeNewRunDirectory(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("pipeline: remove incomplete run directory %q: %w", path, err)
	}
	return nil
}
