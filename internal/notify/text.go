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
		if t := PlanTitle(r.Plan); t != "" {
			return "has a plan ready: " + Short(t, maxCommand)
		}
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
			if d := Details(n.Request); len(d) > 0 {
				n.Body += "\n" + strings.Join(d, "\n")
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

// maxDetail bounds each extra line of a request notification.
const maxDetail = 160

// Details are the extra lines a request notification carries when the
// request has them (other workstreams fill these fields): risk labels, the
// size of a file change and the agent's last words before it asked. (A
// question's options are part of Action.) Each line is one short
// sentence; none when the request has nothing beyond its action.
func Details(r *api.Request) []string {
	var out []string
	if line := RiskLine(r.Risk); line != "" {
		out = append(out, line)
	}
	if p := r.Preview; p != nil && len(p.Files) > 0 {
		files := "1 file"
		if len(p.Files) > 1 {
			files = strconv.Itoa(len(p.Files)) + " files"
		}
		out = append(out, "✎ "+files+", +"+strconv.Itoa(p.Added)+" −"+strconv.Itoa(p.Removed))
	}
	if c := strings.TrimSpace(r.Context); c != "" {
		out = append(out, "“"+Short(c, maxDetail-2)+"”")
	}
	return out
}

// RiskLine is the request's risk labels on one line, in the order given
// (most severe first): "⚠ Force-pushes to main · Reads .env". Empty when
// there are none.
func RiskLine(labels []api.RiskLabel) string {
	var texts []string
	high := false
	for _, l := range labels {
		t := strings.TrimSpace(l.Text)
		if t == "" {
			t = strings.ReplaceAll(l.Code, "_", " ")
		}
		if t == "" {
			continue
		}
		if l.Level == api.RiskHigh {
			high = true
		}
		texts = append(texts, t)
		if len(texts) == 3 {
			break
		}
	}
	if len(texts) == 0 {
		return ""
	}
	mark := "•"
	if high {
		mark = "⚠"
	}
	return Short(mark+" "+strings.Join(texts, " · "), maxDetail)
}

// PlanTitle is the first non-empty line of a plan without Markdown marks.
func PlanTitle(p *api.Plan) string {
	if p == nil {
		return ""
	}
	for _, line := range strings.Split(p.Markdown, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#*->"))
		if line != "" {
			return line
		}
	}
	return ""
}

// Outcome says how a closed request ended, for the messages that replace
// its notification: "Allowed", "Changes requested", "Timed out — answer
// in the terminal". Empty while the request is open.
func Outcome(r api.Request) string {
	switch r.Status {
	case api.RequestDecided:
		d := r.Decision
		if d == nil {
			return "Answered"
		}
		allow := d.Behavior == api.BehaviorAllow
		switch {
		case r.Checkpoint == api.CheckpointPlan && allow:
			return "Plan approved"
		case r.Checkpoint == api.CheckpointPlan:
			return "Changes requested"
		case r.Checkpoint == api.CheckpointQuestion && allow:
			return "Answered"
		case r.Checkpoint == api.CheckpointQuestion:
			return "Dismissed"
		case allow && d.Scope == api.ScopeSession:
			return "Allowed for this session"
		case allow:
			return "Allowed"
		}
		return "Denied"
	case api.RequestAnsweredLocally:
		return "Answered in the terminal"
	case api.RequestSteppedAside:
		return "Moved to the terminal"
	case api.RequestExpired:
		return "Timed out — answer in the terminal"
	}
	return ""
}

// DecidedBy names who answered a decided request, as a phrase: "from
// iPhone", "from the CLI", "by rule no-force-push". deviceName maps a
// paired device's id to its name; it may be nil. Empty when unknown.
func DecidedBy(r api.Request, deviceName func(id string) string) string {
	if r.Status != api.RequestDecided || r.Decision == nil {
		return ""
	}
	by := r.Decision.By
	kind, rest, _ := strings.Cut(by, ":")
	switch kind {
	case "device":
		if deviceName != nil {
			if name := strings.TrimSpace(deviceName(rest)); name != "" {
				return "from " + Short(name, 40)
			}
		}
		return "from a paired device"
	case "cli":
		return "from the CLI"
	case "tui":
		return "from the inbox"
	case "telegram":
		return "from Telegram"
	case "ntfy":
		return "from ntfy"
	case "policy":
		return "by rule " + Short(rest, 40)
	case "grant":
		return "by an earlier allow for this session"
	case "":
		return ""
	}
	return "by " + Short(by, 40)
}

// ResolvedLine is the one-line outcome of a closed request:
// "✓ Allowed from iPhone", "✗ Denied from the CLI", "⏱ Timed out — …".
// Empty while the request is open.
func ResolvedLine(r api.Request, deviceName func(id string) string) string {
	o := Outcome(r)
	if o == "" {
		return ""
	}
	s := Mark(r) + " " + o
	if by := DecidedBy(r, deviceName); by != "" {
		s += " " + by
	}
	return s
}

// Mark is the symbol that goes with Outcome: ✓ ✗ ✎ ↩ ⏱.
func Mark(r api.Request) string {
	switch {
	case r.Status == api.RequestExpired:
		return "⏱"
	case r.Status == api.RequestSteppedAside:
		return "↩"
	case r.Status == api.RequestDecided && r.Decision != nil && r.Decision.Behavior == api.BehaviorDeny:
		if r.Checkpoint == api.CheckpointPlan {
			return "✎"
		}
		return "✗"
	}
	return "✓"
}
