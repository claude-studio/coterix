package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/x/term"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/ui"
)

const defaultSnapshotWidth = 120

const usageText = `Usage:
  coterix run <request>
  coterix run --request-file <path>
  coterix approve <run_id>
  coterix reject <run_id> (--response <s> | --response-file <path>)
  coterix status [<run_id>]
  coterix resume <run_id> [--response <s> | --response-file <path>]
`

type controlPlane interface {
	Run(
		context.Context,
		string,
		string,
	) (pipeline.RunStatus, error)
	Approve(
		context.Context,
		string,
		string,
	) (pipeline.RunStatus, error)
	Reject(
		context.Context,
		string,
		string,
		string,
	) (pipeline.RunStatus, error)
	Resume(
		context.Context,
		string,
		string,
		*string,
	) (pipeline.RunStatus, error)
	Status(
		context.Context,
		string,
		string,
	) ([]pipeline.RunStatus, error)
}

type runDashboard interface {
	Run(context.Context, string, string) (pipeline.RunStatus, error)
	// Open attaches the dashboard to an existing run and dispatches one control
	// operation on it (T15 W1/W2). command is the CLI command name; response is the
	// parsed --response value, or nil when none was given.
	Open(
		ctx context.Context,
		repoRoot string,
		runID string,
		command string,
		response *string,
	) (pipeline.RunStatus, error)
	Interactive() bool
}

type charmDashboard struct {
	executor    pipeline.PlanExecutor
	input       io.Reader
	output      io.Writer
	interactive bool
	width       int
	height      int
}

func (dashboard charmDashboard) Open(
	ctx context.Context,
	repoRoot string,
	runID string,
	command string,
	response *string,
) (pipeline.RunStatus, error) {
	return ui.Open(
		ctx,
		dashboard.executor,
		repoRoot,
		runID,
		command,
		response,
		ui.RunOptions{
			Input:       dashboard.input,
			Output:      dashboard.output,
			Interactive: dashboard.interactive,
			Width:       dashboard.width,
			Height:      dashboard.height,
		},
	)
}

func (dashboard charmDashboard) Run(
	ctx context.Context,
	repoRoot string,
	request string,
) (pipeline.RunStatus, error) {
	return ui.Run(
		ctx,
		dashboard.executor,
		repoRoot,
		request,
		ui.RunOptions{
			Input:       dashboard.input,
			Output:      dashboard.output,
			Interactive: dashboard.interactive,
			Width:       dashboard.width,
			Height:      dashboard.height,
		},
	)
}

func (dashboard charmDashboard) Interactive() bool {
	return dashboard.interactive
}

type usageError struct {
	message string
}

func (failure *usageError) Error() string {
	return failure.message
}

type optionalString struct {
	name     string
	value    string
	provided bool
}

func (option *optionalString) String() string {
	return option.value
}

func (option *optionalString) Set(value string) error {
	if option.provided {
		return fmt.Errorf("%s may be provided only once", option.name)
	}
	option.value = value
	option.provided = true
	return nil
}

