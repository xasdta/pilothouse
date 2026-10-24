package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// Tasks are recorded locally in paths.Home()/tasks.json, always, and
// mirrored to the daemon (POST /v1/tasks) whenever it is running. The
// local file is the source of truth for `pilothouse task`: tasks keep
// working with no daemon, and a daemon that was down when a task changed
// is brought up to date the next time the task is touched.

const storeVersion = 1

type storeFile struct {
	Version int        `json:"version"`
	Tasks   []api.Task `json:"tasks"`
}

// StorePath is the local task file.
func StorePath() string { return filepath.Join(paths.Home(), "tasks.json") }

// loadTasks reads the task file; a missing file means no tasks.
func loadTasks() ([]api.Task, error) {
	b, err := os.ReadFile(StorePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s is corrupt (%v) — fix or remove it", StorePath(), err)
	}
	if f.Version > storeVersion {
		return nil, fmt.Errorf("%s was written by a newer pilothouse (format %d) — upgrade pilothouse", StorePath(), f.Version)
	}
	return f.Tasks, nil
}

// updateTasks applies fn to the task list under an exclusive lock, so
// concurrent `pilothouse task` invocations don't lose each other's writes,
// and saves atomically.
func updateTasks(fn func([]api.Task) ([]api.Task, error)) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	lock, err := os.OpenFile(StorePath()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", StorePath(), err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	tasks, err := loadTasks()
	if err != nil {
		return err
	}
	tasks, err = fn(tasks)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(storeFile{Version: storeVersion, Tasks: tasks}, "", "  ")
	if err != nil {
		return err
	}
	tmp := StorePath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, StorePath())
}

// saveTask inserts or replaces one task.
func saveTask(t api.Task) error {
	return updateTasks(func(ts []api.Task) ([]api.Task, error) {
		for i := range ts {
			if ts[i].ID == t.ID {
				ts[i] = t
				return ts, nil
			}
		}
		return append(ts, t), nil
	})
}

// deleteTask removes one task (used to roll back a failed `task new`).
func deleteTask(id string) error {
	return updateTasks(func(ts []api.Task) ([]api.Task, error) {
		out := ts[:0]
		for _, t := range ts {
			if t.ID != id {
				out = append(out, t)
			}
		}
		return out, nil
	})
}

// Load returns every recorded task, oldest first.
func Load() ([]api.Task, error) { return loadTasks() }

// Save inserts or replaces one task in the local record.
func Save(t api.Task) error { return saveTask(t) }

// SetOpenStatus moves a task that is still open (running or review) to
// status, under the store lock, so a status derived from the agent's
// session never overwrites a task someone just finished. It returns the
// task as stored and whether it changed; a task that is not open, or not
// recorded here, is left alone.
func SetOpenStatus(id string, status api.TaskStatus, now time.Time) (api.Task, bool, error) {
	var (
		out     api.Task
		changed bool
	)
	err := updateTasks(func(ts []api.Task) ([]api.Task, error) {
		for i := range ts {
			if ts[i].ID != id {
				continue
			}
			if (ts[i].Status == api.TaskRunning || ts[i].Status == api.TaskReview) && ts[i].Status != status {
				ts[i].Status, ts[i].UpdatedAt = status, now
				changed = true
			}
			out = ts[i]
		}
		return ts, nil
	})
	return out, changed, err
}
