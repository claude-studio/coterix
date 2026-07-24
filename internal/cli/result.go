package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ridenow/coterix/internal/runner"
)

// StepResult is one of the role-specific output contracts.
type StepResult interface {
	stepResult()
}

// PlanFile is a validated planner artifact. Content is retained so downstream
// code does not reread the agent-produced file.
type PlanFile struct {
	Path    string
	Content []byte
}

func (PlanFile) stepResult() {}

// Questions is a non-empty planner question artifact.
type Questions struct {
	Path    string
	Content []byte
}

func (Questions) stepResult() {}

// ReviewKind distinguishes the two fixed review schemas.
type ReviewKind string

const (
	ReviewKindPlan           ReviewKind = "plan"
	ReviewKindImplementation ReviewKind = "implementation"
)

// Severity is a fixed review finding severity.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
)

// ReviewFinding is the normalized finding shape for either review schema.
type ReviewFinding struct {
	ID              string
	Severity        Severity
	TaskID          *string
	Location        *string
	Issue           string
	RequestedChange string
}

// ReviewVerdict is the normalized structured review verdict.
type ReviewVerdict struct {
	SchemaVersion  int
	Kind           ReviewKind
	TargetPlanHash string
	PlanHash       string
	TaskID         string
	CandidateSHA   string
	Clean          bool
	Findings       []ReviewFinding
}

// ReviewJSON contains one strictly decoded review artifact.
type ReviewJSON struct {
	Path    string
	Content []byte
	Verdict ReviewVerdict
}

func (ReviewJSON) stepResult() {}

// CommittedCandidate is a clean committed HEAD different from its base.
type CommittedCandidate struct {
	BaseSHA      string
	CandidateSHA string
}

func (CommittedCandidate) stepResult() {}

// PlanStructureValidator belongs to the plan-cycle layer. The CLI adapter
// requires it without implementing the M2 plan grammar itself.
type PlanStructureValidator func([]byte) error

// ValidatePlannerResult consumes exactly one of plan output or questions.
func (attempt *OutputAttempt) ValidatePlannerResult(
	role Role,
	paths ResultPaths,
	validatePlan PlanStructureValidator,
) (StepResult, error) {
	if role != RolePlanWriter && role != RolePlanReviser {
		return nil, fmt.Errorf("cli: role %q does not produce planner results", role)
	}
	if _, err := attempt.adapter.PolicyForRole(role, paths); err != nil {
		return nil, err
	}

	planExists, err := attempt.adapter.resultExists(paths.PlanOutput, ".md")
	if err != nil {
		return nil, err
	}
	questionsExist, err := attempt.adapter.resultExists(paths.Questions, ".md")
	if err != nil {
		return nil, err
	}
	if planExists == questionsExist {
		if planExists {
			return nil, fmt.Errorf("cli: planner produced both plan and questions results")
		}
		return nil, fmt.Errorf("cli: planner produced neither plan nor questions result")
	}

	if questionsExist {
		content, err := attempt.Consume(paths.Questions, ".md")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(content)) == "" {
			return nil, fmt.Errorf("cli: questions result is empty")
		}
		return Questions{Path: paths.Questions, Content: content}, nil
	}

	if validatePlan == nil {
		return nil, fmt.Errorf("cli: a plan structure validator is required")
	}
	content, err := attempt.Consume(paths.PlanOutput, ".md")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(content)) == "" {
		return nil, fmt.Errorf("cli: plan result is empty")
	}
	if err := validatePlan(append([]byte(nil), content...)); err != nil {
		return nil, fmt.Errorf("cli: plan structure validation failed: %w", err)
	}
	return PlanFile{Path: paths.PlanOutput, Content: content}, nil
}

// ValidateReviewResult consumes and strictly validates the role's review JSON.
func (attempt *OutputAttempt) ValidateReviewResult(role Role, path string) (ReviewJSON, error) {
	if role != RolePlanReviewer && role != RoleImplReviewer {
		return ReviewJSON{}, fmt.Errorf("cli: role %q does not produce review JSON", role)
	}
	if _, err := attempt.adapter.PolicyForRole(role, ResultPaths{Review: path}); err != nil {
		return ReviewJSON{}, err
	}
	content, err := attempt.Consume(path, ".json")
	if err != nil {
		return ReviewJSON{}, err
	}

	var verdict ReviewVerdict
	switch role {
	case RolePlanReviewer:
		verdict, err = decodePlanReview(content)
	case RoleImplReviewer:
		verdict, err = decodeImplementationReview(content)
	}
	if err != nil {
		return ReviewJSON{}, fmt.Errorf("cli: validate review result: %w", err)
	}
	return ReviewJSON{Path: path, Content: content, Verdict: verdict}, nil
}

