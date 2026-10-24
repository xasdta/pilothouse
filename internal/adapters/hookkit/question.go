package hookkit

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
)

// Questions and plans (docs/PLAN-0.2.md F1). Adapters parse their agent's
// own question and plan tools into api.Question and api.Plan; the helpers
// here bound what they accept and word the summaries, so every adapter and
// the daemon describe a question the same way. That matters for matching:
// Claude Code's PermissionRequest carries no tool_use_id, so the post_tool
// hook that completes a question finds its request by tool and summary.

const (
	// MaxQuestions is the most questions one request may ask; devices can
	// send at most this many answers.
	MaxQuestions = 8
	// MaxQuestionBytes bounds a question's text, which is also its answer
	// key (a device answer key may be at most this long).
	MaxQuestionBytes = 2000
	// MaxOptions bounds the options of one question.
	MaxOptions = 16
	// MaxPlanBytes caps the plan markdown kept for display. The agent's
	// answer echoes its own input, never this copy.
	MaxPlanBytes = 64 << 10

	maxHeaderRunes   = 40
	maxLabelBytes    = 500
	maxDescBytes     = 500
	summaryTextRunes = 120
)

// CheckQuestions reports whether qs can be asked through Pilothouse: one
// to MaxQuestions questions with non-empty, unique, bounded keys and
// bounded options. A question set that fails is left to the agent's own
// dialog (the request then stays an ordinary permission request).
func CheckQuestions(qs []api.Question) bool {
	if len(qs) == 0 || len(qs) > MaxQuestions {
		return false
	}
	seen := map[string]bool{}
	for _, q := range qs {
		k := q.Key()
		if strings.TrimSpace(q.Text) == "" || k == "" || len(k) > MaxQuestionBytes || !utf8.ValidString(k) || seen[k] {
			return false
		}
		seen[k] = true
		if len(q.Options) > MaxOptions {
			return false
		}
		labels := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || len(o.Label) > maxLabelBytes || labels[o.Label] {
				return false
			}
			labels[o.Label] = true
		}
		if len(q.Options) == 0 && !q.AllowOther {
			return false // nothing could answer it
		}
	}
	return true
}

// TidyQuestion trims a question's display-only fields (header, option
// descriptions) to sane sizes. Texts and labels are left exactly as the
// agent sent them: answers are keyed and matched by them.
func TidyQuestion(q api.Question) api.Question {
	q.Header = Truncate(strings.TrimSpace(q.Header), maxHeaderRunes)
	if len(q.Options) > 0 {
		opts := make([]api.QuestionOption, len(q.Options))
		for i, o := range q.Options {
			o.Description = CapBytes(strings.TrimSpace(o.Description), maxDescBytes)
			opts[i] = o
		}
		q.Options = opts
	}
	return q
}

// QuestionSummary is the one-line summary of a question request:
// "Asks: Which database?", with "(+N more)" when it asks several.
func QuestionSummary(qs []api.Question) string {
	if len(qs) == 0 {
		return "Asks a question"
	}
	s := "Asks: " + OneLine(qs[0].Text, summaryTextRunes)
	if len(qs) > 1 {
		s += " (+" + strconv.Itoa(len(qs)-1) + " more)"
	}
	return s
}

// PlanSummary is the one-line summary of a plan request:
// "Plan ready: <first heading or line>".
func PlanSummary(markdown string) string {
	if t := PlanTitle(markdown); t != "" {
		return "Plan ready: " + t
	}
	return "Plan ready"
}

// genericPlanHeadings are headings that say nothing about the plan; the
// title skips them in favour of the next heading or line.
var genericPlanHeadings = map[string]bool{
	"plan": true, "the plan": true, "my plan": true, "implementation plan": true,
	"proposed plan": true, "overview": true, "summary": true,
}

// PlanTitle is the plan's first meaningful heading, else its first line
// of text, without markdown markers.
func PlanTitle(markdown string) string {
	var firstLine, heading string
	for i, line := range strings.Split(markdown, "\n") {
		if i > 200 {
			break
		}
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "```") || strings.Trim(t, "-*_= ") == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			h := plainText(strings.TrimLeft(t, "# "))
			// "Plan: Add OAuth login" → "Add OAuth login".
			if rest, ok := cutPrefixFold(h, "plan:"); ok {
				h = strings.TrimSpace(rest)
			}
			if h != "" && !genericPlanHeadings[strings.ToLower(strings.TrimRight(h, ":"))] {
				heading = h
				break
			}
			continue
		}
		if firstLine == "" {
			firstLine = plainText(listMarker.ReplaceAllString(t, ""))
		}
	}
	return OneLine(firstNonBlank(heading, firstLine), summaryTextRunes)
}

// listMarker matches a leading list or quote marker: "- ", "1. ", "> ".
var listMarker = regexp.MustCompile(`^(?:[-*+>]\s+|\d{1,3}[.)]\s+)+`)

// plainText drops the inline markdown markers a summary shouldn't show.
func plainText(s string) string {
	for _, m := range []string{"**", "__", "`"} {
		s = strings.ReplaceAll(s, m, "")
	}
	return strings.TrimSpace(s)
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
