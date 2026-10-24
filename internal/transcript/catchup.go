package transcript

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// CatchUpExtra is what the daemon knows that the transcript doesn't.
type CatchUpExtra struct {
	Waiting   []string // what open requests ask for right now
	Approvals int      // permission requests raised since the catch-up start
}

const maxWaitingLines = 3

// catchUp summarises the feed after since in short sentences, most
// important first. The same feed, since and now always give the same
// lines.
func (f *feed) catchUp(since, now time.Time, extra CatchUpExtra) *api.CatchUp {
	cu := &api.CatchUp{Since: since, Lines: []string{}}
	type test struct {
		name    string
		at      time.Time
		pending bool
		failed  bool
	}
	var (
		lastTest   *test
		files      = map[string]*api.FileChange{}
		fileOrder  []string
		asks       int
		plans      int
		subagents  int
		compacted  int
		lastError  string
		lastText   string
		errorCount int
		denied     int
	)
	for _, it := range f.items {
		if !it.At.After(since) {
			continue
		}
		switch it.Kind {
		case api.ActivityTool, api.ActivitySubagent:
			switch {
			case it.asks > 0:
				asks += it.asks
				continue
			case it.plan:
				plans++
				continue
			}
			cu.ToolCalls++
			if it.Kind == api.ActivitySubagent {
				subagents++
			}
			switch {
			case it.denied:
				denied++
			case it.IsError:
				errorCount++
				lastError = it.Summary
			}
			if name := testRunner(it.command); name != "" {
				lastTest = &test{name: name, at: it.At, pending: it.Pending, failed: it.IsError}
			}
			for _, e := range it.edits {
				p := hookkit.RelPath(f.cwd, e.path)
				fc := files[p]
				if fc == nil {
					fc = &api.FileChange{Path: p, Op: e.op}
					files[p] = fc
					fileOrder = append(fileOrder, p)
				}
				fc.Added += e.added
				fc.Removed += e.removed
			}
		case api.ActivityError:
			errorCount++
			lastError = it.Text
		case api.ActivitySystem:
			if strings.HasPrefix(it.Text, "Compacted") {
				compacted++
			}
		case api.ActivityText:
			if !it.Sidechain {
				lastText = it.Text
			}
		}
	}
	cu.Errors, cu.Files = errorCount, len(files)
	add := func(format string, args ...any) { cu.Lines = append(cu.Lines, fmt.Sprintf(format, args...)) }

	for i, w := range extra.Waiting {
		if i == maxWaitingLines {
			add("…and %d more waiting for you", len(extra.Waiting)-i)
			break
		}
		add("Waiting for you: %s", hookkit.OneLine(w, 120))
	}
	if errorCount > 0 {
		s := countNoun(errorCount, "error")
		if lastError != "" {
			s += " (last: " + hookkit.OneLine(lastError, 100) + ")"
		}
		add("Hit %s", s)
	}
	switch {
	case denied == 1:
		add("1 tool call was denied")
	case denied > 1:
		add("%d tool calls were denied", denied)
	}
	if lastTest != nil {
		switch {
		case lastTest.pending:
			add("Tests: %s is running (started %s)", lastTest.name, ago(lastTest.at, now))
		case lastTest.failed:
			add("Tests: %s failed (last run %s)", lastTest.name, ago(lastTest.at, now))
		default:
			add("Tests: %s passed (last run %s)", lastTest.name, ago(lastTest.at, now))
		}
	}
	if len(files) > 0 {
		var added, removed int
		for _, fc := range files {
			added += fc.Added
			removed += fc.Removed
		}
		s := "Edited " + countNoun(len(files), "file")
		if added+removed > 0 {
			s += fmt.Sprintf(" (+%d/−%d)", added, removed)
		}
		if len(fileOrder) <= 3 {
			s += ": " + strings.Join(fileOrder, ", ")
		}
		add("%s", s)
	}
	if cu.ToolCalls > 0 {
		add("Ran %s", countNoun(cu.ToolCalls, "tool call"))
	}
	if asks > 0 {
		add("Asked you %s", countNoun(asks, "question"))
	}
	switch {
	case plans == 1:
		add("Asked you to approve a plan")
	case plans > 1:
		add("Asked you to approve %d plans", plans)
	}
	if extra.Approvals > 0 {
		add("Needed your approval %s", times(extra.Approvals))
	}
	if subagents > 0 {
		add("Started %s", countNoun(subagents, "subagent"))
	}
	if compacted > 0 {
		add("Compacted its context")
	}
	if lastText != "" {
		add("Last said: “%s”", hookkit.OneLine(lastText, 160))
	}
	if len(cu.Lines) == 0 {
		if since.IsZero() {
			add("Nothing yet")
		} else {
			add("Nothing new since %s", since.In(f.loc).Format("15:04"))
		}
	}
	return cu
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return strconv.Itoa(n) + " times"
}

// ago says how long before now t was, the way a person would.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	}
	return "on " + t.Local().Format("Jan 2")
}
