package notify

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

const maxCommand = 80

// AgentName is the display name of an agent kind.
func AgentName(a api.AgentKind) string {
	switch a {
	case api.AgentClaude:
		return "Claude Code"
	case api.AgentCodex:
		return "Codex"
	case "", api.AgentGeneric:
		return "An agent"
	}
	return strings.ToUpper(string(a[:1])) + string(a[1:])
}

// Short trims s to n runes on one line, with an ellipsis.
func Short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// Action is what a request wants, without the agent:
// "wants to run `git push origin main`", "asks: Which database? (Postgres
// or SQLite)", "has a plan ready: Add OAuth login".
func Action(r *api.Request) string {
	switch {
	case r.Checkpoint == api.CheckpointQuestion && len(r.Questions) > 0:
		return questionAction(r.Questions)
	case r.Checkpoint == api.CheckpointPlan:
		if t := strings.TrimSpace(strings.TrimPrefix(r.Summary, "Plan ready")); strings.HasPrefix(t, ":") {
			return "has a plan ready: " + Short(strings.TrimSpace(t[1:]), maxCommand)
		}
		return "has a plan ready for you to approve"
	case r.Command != "":
		return "wants to run `" + Short(r.Command, maxCommand) + "`"
	case len(r.Paths) > 0:
		p := r.Paths[0]
		if rel, err := filepath.Rel(r.Cwd, p); r.Cwd != "" && err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
		}
		verb := "edit"
		if r.ToolKind == api.ToolRead {
			verb = "read"
		}
		s := "wants to " + verb + " `" + Short(p, maxCommand) + "`"
		if len(r.Paths) > 1 {
			s += " and " + strconv.Itoa(len(r.Paths)-1) + " more"
		}
		return s
	case r.Summary != "":
		return "asks: " + Short(r.Summary, maxCommand)
	}
	return "wants to use " + r.Tool
}

// maxOptionsShown bounds the options listed in a notification.
const maxOptionsShown = 4

// questionAction words a question with its options:
// "asks: Which database? (Postgres, MySQL or SQLite)", plus "+1 more
// question" when it asks several.
func questionAction(qs []api.Question) string {
	q := qs[0]
	s := "asks: " + Short(q.Text, 120)
	if n := len(q.Options); n > 0 {
		labels := make([]string, 0, min(n, maxOptionsShown))
		for _, o := range q.Options[:min(n, maxOptionsShown)] {
			labels = append(labels, Short(o.Label, 40))
		}
		list := strings.Join(labels, ", ")
		switch {
		case n > maxOptionsShown:
			list += " …"
		case n > 1:
			list = strings.Join(labels[:n-1], ", ") + " or " + labels[n-1]
		}
		if q.MultiSelect {
			list = "any of " + list
		}
		s += " (" + list + ")"
	}
	if len(qs) > 1 {
		s += " +" + strconv.Itoa(len(qs)-1) + " more question"
		if len(qs) > 2 {
			s += "s"
		}
	}
	return s
}

// Where names the repo (and branch) of a request or session.
func Where(r *api.Request, s *api.Session) string {
	name, branch := "", ""
	if s != nil {
		name, branch = s.Title, s.Branch
		if name == "" && s.RepoRoot != "" {
			name = filepath.Base(s.RepoRoot)
		}
		if name == "" {
			name = filepath.Base(s.Cwd)
		}
	}
	if name == "" && r != nil && r.Cwd != "" {
		name = filepath.Base(r.Cwd)
	}
	if name == "" || name == "." || name == "/" {
		return ""
	}
	if branch != "" {
		return name + " (" + branch + ")"
	}
	return name
}

// Line is the one-line text used by ntfy and webhooks:
// "Claude Code wants to run `git push origin main` · repo-name".
func Line(n core.Notification) string {
	if n.Request == nil {
		switch {
		case n.Title != "" && n.Body != "":
			return n.Title + ": " + n.Body
		case n.Title != "":
			return n.Title
		}
		return n.Body
	}
	s := AgentName(n.Request.Agent) + " " + Action(n.Request)
	if w := Where(n.Request, n.Session); w != "" {
		s += " · " + w
	}
	if n.Kind == core.NotifyBlocked {
		s += " — answer in the terminal"
	}
	return s
}

// Render fills an empty Title/Body with the standard wording, so every
// channel describes requests the same way. A caller-set Title wins.
func Render(n core.Notification) core.Notification {
	agent := "An agent"
	switch {
	case n.Request != nil:
		agent = AgentName(n.Request.Agent)
	case n.Session != nil:
		agent = AgentName(n.Session.Agent)
	}
	where := Where(n.Request, n.Session)
	if n.Title == "" {
		switch {
		case n.Kind == core.NotifyTest:
			n.Title = "Pilothouse test notification"
		case where != "":
			n.Title = agent + " · " + where
		default:
			n.Title = agent
		}
	}
	if n.Body == "" {
		switch {
		case n.Request != nil:
			n.Body = capitalize(Action(n.Request))
			if n.Kind == core.NotifyBlocked {
				n.Body += " — waiting in the terminal"
			}
		case n.Kind == core.NotifyWaiting && n.Session != nil && n.Session.LastMessage != "":
			n.Body = "Finished: " + Short(n.Session.LastMessage, 160)
		case n.Kind == core.NotifyWaiting:
			n.Body = "Finished its turn and is waiting for you"
		case n.Kind == core.NotifyTest:
			n.Body = "Notifications are working."
		}
	}
	return n
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