// ValidateCommittedCandidate verifies the mutating role's postcondition.
func ValidateCommittedCandidate(
	ctx context.Context,
	role Role,
	repoDir string,
	baseSHA string,
) (CommittedCandidate, error) {
	if role != RoleImplWriter && role != RoleFixer {
		return CommittedCandidate{}, fmt.Errorf(
			"cli: role %q does not produce a committed candidate",
			role,
		)
	}
	if !gitObjectIDPattern.MatchString(baseSHA) {
		return CommittedCandidate{}, fmt.Errorf("cli: base SHA must be a full git object id")
	}
	snapshot, err := (runner.GitMutationGuard{}).Capture(ctx, repoDir)
	if err != nil {
		return CommittedCandidate{}, fmt.Errorf("cli: validate committed candidate: %w", err)
	}
	if snapshot.Head == baseSHA {
		return CommittedCandidate{}, fmt.Errorf("cli: candidate HEAD must differ from base SHA")
	}
	return CommittedCandidate{
		BaseSHA:      baseSHA,
		CandidateSHA: snapshot.Head,
	}, nil
}

var gitObjectIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type planReviewWire struct {
	SchemaVersion  *int               `json:"schema_version"`
	TargetPlanHash *string            `json:"target_plan_hash"`
	Clean          *bool              `json:"clean"`
	Findings       *[]planFindingWire `json:"findings"`
}

type planFindingWire struct {
	ID              *string         `json:"id"`
	Severity        *string         `json:"severity"`
	TaskID          json.RawMessage `json:"task_id"`
	Issue           *string         `json:"issue"`
	RequestedChange *string         `json:"requested_change"`
}

type implementationReviewWire struct {
	SchemaVersion *int                         `json:"schema_version"`
	PlanHash      *string                      `json:"plan_hash"`
	TaskID        *string                      `json:"task_id"`
	CandidateSHA  *string                      `json:"candidate_sha"`
	Clean         *bool                        `json:"clean"`
	Findings      *[]implementationFindingWire `json:"findings"`
}

type implementationFindingWire struct {
	ID              *string         `json:"id"`
	Severity        *string         `json:"severity"`
	Location        json.RawMessage `json:"location"`
	Issue           *string         `json:"issue"`
	RequestedChange *string         `json:"requested_change"`
}

func decodePlanReview(content []byte) (ReviewVerdict, error) {
	var wire *planReviewWire
	if err := decodeStrictJSON(content, &wire); err != nil {
		return ReviewVerdict{}, err
	}
	if wire == nil {
		return ReviewVerdict{}, fmt.Errorf("review must be a JSON object")
	}
	if wire.SchemaVersion == nil || wire.TargetPlanHash == nil ||
		wire.Clean == nil || wire.Findings == nil {
		return ReviewVerdict{}, fmt.Errorf("plan review is missing required fields")
	}
	if *wire.SchemaVersion != 1 {
		return ReviewVerdict{}, fmt.Errorf("unsupported schema_version %d", *wire.SchemaVersion)
	}
	if strings.TrimSpace(*wire.TargetPlanHash) == "" {
		return ReviewVerdict{}, fmt.Errorf("target_plan_hash must not be empty")
	}

	findings := make([]ReviewFinding, 0, len(*wire.Findings))
	for index, finding := range *wire.Findings {
		normalized, err := normalizePlanFinding(finding)
		if err != nil {
			return ReviewVerdict{}, fmt.Errorf("finding %d: %w", index, err)
		}
		findings = append(findings, normalized)
	}
	if err := validateReviewConsistency(*wire.Clean, findings); err != nil {
		return ReviewVerdict{}, err
	}
	return ReviewVerdict{
		SchemaVersion:  1,
		Kind:           ReviewKindPlan,
		TargetPlanHash: *wire.TargetPlanHash,
		Clean:          *wire.Clean,
		Findings:       findings,
	}, nil
}

