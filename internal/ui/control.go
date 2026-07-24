package ui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/ridenow/coterix/internal/pipeline"
)

type controlPlane interface {
	Run(context.Context, string, string) (pipeline.RunStatus, error)
	Approve(context.Context, string, string) (pipeline.RunStatus, error)
	Reject(context.Context, string, string, string) (pipeline.RunStatus, error)
	Resume(context.Context, string, string, *string) (pipeline.RunStatus, error)
	Status(context.Context, string, string) ([]pipeline.RunStatus, error)
}

type operationKind string

const (
	operationRun     operationKind = "run"
	operationApprove operationKind = "approve"
	operationReject  operationKind = "reject"
	operationResume  operationKind = "resume"
)

type operationDoneMsg struct {
	kind   operationKind
	status pipeline.RunStatus
	err    error
}

type pipelineEventMsg struct {
	pipeline.Event
}

type trackedOperation struct {
	sequence uint64
	done     chan struct{}
	result   operationDoneMsg
}

type operationTracker struct {
	mu           sync.Mutex
	sequence     uint64
	active       *trackedOperation
	lastSequence uint64
	last         operationDoneMsg
}

func runOperation(
	ctx context.Context,
	controller controlPlane,
	tracker *operationTracker,
	kind operationKind,
	repoRoot string,
	request string,
	runID string,
	response *string,
) tea.Cmd {
	action := func() operationDoneMsg {
		var (
			status pipeline.RunStatus
			err    error
		)
		switch kind {
		case operationRun:
			status, err = controller.Run(ctx, repoRoot, request)
		case operationApprove:
			status, err = controller.Approve(ctx, repoRoot, runID)
		case operationReject:
			if response == nil {
				err = context.Canceled
			} else {
				status, err = controller.Reject(
					ctx,
					repoRoot,
					runID,
					*response,
				)
			}
		case operationResume:
			status, err = controller.Resume(ctx, repoRoot, runID, response)
		}
		return operationDoneMsg{kind: kind, status: status, err: err}
	}
	if tracker == nil {
		return func() tea.Msg {
			return action()
		}
	}
	return tracker.start(action)
}

func (tracker *operationTracker) start(
	action func() operationDoneMsg,
) tea.Cmd {
	tracker.mu.Lock()
	tracker.sequence++
	operation := &trackedOperation{
		sequence: tracker.sequence,
		done:     make(chan struct{}),
	}
	tracker.active = operation
	tracker.mu.Unlock()

	go func() {
		result := action()
		tracker.mu.Lock()
		operation.result = result
		tracker.lastSequence = operation.sequence
		tracker.last = result
		if tracker.active == operation {
			tracker.active = nil
		}
		close(operation.done)
		tracker.mu.Unlock()
	}()

	return func() tea.Msg {
		<-operation.done
		return operation.result
	}
}

func (tracker *operationTracker) waitLatest() (operationDoneMsg, bool) {
	if tracker == nil {
		return operationDoneMsg{}, false
	}
	tracker.mu.Lock()
	active := tracker.active
	tracker.mu.Unlock()
	if active != nil {
		<-active.done
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.lastSequence == 0 {
		return operationDoneMsg{}, false
	}
	return tracker.last, true
}
