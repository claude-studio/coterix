package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GitMutationGuard enforces a resolvable HEAD and clean worktree.
type GitMutationGuard struct{}

// Capture records HEAD when the worktree is clean.
func (GitMutationGuard) Capture(ctx context.Context, dir string) (MutationSnapshot, error) {
	head, err := gitOutput(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return MutationSnapshot{}, err
	}
	if err := requireCleanWorktree(ctx, dir); err != nil {
		return MutationSnapshot{}, err
	}
	return MutationSnapshot{Head: head}, nil
}

// Verify confirms that HEAD and the clean worktree still match the snapshot.
func (GitMutationGuard) Verify(ctx context.Context, dir string, snapshot MutationSnapshot) error {
	head, err := gitOutput(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if head != snapshot.Head {
		return fmt.Errorf("HEAD changed from %s to %s", snapshot.Head, head)
	}
	return requireCleanWorktree(ctx, dir)
}

func requireCleanWorktree(ctx context.Context, dir string) error {
	status, err := gitOutput(ctx, dir, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("worktree is not clean")
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return strings.TrimSpace(string(output)), nil
}
