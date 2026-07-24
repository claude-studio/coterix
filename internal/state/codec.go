package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type stateWire struct {
	SchemaVersion    *int                       `json:"schema_version"`
	Phase            *Phase                     `json:"phase"`
	PlanHash         json.RawMessage            `json:"plan_hash"`
	ApprovedPlanHash json.RawMessage            `json:"approved_plan_hash"`
	PlanRound        *int                       `json:"plan_round"`
	PendingAction    json.RawMessage            `json:"pending_action"`
	TaskOrder        *[]string                  `json:"task_order"`
	CurrentTaskID    json.RawMessage            `json:"current_task_id"`
	Tasks            *map[string]*taskStateWire `json:"tasks"`
	LastError        json.RawMessage            `json:"last_error"`
}

type taskStateWire struct {
	Status       *TaskStatus     `json:"status"`
	Attempt      *int            `json:"attempt"`
	BaseSHA      json.RawMessage `json:"base_sha"`
	CandidateSHA json.RawMessage `json:"candidate_sha"`
	GateResult   json.RawMessage `json:"gate_result"`
	ReviewResult json.RawMessage `json:"review_result"`
}

type pendingActionWire struct {
	Kind        *PendingKind    `json:"kind"`
	ResumePhase *Phase          `json:"resume_phase"`
	TaskID      json.RawMessage `json:"task_id"`
	Prompt      *string         `json:"prompt"`
	Response    json.RawMessage `json:"response"`
}

// Parse strictly decodes and validates one state.json document.
func Parse(content []byte) (*State, error) {
	var wire *stateWire
	if err := decodeStrictJSON(content, &wire); err != nil {
		return nil, fmt.Errorf("state: decode state.json: %w", err)
	}
	if wire == nil {
		return nil, fmt.Errorf("state: state.json must be an object")
	}
	if err := requireStateFields(wire); err != nil {
		return nil, err
	}

	planHash, err := decodeNullableString(wire.PlanHash, "plan_hash")
	if err != nil {
		return nil, err
	}
	approvedPlanHash, err := decodeNullableString(
		wire.ApprovedPlanHash,
		"approved_plan_hash",
	)
	if err != nil {
		return nil, err
	}
	currentTaskID, err := decodeNullableString(wire.CurrentTaskID, "current_task_id")
	if err != nil {
		return nil, err
	}
	lastError, err := decodeNullableString(wire.LastError, "last_error")
	if err != nil {
		return nil, err
	}
	pendingAction, err := resolvePendingAction(wire.PendingAction)
	if err != nil {
		return nil, err
	}

	tasks := make(map[string]*TaskState, len(*wire.Tasks))
	for taskID, taskWire := range *wire.Tasks {
		task, err := resolveTaskState(taskID, taskWire)
		if err != nil {
			return nil, err
		}
		tasks[taskID] = task
	}
	current := &State{
		SchemaVersion:    *wire.SchemaVersion,
		Phase:            *wire.Phase,
		PlanHash:         planHash,
		ApprovedPlanHash: approvedPlanHash,
		PlanRound:        *wire.PlanRound,
		PendingAction:    pendingAction,
		TaskOrder:        append([]string(nil), (*wire.TaskOrder)...),
		CurrentTaskID:    currentTaskID,
		Tasks:            tasks,
		LastError:        lastError,
	}
	if current.TaskOrder == nil {
		current.TaskOrder = make([]string, 0)
	}
	if err := current.Validate(); err != nil {
		return nil, err
	}
	return current, nil
}

// Encode validates state and returns its canonical indented JSON form.
func Encode(current *State) ([]byte, error) {
	if err := current.Validate(); err != nil {
		return nil, err
	}
	content, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("state: encode state.json: %w", err)
	}
	return append(content, '\n'), nil
}

// Load reads and validates state.json.
func Load(path string) (*State, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("state: read %q: %w", path, err)
	}
	return Parse(content)
}

// Save atomically replaces state.json using a temporary file in the target
// directory. It intentionally does not add fsync, journaling, or a reducer.
func Save(path string, current *State) error {
	if path == "" || filepath.Base(path) == "." {
		return fmt.Errorf("state: state.json path is required")
	}
	content, err := Encode(current)
	if err != nil {
		return err
	}

	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("state: create temporary state file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
	}()

	written, writeErr := temporary.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		closeErr := temporary.Close()
		return fmt.Errorf(
			"state: write temporary state file: %w",
			errors.Join(writeErr, closeErr),
		)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("state: close temporary state file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("state: replace %q: %w", path, err)
	}
	temporaryPath = ""
	return nil
}

