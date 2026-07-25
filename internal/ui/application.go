package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"github.com/ridenow/coterix/internal/pipeline"
)

// RunOptions controls terminal I/O without changing the public Coterix command
// surface. Non-interactive runs still pass through the dashboard model, but
// stop at the first control boundary so approve/reject/resume remain available
// as ordinary CLI commands.
type RunOptions struct {
	Input       io.Reader
	Output      io.Writer
	Interactive bool
	Width       int
	Height      int
}

// IsInteractive reports whether both streams are terminals suitable for an
// interactive Bubble Tea session.
func IsInteractive(input io.Reader, output io.Writer) bool {
	in, inputOK := input.(*os.File)
	out, outputOK := output.(*os.File)
	return inputOK && outputOK &&
		term.IsTerminal(in.Fd()) &&
		term.IsTerminal(out.Fd())
}

// Run starts the existing pipeline Controller inside the dashboard. Every
// human action in the model calls this same Controller instance.
func Run(
	ctx context.Context,
	executor pipeline.PlanExecutor,
	repoRoot string,
	request string,
	options RunOptions,
) (pipeline.RunStatus, error) {
	return runDashboard(ctx, executor, repoRoot, options, nil, request)
}

// Open attaches the dashboard to an existing run and dispatches one operation on it
// — the interactive form of `coterix approve|reject|resume <run_id>` (T15 W1).
//
// `Run` starts from a new request; `Open` starts from a run that already exists. Both
// go through the same program and the same control plane; only the seeding differs.
// `command` is the CLI command name — "approve", "reject" or "resume" — because that
// is what the caller already has in hand; `response` is the parsed `--response` value
// or nil when none was given.
func Open(
	ctx context.Context,
	executor pipeline.PlanExecutor,
	repoRoot string,
	runID string,
	command string,
	response *string,
	options RunOptions,
) (pipeline.RunStatus, error) {
	if runID == "" {
		return pipeline.RunStatus{}, fmt.Errorf("ui: a run id is required")
	}
	kind, err := operationKindForCommand(command)
	if err != nil {
		return pipeline.RunStatus{}, err
	}
	return runDashboard(ctx, executor, repoRoot, options, &openSeed{
		runID: runID,
		initial: initialOperation{
			Kind:     kind,
			Response: copyResponse(response),
		},
	}, "")
}

// copyResponse defends the model from a caller that reuses its buffer.
func copyResponse(response *string) *string {
	if response == nil {
		return nil
	}
	copied := *response
	return &copied
}

func operationKindForCommand(command string) (operationKind, error) {
	switch command {
	case "approve":
		return operationApprove, nil
	case "reject":
		return operationReject, nil
	case "resume":
		return operationResume, nil
	}
	return "", fmt.Errorf(
		"ui: %q cannot open an existing run interactively",
		command,
	)
}

// openSeed carries what Open needs into the shared body.
type openSeed struct {
	runID   string
	initial initialOperation
}

func runDashboard(
	ctx context.Context,
	executor pipeline.PlanExecutor,
	repoRoot string,
	options RunOptions,
	seed *openSeed,
	request string,
) (pipeline.RunStatus, error) {
	if executor == nil {
		return pipeline.RunStatus{}, fmt.Errorf("ui: a pipeline executor is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Output == nil {
		options.Output = os.Stdout
	}
	if options.Input == nil && options.Interactive {
		options.Input = os.Stdin
	}
	explicitWindowSize := options.Width > 0 && options.Height > 0
	if options.Width <= 0 {
		options.Width = wideBreakpointWidth
	}
	if options.Height <= 0 {
		options.Height = wideBreakpointHeight
	}

	theme, err := loadTheme()
	if err != nil {
		return pipeline.RunStatus{}, err
	}

	operationContext, cancelOperations := context.WithCancel(ctx)
	defer cancelOperations()

	var program *tea.Program
	observer := func(event pipeline.Event) {
		if program != nil {
			program.Send(pipelineEventMsg{Event: event})
		}
	}
	controller := pipeline.NewController(
		executor,
		pipeline.WithObserver(observer),
	)
	tracker := &operationTracker{}
	var initial model
	if seed != nil {
		initial = newOpenModel(
			operationContext,
			cancelOperations,
			controller,
			repoRoot,
			seed.runID,
			seed.initial,
			theme,
			!options.Interactive,
			tracker,
		)
	} else {
		initial = newModel(
			operationContext,
			cancelOperations,
			controller,
			repoRoot,
			request,
			theme,
			!options.Interactive,
			tracker,
		)
	}

	programOptions := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithOutput(options.Output),
	}
	if options.Interactive {
		programOptions = append(programOptions, tea.WithInput(options.Input))
		if explicitWindowSize {
			programOptions = append(
				programOptions,
				tea.WithWindowSize(options.Width, options.Height),
			)
		}
	} else {
		programOptions = append(
			programOptions,
			tea.WithInput(nil),
			tea.WithWindowSize(options.Width, options.Height),
			tea.WithoutRenderer(),
			tea.WithoutSignals(),
		)
	}

	program = tea.NewProgram(initial, programOptions...)
	finalModel, runErr := program.Run()
	cancelOperations()
	tracked, hasTracked := tracker.waitLatest()
	if finalModel == nil {
		if hasTracked {
			return tracked.status, errors.Join(runErr, tracked.err)
		}
		return pipeline.RunStatus{}, runErr
	}
	final, ok := finalModel.(model)
	if !ok {
		return pipeline.RunStatus{}, errors.Join(
			runErr,
			fmt.Errorf("ui: Bubble Tea returned unexpected model %T", finalModel),
		)
	}
	status := final.status
	operationErr := final.operationErr
	if hasTracked {
		if tracked.status.RunID != "" {
			status = tracked.status
		}
		operationErr = tracked.err
	}
	return status, errors.Join(runErr, operationErr)
}
