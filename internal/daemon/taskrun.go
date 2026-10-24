package daemon

import (
	"context"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Tasks from devices (F7; workstream G).

var _ core.TaskRunner = (*Daemon)(nil)

// Repos lists repositories tasks may start in.
func (d *Daemon) Repos(ctx context.Context) ([]api.RepoInfo, error) {
	return nil, core.ErrUnsupported
}

// Tasks lists tasks, newest first.
func (d *Daemon) Tasks(ctx context.Context) ([]api.Task, error) {
	return nil, core.ErrUnsupported
}

// StartTask starts an agent on a prompt in a new worktree.
func (d *Daemon) StartTask(ctx context.Context, t api.TaskStart, by string) (api.Task, error) {
	return api.Task{}, core.ErrUnsupported
}

// TaskDiff is what a task changed relative to its base.
func (d *Daemon) TaskDiff(ctx context.Context, taskID string) (api.TaskDiff, error) {
	return api.TaskDiff{}, core.ErrUnsupported
}

// FinishTask ships (push + PR) or discards a task.
func (d *Daemon) FinishTask(ctx context.Context, taskID string, f api.TaskFinish, by string) (api.Task, error) {
	return api.Task{}, core.ErrUnsupported
}
