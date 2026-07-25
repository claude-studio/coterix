package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ridenow/coterix/internal/pipeline"
)

const (
	maxArtifactFileBytes int64 = 1 << 20
	maxArtifactDiffBytes int64 = 1 << 20
	maxDiffStderrBytes   int64 = 64 << 10
	diffContextLines           = 3
	diffTimeout                = 5 * time.Second
)

type evidenceOutcome uint8

const (
	evidenceUnknown evidenceOutcome = iota
	evidencePass
	evidenceFail
)

type namedVerdict struct {
	Name string
	JSON string
}

type artifactData struct {
	PlanMarkdown string
	DiffContent  *string
	// ChangedFiles is `base..candidate` name + ±counts for the STATUS card (T13 W5).
	// Derived from the same git call shape as DiffContent, so its timeout, byte cap and
	// object-id validation are shared.
	ChangedFiles  []changedFile
	Verdicts      []namedVerdict
	GateOutcome   evidenceOutcome
	ReviewOutcome evidenceOutcome
}

// changedFile is one row of `git diff --numstat`.
type changedFile struct {
	Path      string
	Additions int
	Deletions int
}

func loadArtifactData(
	ctx context.Context,
	repoRoot string,
	status pipeline.RunStatus,
) (artifactData, error) {
	data := artifactData{
		Verdicts:      make([]namedVerdict, 0, 2),
		GateOutcome:   evidenceUnknown,
		ReviewOutcome: evidenceUnknown,
	}

	root, runDir, err := resolveArtifactRun(repoRoot, status.RunID)
	if err != nil {
		return artifactData{}, err
	}

	plan, exists, err := readOptionalRunArtifact(
		runDir,
		"plan.md",
		maxArtifactFileBytes,
	)
	if err != nil {
		return artifactData{}, fmt.Errorf("ui: load plan artifact: %w", err)
	}
	if exists {
		data.PlanMarkdown = string(plan)
	}

	planVerdict, exists, err := readOptionalRunArtifact(
		runDir,
		"plan-review.json",
		maxArtifactFileBytes,
	)
	if err != nil {
		return artifactData{}, fmt.Errorf("ui: load plan verdict: %w", err)
	}
	if exists {
		data.Verdicts = append(data.Verdicts, namedVerdict{
			Name: "plan-review.json",
			JSON: string(planVerdict),
		})
	}

	if status.CurrentTaskID == nil {
		return data, nil
	}
	task, exists := status.Tasks[*status.CurrentTaskID]
	if !exists {
		return data, nil
	}

	data.DiffContent, err = loadCandidateDiff(
		ctx,
		root,
		task.BaseSHA,
		task.CandidateSHA,
	)
	if err != nil {
		return artifactData{}, fmt.Errorf("ui: load candidate diff: %w", err)
	}
	data.ChangedFiles, err = loadChangedFiles(
		ctx,
		root,
		task.BaseSHA,
		task.CandidateSHA,
	)
	if err != nil {
		return artifactData{}, fmt.Errorf("ui: load changed files: %w", err)
	}

	if task.GateResult != nil {
		content, err := readRunArtifact(
			runDir,
			*task.GateResult,
			maxArtifactFileBytes,
		)
		if err != nil {
			return artifactData{}, fmt.Errorf("ui: load gate evidence: %w", err)
		}
		data.GateOutcome, err = decodeGateOutcome(content)
		if err != nil {
			return artifactData{}, fmt.Errorf("ui: decode gate evidence: %w", err)
		}
	}

	if task.ReviewResult != nil {
		content, err := readRunArtifact(
			runDir,
			*task.ReviewResult,
			maxArtifactFileBytes,
		)
		if err != nil {
			return artifactData{}, fmt.Errorf("ui: load review evidence: %w", err)
		}
		data.ReviewOutcome, err = decodeReviewOutcome(content)
		if err != nil {
			return artifactData{}, fmt.Errorf("ui: decode review evidence: %w", err)
		}
		data.Verdicts = append(data.Verdicts, namedVerdict{
			Name: *task.ReviewResult,
			JSON: string(content),
		})
	}

	return data, nil
}

func resolveArtifactRun(repoRoot, runID string) (string, string, error) {
	if err := validateArtifactRunID(runID); err != nil {
		return "", "", err
	}
	if repoRoot == "" {
		return "", "", fmt.Errorf("ui: repository root is required")
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", "", fmt.Errorf("ui: resolve repository root: %w", err)
	}
	root = filepath.Clean(root)
	if err := requireRealDirectory(root); err != nil {
		return "", "", fmt.Errorf("ui: invalid repository root: %w", err)
	}

	current := root
	for _, component := range []string{".coterix", "runs", runID} {
		current = filepath.Join(current, component)
		if err := requireRealDirectory(current); err != nil {
			return "", "", fmt.Errorf("ui: invalid run directory: %w", err)
		}
	}
	return root, current, nil
}

