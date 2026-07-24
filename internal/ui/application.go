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
	initial := newModel(
		operationContext,
		cancelOperations,
		controller,
		repoRoot,
		request,
		theme,
		!options.Interactive,
		tracker,
	)

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