func decodeImplementationReview(content []byte) (ReviewVerdict, error) {
	var wire *implementationReviewWire
	if err := decodeStrictJSON(content, &wire); err != nil {
		return ReviewVerdict{}, err
	}
	if wire == nil {
		return ReviewVerdict{}, fmt.Errorf("review must be a JSON object")
	}
	if wire.SchemaVersion == nil || wire.PlanHash == nil || wire.TaskID == nil ||
		wire.CandidateSHA == nil || wire.Clean == nil || wire.Findings == nil {
		return ReviewVerdict{}, fmt.Errorf("implementation review is missing required fields")
	}
	if *wire.SchemaVersion != 1 {
		return ReviewVerdict{}, fmt.Errorf("unsupported schema_version %d", *wire.SchemaVersion)
	}
	if strings.TrimSpace(*wire.PlanHash) == "" ||
		strings.TrimSpace(*wire.TaskID) == "" ||
		strings.TrimSpace(*wire.CandidateSHA) == "" {
		return ReviewVerdict{}, fmt.Errorf("plan_hash, task_id, and candidate_sha must not be empty")
	}

	findings := make([]ReviewFinding, 0, len(*wire.Findings))
	for index, finding := range *wire.Findings {
		normalized, err := normalizeImplementationFinding(finding)
		if err != nil {
			return ReviewVerdict{}, fmt.Errorf("finding %d: %w", index, err)
		}
		findings = append(findings, normalized)
	}
	if err := validateReviewConsistency(*wire.Clean, findings); err != nil {
		return ReviewVerdict{}, err
	}
	return ReviewVerdict{
		SchemaVersion: 1,
		Kind:          ReviewKindImplementation,
		PlanHash:      *wire.PlanHash,
		TaskID:        *wire.TaskID,
		CandidateSHA:  *wire.CandidateSHA,
		Clean:         *wire.Clean,
		Findings:      findings,
	}, nil
}

func normalizePlanFinding(wire planFindingWire) (ReviewFinding, error) {
	if wire.ID == nil || wire.Severity == nil || wire.TaskID == nil ||
		wire.Issue == nil || wire.RequestedChange == nil {
		return ReviewFinding{}, fmt.Errorf("missing required fields")
	}
	taskID, err := decodeNullableString(wire.TaskID)
	if err != nil {
		return ReviewFinding{}, fmt.Errorf("task_id: %w", err)
	}
	return normalizeFinding(
		*wire.ID,
		*wire.Severity,
		taskID,
		nil,
		*wire.Issue,
		*wire.RequestedChange,
	)
}

func normalizeImplementationFinding(wire implementationFindingWire) (ReviewFinding, error) {
	if wire.ID == nil || wire.Severity == nil || wire.Location == nil ||
		wire.Issue == nil || wire.RequestedChange == nil {
		return ReviewFinding{}, fmt.Errorf("missing required fields")
	}
	location, err := decodeNullableString(wire.Location)
	if err != nil {
		return ReviewFinding{}, fmt.Errorf("location: %w", err)
	}
	if err := validateFindingLocation(location); err != nil {
		return ReviewFinding{}, fmt.Errorf("location: %w", err)
	}
	return normalizeFinding(
		*wire.ID,
		*wire.Severity,
		nil,
		location,
		*wire.Issue,
		*wire.RequestedChange,
	)
}

func validateFindingLocation(location *string) error {
	if location == nil {
		return nil
	}
	value := *location
	separator := strings.LastIndexByte(value, ':')
	if separator <= 0 || separator == len(value)-1 {
		return fmt.Errorf("must use path:line format")
	}
	path, line := value[:separator], value[separator+1:]
	if strings.TrimSpace(path) != path || path == "" {
		return fmt.Errorf("must contain a non-empty path")
	}
	for index, character := range line {
		if character < '0' || character > '9' || (index == 0 && character == '0') {
			return fmt.Errorf("line must be a positive decimal integer")
		}
	}
	return nil
}

func normalizeFinding(
	id string,
	severity string,
	taskID *string,
	location *string,
	issue string,
	requestedChange string,
) (ReviewFinding, error) {
	if strings.TrimSpace(id) == "" ||
		strings.TrimSpace(issue) == "" ||
		strings.TrimSpace(requestedChange) == "" {
		return ReviewFinding{}, fmt.Errorf("id, issue, and requested_change must not be empty")
	}
	normalizedSeverity := Severity(severity)
	switch normalizedSeverity {
	case SeverityCritical, SeverityMajor, SeverityMinor:
	default:
		return ReviewFinding{}, fmt.Errorf("invalid severity %q", severity)
	}
	return ReviewFinding{
		ID:              id,
		Severity:        normalizedSeverity,
		TaskID:          taskID,
		Location:        location,
		Issue:           issue,
		RequestedChange: requestedChange,
	}, nil
}

func decodeNullableString(content json.RawMessage) (*string, error) {
	var value *string
	if err := decodeStrictJSON(content, &value); err != nil {
		return nil, fmt.Errorf("must be a string or null: %w", err)
	}
	if value == nil {
		return nil, nil
	}
	if strings.TrimSpace(*value) == "" {
		return nil, fmt.Errorf("string value must not be empty")
	}
	return value, nil
}

func validateReviewConsistency(clean bool, findings []ReviewFinding) error {
	blocking := 0
	for _, finding := range findings {
		if finding.Severity == SeverityCritical || finding.Severity == SeverityMajor {
			blocking++
		}
	}
	if clean != (blocking == 0) {
		return fmt.Errorf(
			"clean must be true if and only if there are zero blocking findings",
		)
	}
	return nil
}