func requireStateFields(wire *stateWire) error {
	switch {
	case wire.SchemaVersion == nil:
		return missingField("schema_version")
	case wire.Phase == nil:
		return missingField("phase")
	case wire.PlanHash == nil:
		return missingField("plan_hash")
	case wire.ApprovedPlanHash == nil:
		return missingField("approved_plan_hash")
	case wire.PlanRound == nil:
		return missingField("plan_round")
	case wire.PendingAction == nil:
		return missingField("pending_action")
	case wire.TaskOrder == nil:
		return missingField("task_order")
	case wire.CurrentTaskID == nil:
		return missingField("current_task_id")
	case wire.Tasks == nil:
		return missingField("tasks")
	case wire.LastError == nil:
		return missingField("last_error")
	default:
		return nil
	}
}

func resolveTaskState(taskID string, wire *taskStateWire) (*TaskState, error) {
	if wire == nil {
		return nil, fmt.Errorf("state: task %q must be an object, not null", taskID)
	}
	switch {
	case wire.Status == nil:
		return nil, missingTaskField(taskID, "status")
	case wire.Attempt == nil:
		return nil, missingTaskField(taskID, "attempt")
	case wire.BaseSHA == nil:
		return nil, missingTaskField(taskID, "base_sha")
	case wire.CandidateSHA == nil:
		return nil, missingTaskField(taskID, "candidate_sha")
	case wire.GateResult == nil:
		return nil, missingTaskField(taskID, "gate_result")
	case wire.ReviewResult == nil:
		return nil, missingTaskField(taskID, "review_result")
	}

	baseSHA, err := decodeNullableString(wire.BaseSHA, "task "+taskID+" base_sha")
	if err != nil {
		return nil, err
	}
	candidateSHA, err := decodeNullableString(
		wire.CandidateSHA,
		"task "+taskID+" candidate_sha",
	)
	if err != nil {
		return nil, err
	}
	gateResult, err := decodeNullableString(
		wire.GateResult,
		"task "+taskID+" gate_result",
	)
	if err != nil {
		return nil, err
	}
	reviewResult, err := decodeNullableString(
		wire.ReviewResult,
		"task "+taskID+" review_result",
	)
	if err != nil {
		return nil, err
	}
	return &TaskState{
		Status:       *wire.Status,
		Attempt:      *wire.Attempt,
		BaseSHA:      baseSHA,
		CandidateSHA: candidateSHA,
		GateResult:   gateResult,
		ReviewResult: reviewResult,
	}, nil
}

func resolvePendingAction(content json.RawMessage) (*PendingAction, error) {
	if isJSONNull(content) {
		return nil, nil
	}
	var wire *pendingActionWire
	if err := decodeStrictJSON(content, &wire); err != nil {
		return nil, fmt.Errorf("state: decode pending_action: %w", err)
	}
	if wire == nil {
		return nil, fmt.Errorf("state: pending_action must be an object or null")
	}
	switch {
	case wire.Kind == nil:
		return nil, missingPendingField("kind")
	case wire.ResumePhase == nil:
		return nil, missingPendingField("resume_phase")
	case wire.TaskID == nil:
		return nil, missingPendingField("task_id")
	case wire.Prompt == nil:
		return nil, missingPendingField("prompt")
	case wire.Response == nil:
		return nil, missingPendingField("response")
	}
	taskID, err := decodeNullableString(wire.TaskID, "pending_action task_id")
	if err != nil {
		return nil, err
	}
	response, err := decodeNullableString(wire.Response, "pending_action response")
	if err != nil {
		return nil, err
	}
	return &PendingAction{
		Kind:        *wire.Kind,
		ResumePhase: *wire.ResumePhase,
		TaskID:      taskID,
		Prompt:      *wire.Prompt,
		Response:    response,
	}, nil
}

func decodeNullableString(content json.RawMessage, field string) (*string, error) {
	if content == nil {
		return nil, missingField(field)
	}
	if isJSONNull(content) {
		return nil, nil
	}
	var value string
	if err := decodeStrictJSON(content, &value); err != nil {
		return nil, fmt.Errorf("state: %s must be a string or null: %w", field, err)
	}
	return &value, nil
}

func isJSONNull(content []byte) bool {
	return bytes.Equal(bytes.TrimSpace(content), []byte("null"))
}

func decodeStrictJSON(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func missingField(field string) error {
	return fmt.Errorf("state: required field %q is missing or null", field)
}

func missingTaskField(taskID, field string) error {
	return fmt.Errorf("state: task %q required field %q is missing or null", taskID, field)
}

func missingPendingField(field string) error {
	return fmt.Errorf(
		"state: pending_action required field %q is missing or null",
		field,
	)
}