func main() {
	os.Exit(runMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func runMain(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	processRunner := runner.New()
	controller := pipeline.NewController(processRunner)
	dashboard := charmDashboard{
		executor:    processRunner,
		input:       os.Stdin,
		output:      stdout,
		interactive: ui.IsInteractive(os.Stdin, stdout),
	}

	exitCode := execute(
		ctx,
		controller,
		".",
		args,
		stdout,
		stderr,
		dashboard,
	)
	if err := processRunner.Close(); err != nil {
		fmt.Fprintf(stderr, "coterix: close runner: %v\n", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	return exitCode
}

func execute(
	ctx context.Context,
	controller controlPlane,
	repoRoot string,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	dashboards ...runDashboard,
) int {
	if ctx == nil {
		ctx = context.Background()
	}

	// A help request is not a usage error: it goes to stdout and exits 0, and it is
	// intercepted before any parsing so `coterix --help` never reaches the "unknown
	// command" branch (T15 W3). `help` is included as a bareword by decision — it
	// prints the same usage and adds no command surface of its own. Bare `coterix`
	// stays a usage error (exit 2): that is a mistake, not a question.
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h", "help":
			_, _ = io.WriteString(stdout, usageText)
			return 0
		}
	}

	var (
		result      any
		err         error
		interactive bool
	)
	switch {
	case len(dashboards) > 0 && len(args) > 0 && args[0] == "run":
		request, parseErr := parseRunArguments(args[1:])
		if parseErr != nil {
			err = parseErr
		} else {
			result, err = dashboards[0].Run(ctx, repoRoot, request)
			interactive = dashboards[0].Interactive()
		}
	// approve/reject/resume run for minutes and used to print nothing until they
	// finished, then a single snapshot. On a TTY they now open the live dashboard
	// (T15 W2). Headless they keep the exact JSON and exit codes they always had, and
	// `status` keeps its one-shot snapshot either way.
	case len(dashboards) > 0 && len(args) > 0 &&
		dashboards[0].Interactive() && isInteractiveControlCommand(args[0]):
		runID, response, parseErr := parseControlEntry(args)
		if parseErr != nil {
			err = parseErr
		} else {
			result, err = dashboards[0].Open(
				ctx,
				repoRoot,
				runID,
				args[0],
				response,
			)
			interactive = true
		}
	default:
		result, err = dispatch(ctx, controller, repoRoot, args)
	}
	if err != nil {
		fmt.Fprintf(stderr, "coterix: %v\n", err)
		var usageFailure *usageError
		if errors.As(err, &usageFailure) {
			_, _ = io.WriteString(stderr, usageText)
			return 2
		}
		return 1
	}

	if interactive {
		return 0
	}
	statuses, presentation, snapshot := snapshotResult(args, result)
	if snapshot && len(dashboards) > 0 && dashboards[0].Interactive() {
		rendered, renderErr := ui.RenderSnapshot(
			statuses,
			outputWidth(stdout),
			presentation,
		)
		if renderErr != nil {
			fmt.Fprintf(
				stderr,
				"coterix: render snapshot output: %v\n",
				renderErr,
			)
			return 1
		}
		if _, writeErr := io.WriteString(stdout, rendered+"\n"); writeErr != nil {
			fmt.Fprintf(
				stderr,
				"coterix: write snapshot output: %v\n",
				writeErr,
			)
			return 1
		}
		return 0
	}
	if err := writeJSON(stdout, result); err != nil {
		fmt.Fprintf(stderr, "coterix: write JSON output: %v\n", err)
		return 1
	}
	return 0
}

func snapshotResult(
	args []string,
	result any,
) ([]pipeline.RunStatus, ui.SnapshotPresentation, bool) {
	if len(args) == 0 {
		return nil, 0, false
	}
	switch args[0] {
	case "approve", "reject", "resume":
		status, ok := result.(pipeline.RunStatus)
		if !ok {
			return nil, 0, false
		}
		return []pipeline.RunStatus{status},
			ui.SnapshotPresentationDetail,
			true
	case "status":
		statuses, ok := result.([]pipeline.RunStatus)
		if !ok {
			return nil, 0, false
		}
		presentation := ui.SnapshotPresentationDetail
		if len(args) == 1 {
			presentation = ui.SnapshotPresentationTable
		}
		return statuses, presentation, true
	default:
		return nil, 0, false
	}
}

func outputWidth(output io.Writer) int {
	file, ok := output.(*os.File)
	if !ok {
		return defaultSnapshotWidth
	}
	width, _, err := term.GetSize(file.Fd())
	if err != nil || width <= 0 {
		return defaultSnapshotWidth
	}
	return width
}

func dispatch(
	ctx context.Context,
	controller controlPlane,
	repoRoot string,
	args []string,
) (any, error) {
	if len(args) == 0 {
		return nil, newUsageError("a command is required")
	}

	switch args[0] {
	case "run":
		request, err := parseRunArguments(args[1:])
		if err != nil {
			return nil, err
		}
		return controller.Run(ctx, repoRoot, request)
	case "approve":
		runID, err := parseRequiredRunID("approve", args[1:])
		if err != nil {
			return nil, err
		}
		return controller.Approve(ctx, repoRoot, runID)
	case "reject":
		runID, response, err := parseRejectArguments(args[1:])
		if err != nil {
			return nil, err
		}
		return controller.Reject(ctx, repoRoot, runID, response)
	case "status":
		runID, err := parseStatusArguments(args[1:])
		if err != nil {
			return nil, err
		}
		statuses, err := controller.Status(ctx, repoRoot, runID)
		if statuses == nil && err == nil {
			statuses = []pipeline.RunStatus{}
		}
		return statuses, err
	case "resume":
		runID, response, err := parseResumeArguments(args[1:])
		if err != nil {
			return nil, err
		}
		return controller.Resume(ctx, repoRoot, runID, response)
	default:
		return nil, newUsageError("unknown command %q", args[0])
	}
}

func parseRunArguments(args []string) (string, error) {
	flags := newFlagSet("run")
	requestFile := &optionalString{name: "--request-file"}
	flags.Var(requestFile, "request-file", "read the request from a file")
	if err := flags.Parse(args); err != nil {
		return "", newUsageError("run: %v", err)
	}

	positional := flags.Args()
	switch {
	case requestFile.provided && len(positional) != 0:
		return "", newUsageError(
			"run: provide exactly one of a request string or --request-file",
		)
	case requestFile.provided:
		content, err := os.ReadFile(requestFile.value)
		if err != nil {
			return "", fmt.Errorf(
				"run: read request file %q: %w",
				requestFile.value,
				err,
			)
		}
		return string(content), nil
	case len(positional) != 1:
		return "", newUsageError(
			"run: provide exactly one request string or --request-file",
		)
	default:
		// A positional request is always literal, even when it names an existing
		// file. File input is selected only by --request-file.
		return positional[0], nil
	}
}

func parseRequiredRunID(command string, args []string) (string, error) {
	if len(args) != 1 {
		return "", newUsageError("%s: exactly one run_id is required", command)
	}
	return args[0], nil
}

func isInteractiveControlCommand(command string) bool {
	switch command {
	case "approve", "reject", "resume":
		return true
	}
	return false
}

// parseControlEntry parses the argv of a control command that is about to open the
// dashboard. It reuses the same parsers the headless path uses so the two cannot
// diverge — only reject's `--response` requirement differs, and that difference is
// the TTY flag itself.
func parseControlEntry(args []string) (string, *string, error) {
	switch args[0] {
	case "approve":
		runID, err := parseRequiredRunID("approve", args[1:])
		return runID, nil, err
	case "reject":
		return parseRejectArgumentsFor(args[1:], true)
	case "resume":
		return parseResumeArguments(args[1:])
	}
	return "", nil, newUsageError("unknown command %q", args[0])
}

func parseRejectArguments(args []string) (string, string, error) {
	runID, response, err := parseRejectArgumentsFor(args, false)
	if err != nil {
		return "", "", err
	}
	return runID, *response, nil
}

// parseRejectArgumentsFor makes `--response` optional on a TTY and required without
// one (T15 W2 · R1 option a, a deliberate reversal of T11 f1's argv objection). A
// bare `coterix reject <id>` in a terminal opens the dashboard and asks for the
// feedback; headless it stays a usage error, so scripts see no change.
func parseRejectArgumentsFor(
	args []string,
	interactive bool,
) (string, *string, error) {
	if len(args) == 0 {
		return "", nil, newUsageError("reject: run_id is required")
	}
	runID := args[0]
	response, err := parseResponseSource("reject", args[1:], !interactive)
	if err != nil {
		return "", nil, err
	}
	return runID, response, nil
}

func parseResumeArguments(args []string) (string, *string, error) {
	if len(args) == 0 {
		return "", nil, newUsageError("resume: run_id is required")
	}
	runID := args[0]
	response, err := parseResponseSource("resume", args[1:], false)
	if err != nil {
		return "", nil, err
	}
	return runID, response, nil
}

func parseStatusArguments(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	default:
		return "", newUsageError("status: at most one run_id is allowed")
	}
}

func parseResponseSource(
	command string,
	args []string,
	required bool,
) (*string, error) {
	flags := newFlagSet(command)
	inline := &optionalString{name: "--response"}
	responseFile := &optionalString{name: "--response-file"}
	flags.Var(inline, "response", "use an inline response")
	flags.Var(responseFile, "response-file", "read the response from a file")
	if err := flags.Parse(args); err != nil {
		return nil, newUsageError("%s: %v", command, err)
	}
	if len(flags.Args()) != 0 {
		return nil, newUsageError("%s: unexpected positional arguments", command)
	}
	if inline.provided && responseFile.provided {
		return nil, newUsageError(
			"%s: --response and --response-file are mutually exclusive",
			command,
		)
	}
	if !inline.provided && !responseFile.provided {
		if required {
			return nil, newUsageError(
				"%s: exactly one of --response or --response-file is required",
				command,
			)
		}
		return nil, nil
	}
	if inline.provided {
		response := inline.value
		return &response, nil
	}

	content, err := os.ReadFile(responseFile.value)
	if err != nil {
		return nil, fmt.Errorf(
			"%s: read response file %q: %w",
			command,
			responseFile.value,
			err,
		)
	}
	response := string(content)
	return &response, nil
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func newUsageError(format string, arguments ...any) error {
	return &usageError{message: fmt.Sprintf(format, arguments...)}
}

func writeJSON(destination io.Writer, value any) error {
	encoder := json.NewEncoder(destination)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