func validateArtifactRunID(runID string) error {
	if runID == "" || runID == "." || runID == ".." ||
		strings.TrimSpace(runID) != runID ||
		filepath.Clean(runID) != runID ||
		filepath.Base(runID) != runID ||
		strings.ContainsAny(runID, `/\`) {
		return fmt.Errorf("ui: unsafe run id %q", runID)
	}
	return nil
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a real directory", path)
	}
	return nil
}

func readOptionalRunArtifact(
	runDir string,
	relative string,
	maximum int64,
) ([]byte, bool, error) {
	content, err := readRunArtifact(runDir, relative, maximum)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

func readRunArtifact(runDir, relative string, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		return nil, fmt.Errorf("ui: artifact byte limit must be positive")
	}
	if err := validateRunRelativePath(relative); err != nil {
		return nil, err
	}

	components := strings.Split(relative, string(filepath.Separator))
	current := runDir
	var pathInfo os.FileInfo
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("ui: inspect run artifact %q: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("ui: run artifact path contains a symlink: %q", relative)
		}
		if index < len(components)-1 {
			if !info.IsDir() {
				return nil, fmt.Errorf(
					"ui: run artifact parent is not a directory: %q",
					relative,
				)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("ui: run artifact is not a regular file: %q", relative)
		}
		pathInfo = info
	}

	if pathInfo.Size() > maximum {
		return nil, fmt.Errorf(
			"ui: run artifact %q exceeds %d bytes",
			relative,
			maximum,
		)
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, fmt.Errorf("ui: open run artifact %q: %w", relative, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("ui: inspect opened run artifact %q: %w", relative, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("ui: run artifact changed before read: %q", relative)
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("ui: read run artifact %q: %w", relative, err)
	}
	if int64(len(content)) > maximum {
		return nil, fmt.Errorf(
			"ui: run artifact %q exceeds %d bytes",
			relative,
			maximum,
		)
	}
	return content, nil
}

func validateRunRelativePath(relative string) error {
	cleaned := filepath.Clean(relative)
	if relative == "" ||
		filepath.IsAbs(relative) ||
		cleaned != relative ||
		cleaned == "." ||
		cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("ui: unsafe run-relative artifact path %q", relative)
	}
	return nil
}

func decodeGateOutcome(content []byte) (evidenceOutcome, error) {
	var evidence struct {
		Exit     *int  `json:"exit"`
		TimedOut *bool `json:"timed_out"`
	}
	if err := json.Unmarshal(content, &evidence); err != nil {
		return evidenceUnknown, err
	}
	if evidence.Exit == nil || evidence.TimedOut == nil {
		return evidenceUnknown, fmt.Errorf("gate evidence is missing exit or timed_out")
	}
	if *evidence.Exit == 0 && !*evidence.TimedOut {
		return evidencePass, nil
	}
	return evidenceFail, nil
}

func decodeReviewOutcome(content []byte) (evidenceOutcome, error) {
	var evidence struct {
		Clean *bool `json:"clean"`
	}
	if err := json.Unmarshal(content, &evidence); err != nil {
		return evidenceUnknown, err
	}
	if evidence.Clean == nil {
		return evidenceUnknown, fmt.Errorf("review evidence is missing clean")
	}
	if *evidence.Clean {
		return evidencePass, nil
	}
	return evidenceFail, nil
}

// loadChangedFiles lists the candidate's touched files with ± counts. It mirrors
// loadCandidateDiff's guards — object-id validation, timeout, capped output — because it
// is the same untrusted git surface (T13 W5).
func loadChangedFiles(
	ctx context.Context,
	repoRoot string,
	baseSHA *string,
	candidateSHA *string,
) ([]changedFile, error) {
	if baseSHA == nil || candidateSHA == nil {
		return nil, nil
	}
	if err := validateGitObjectID(*baseSHA); err != nil {
		return nil, fmt.Errorf("invalid base_sha: %w", err)
	}
	if err := validateGitObjectID(*candidateSHA); err != nil {
		return nil, fmt.Errorf("invalid candidate_sha: %w", err)
	}
	if *baseSHA == *candidateSHA {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	statContext, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	// core.quotePath=false is what makes a CJK path readable. Measured (2026-07-25):
	// with git's default, `한글.txt` arrives as `"\355\225\234\352\270\200.txt"` — octal
	// escapes the operator cannot read. With the flag it arrives as raw UTF-8, while a
	// path containing a tab is **still** quoted, so the tab-separated format stays
	// unambiguous and the parse below stays safe.
	command := exec.CommandContext(
		statContext,
		"git",
		"-c",
		"core.quotePath=false",
		"diff",
		"--no-color",
		"--no-ext-diff",
		"--numstat",
		*baseSHA,
		*candidateSHA,
		"--",
	)
	command.Dir = repoRoot
	stdout := newCappedBuffer(maxArtifactDiffBytes)
	stderr := newCappedBuffer(maxDiffStderrBytes)
	command.Stdout = stdout
	command.Stderr = stderr

	runErr := command.Run()
	switch {
	case stdout.exceeded:
		return nil, fmt.Errorf("git diff --numstat exceeds %d bytes", maxArtifactDiffBytes)
	case statContext.Err() != nil:
		return nil, fmt.Errorf("git diff --numstat did not complete: %w", statContext.Err())
	case runErr != nil:
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = runErr.Error()
		}
		return nil, fmt.Errorf("git diff --numstat failed: %s", message)
	}

	files := make([]changedFile, 0, 8)
	for _, line := range strings.Split(stdout.String(), "\n") {
		// Only the line terminator and the count fields are trimmed. A path may legally
		// end in a space, and trimming the whole line silently renamed it (review T13b f3).
		// SplitN keeps a tab inside the path in one piece for the same reason.
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(fields) < 3 || fields[2] == "" {
			continue
		}
		// Binary files report "-" for both counts; keep the row, drop the numbers.
		additions, _ := strconv.Atoi(strings.TrimSpace(fields[0]))
		deletions, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
		files = append(files, changedFile{
			Path:      fields[2],
			Additions: additions,
			Deletions: deletions,
		})
	}
	return files, nil
}

func loadCandidateDiff(
	ctx context.Context,
	repoRoot string,
	baseSHA *string,
	candidateSHA *string,
) (*string, error) {
	if baseSHA == nil || candidateSHA == nil {
		return nil, nil
	}
	if err := validateGitObjectID(*baseSHA); err != nil {
		return nil, fmt.Errorf("invalid base_sha: %w", err)
	}
	if err := validateGitObjectID(*candidateSHA); err != nil {
		return nil, fmt.Errorf("invalid candidate_sha: %w", err)
	}
	if *baseSHA == *candidateSHA {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	diffContext, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	command := exec.CommandContext(
		diffContext,
		"git",
		"diff",
		"--no-color",
		"--no-ext-diff",
		"--no-textconv",
		fmt.Sprintf("--unified=%d", diffContextLines),
		*baseSHA,
		*candidateSHA,
		"--",
	)
	command.Dir = repoRoot
	stdout := newCappedBuffer(maxArtifactDiffBytes)
	stderr := newCappedBuffer(maxDiffStderrBytes)
	command.Stdout = stdout
	command.Stderr = stderr

	runErr := command.Run()
	switch {
	case stdout.exceeded:
		return nil, fmt.Errorf("git diff exceeds %d bytes", maxArtifactDiffBytes)
	case stderr.exceeded:
		return nil, fmt.Errorf("git diff stderr exceeds %d bytes", maxDiffStderrBytes)
	case diffContext.Err() != nil:
		return nil, fmt.Errorf("git diff did not complete: %w", diffContext.Err())
	case runErr != nil:
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = runErr.Error()
		}
		return nil, fmt.Errorf("git diff failed: %s", message)
	}

	content := stdout.String()
	if content == "" {
		return nil, nil
	}
	return &content, nil
}

func validateGitObjectID(value string) error {
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("must be a full git object id")
	}
	for _, character := range value {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return fmt.Errorf("must contain only lowercase hexadecimal characters")
		}
	}
	return nil
}

type cappedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
}

func newCappedBuffer(limit int64) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (buffer *cappedBuffer) Write(content []byte) (int, error) {
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining <= 0 {
		buffer.exceeded = true
		return 0, fmt.Errorf("buffer exceeds %d bytes", buffer.limit)
	}
	if int64(len(content)) <= remaining {
		return buffer.buffer.Write(content)
	}
	written, _ := buffer.buffer.Write(content[:remaining])
	buffer.exceeded = true
	return written, fmt.Errorf("buffer exceeds %d bytes", buffer.limit)
}

func (buffer *cappedBuffer) String() string {
	return buffer.buffer.String()
}
