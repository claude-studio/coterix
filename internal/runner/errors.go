package runner

import "fmt"

// ExitError reports a subprocess that started but exited unsuccessfully.
type ExitError struct {
	Attempt int
	Result  RunResult
	Err     error
}

func (err *ExitError) Error() string {
	return fmt.Sprintf("runner: command exited with code %d on attempt %d", err.Result.Exit, err.Attempt)
}

func (err *ExitError) Unwrap() error {
	return err.Err
}

// TimeoutError reports an idle timeout. The timeout classification takes
// precedence over the signal-related exit caused by killing the process.
type TimeoutError struct {
	Attempt int
	Result  RunResult
	Err     error
}

func (err *TimeoutError) Error() string {
	return fmt.Sprintf("runner: command exceeded its idle timeout on attempt %d", err.Attempt)
}

func (err *TimeoutError) Unwrap() error {
	return err.Err
}

// ResultError reports missing, malformed, stale, or otherwise invalid
// role-specific output after a successful subprocess exit.
type ResultError struct {
	Attempt int
	Result  RunResult
	Err     error
}

func (err *ResultError) Error() string {
	return fmt.Sprintf("runner: result validation failed on attempt %d: %v", err.Attempt, err.Err)
}

func (err *ResultError) Unwrap() error {
	return err.Err
}

// SafetyError reports that an attempt could not be proven to have preserved
// the canonical input or repository snapshot required for a safe retry.
type SafetyError struct {
	Attempt int
	Result  RunResult
	Err     error
}

func (err *SafetyError) Error() string {
	return fmt.Sprintf("runner: command failed safety verification on attempt %d: %v", err.Attempt, err.Err)
}

func (err *SafetyError) Unwrap() error {
	return err.Err
}

// InterruptedError reports cancellation, Runner shutdown, or a watched signal.
type InterruptedError struct {
	Attempt int
	Result  RunResult
	Err     error
}

func (err *InterruptedError) Error() string {
	return fmt.Sprintf("runner: command interrupted on attempt %d: %v", err.Attempt, err.Err)
}

func (err *InterruptedError) Unwrap() error {
	return err.Err
}

// ExecutionError reports setup, start, output preparation, or stream I/O
// failures that are not process exit, timeout, or result validation failures.
type ExecutionError struct {
	Attempt int
	Result  RunResult
	Err     error
}

func (err *ExecutionError) Error() string {
	return fmt.Sprintf("runner: command execution failed on attempt %d: %v", err.Attempt, err.Err)
}

func (err *ExecutionError) Unwrap() error {
	return err.Err
}
