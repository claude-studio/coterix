package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
)

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

	exitCode := execute(ctx, controller, ".", args, stdout, stderr)
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
) int {
	if ctx == nil {
		ctx = context.Background()
	}

	result, err := dispatch(ctx, controller, repoRoot, args)
	if err != nil {
		fmt.Fprintf(stderr, "coterix: %v\n", err)
		var usageFailure *usageError
		if errors.As(err, &usageFailure) {
			_, _ = io.WriteString(stderr, usageText)
			return 2
		}
		return 1
	}

	if err := writeJSON(stdout, result); err != nil {
		fmt.Fprintf(stderr, "coterix: write JSON output: %v\n", err)
		return 1
	}
	return 0
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

func parseRejectArguments(args []string) (string, string, error) {
	if len(args) == 0 {
		return "", "", newUsageError("reject: run_id is required")
	}
	runID := args[0]
	response, err := parseResponseSource("reject", args[1:], true)
	if err != nil {
		return "", "", err
	}
	return runID, *response, nil
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
