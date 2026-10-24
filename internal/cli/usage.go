package cli

// Workstream C (0.2): `pilothouse usage` (tokens, estimated cost, what the
// rules did) and `pilothouse activity` (a session's feed).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

// Seams for tests.
var (
	usageNow          = time.Now
	usageLocation     = time.Local
	activityPollEvery = 2 * time.Second
)

func usageClient() *client.Client { return client.New().WithClientName("cli") }

// ---------------------------------------------------------------- usage

func newUsageCmd() *cobra.Command {
	var days int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usage [--days N] [--json]",
		Short: "Show tokens and estimated cost per day, agent, model and repo",
		Long: `Show token use and estimated cost of your agents over the last days, from
their transcripts, and what your rules did meanwhile. Costs are estimates
from list prices (override them with usage.prices in config.yaml); a
model Pilothouse has no price for counts tokens but no cost.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if days < 1 || days > 90 {
				return fmt.Errorf("--days must be 1-90, not %d", days)
			}
			in, err := usageClient().Insights(cmd.Context(), api.InsightsQuery{Days: days})
			if err != nil {
				return inboxErr(err, "")
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(in)
			}
			printUsage(cmd.OutOrStdout(), in)
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "how many days back, today included (1-90)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the insights as JSON")
	return cmd
}

func printUsage(w io.Writer, in api.Insights) {
	n := len(in.Days)
	fmt.Fprintf(w, "Usage, last %d day%s (estimated from list prices)\n\n", n, usgPlural(n))
	if in.Total == (api.Usage{}) {
		fmt.Fprintln(w, "  No agent usage recorded yet. Pilothouse reads it from Claude Code and Codex")
		fmt.Fprintln(w, "  transcripts while the daemon runs.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "DAY\tCOST\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tSESSIONS\t")
		for _, d := range in.Days {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t\n", usgDay(d.Date), usgMoney(d.Usage.CostUSD),
				usgTokens(d.Usage.InputTokens), usgTokens(d.Usage.OutputTokens), usgTokens(d.Usage.CacheReadTokens),
				usgTokens(d.Usage.CacheWriteTokens), d.Sessions)
		}
		fmt.Fprintf(tw, "Total\t%s\t%s\t%s\t%s\t%s\t\t\n", usgMoney(in.Total.CostUSD), usgTokens(in.Total.InputTokens),
			usgTokens(in.Total.OutputTokens), usgTokens(in.Total.CacheReadTokens), usgTokens(in.Total.CacheWriteTokens))
		tw.Flush()
		if n > 1 {
			costs := make([]float64, n)
			for i, d := range in.Days {
				costs[i] = d.Usage.CostUSD
			}
			fmt.Fprintf(w, "\n  Cost per day  %s  (%s … %s)\n", usgSparkline(costs), usgDay(in.Days[0].Date), usgDay(in.Days[n-1].Date))
		}
		fmt.Fprintln(w)
		usgGroups(w, "By agent", in.ByAgent, func(k string) string { return k })
		usgGroups(w, "By model", in.ByModel, func(k string) string { return k })
		usgGroups(w, "By repo", in.ByRepo, func(k string) string {
			if strings.HasPrefix(k, "/") {
				return filepath.Base(k)
			}
			return k
		})
	}
	if b := in.Budget; b != nil {
		s := usgMoney(b.SpentUSD) + " spent"
		if b.DailyUSD > 0 {
			s = fmt.Sprintf("%s of %s budget (%.0f%%)", usgMoney(b.SpentUSD), usgMoney(b.DailyUSD), b.Percent)
		}
		if b.BurnUSDh > 0 {
			s += fmt.Sprintf(" · %s in the last hour", usgMoney(b.BurnUSDh))
		}
		fmt.Fprintf(w, "Today     %s\n", s)
	}
	r := in.Rules
	fmt.Fprintf(w, "Rules     %d auto-allowed (%d by \"allow for session\") · %d asked you · %d blocked\n",
		r.AutoAllowed, r.Granted, r.Asked, r.Blocked)
	if answered := r.AnsweredByDevice + r.AnsweredAtTerminal; answered+r.Expired > 0 {
		s := fmt.Sprintf("%d answered on a device, %d at the terminal", r.AnsweredByDevice, r.AnsweredAtTerminal)
		if r.Expired > 0 {
			s += fmt.Sprintf(", %d expired", r.Expired)
		}
		if r.MedianAnswerSecs > 0 {
			s += " · median answer " + usgSeconds(r.MedianAnswerSecs)
		}
		if r.BlockedMinutes > 0 {
			s += fmt.Sprintf(" · agents waited %s", usgMinutes(r.BlockedMinutes))
		}
		fmt.Fprintf(w, "          %s\n", s)
	}
	if len(r.TopBlocked) > 0 {
		var parts []string
		for _, c := range r.TopBlocked {
			parts = append(parts, fmt.Sprintf("%s (%d)", inboxClip(oneLineText(c.Key), 50), c.Count))
		}
		fmt.Fprintf(w, "Blocked   %s\n", strings.Join(parts, " · "))
	}
}

func usgGroups(w io.Writer, title string, gs []api.UsageGroup, name func(string) string) {
	if len(gs) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, g := range gs {
		label := ""
		if i == 0 {
			label = title
		}
		if i == 5 && len(gs) > 6 {
			var rest float64
			for _, x := range gs[5:] {
				rest += x.Usage.CostUSD
			}
			fmt.Fprintf(tw, "%s\t%d more\t%s\t\n", label, len(gs)-5, usgMoney(rest))
			break
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s tokens · %d session%s\n", label, name(g.Key), usgMoney(g.Usage.CostUSD),
			usgTokens(g.Usage.InputTokens+g.Usage.OutputTokens+g.Usage.CacheReadTokens+g.Usage.CacheWriteTokens),
			g.Sessions, usgPlural(g.Sessions))
	}
	tw.Flush()
	fmt.Fprintln(w)
}

func usgDay(date string) string {
	d, err := time.ParseInLocation("2006-01-02", date, usageLocation)
	if err != nil {
		return date
	}
	return d.Format("Mon Jan 2")
}

func usgMoney(x float64) string {
	switch {
	case x == 0:
		return "$0"
	case x < 0.01:
		return "<$0.01"
	}
	return "$" + strconv.FormatFloat(x, 'f', 2, 64)
}

// usgTokens renders a count as 950, 12.3k, 4.5M.
func usgTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "B"
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

func usgSeconds(s float64) string {
	if s < 60 {
		return strconv.Itoa(int(math.Round(s))) + "s"
	}
	return fmt.Sprintf("%dm %ds", int(s)/60, int(s)%60)
}

func usgMinutes(m float64) string {
	if m < 60 {
		return strconv.FormatFloat(m, 'f', 1, 64) + " min"
	}
	return fmt.Sprintf("%dh %dm", int(m)/60, int(m)%60)
}

// usgSparkline draws values as block characters, the largest full height.
func usgSparkline(vals []float64) string {
	const bars = "▁▂▃▄▅▆▇█"
	levels := []rune(bars)
	var top float64
	for _, v := range vals {
		top = math.Max(top, v)
	}
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if top > 0 {
			i = int(math.Round(v / top * float64(len(levels)-1)))
		}
		b.WriteRune(levels[i])
	}
	return b.String()
}

func usgPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------------------------------------------------------------- activity

func newActivityCmd() *cobra.Command {
	var (
		follow  bool
		asJSON  bool
		lines   int
		catchUp bool
	)
	cmd := &cobra.Command{
		Use:   "activity <session> [-f] [-n N] [--catch-up] [--json]",
		Short: "Show what an agent session has been doing, from its transcript",
		Long: `Show a session's activity feed: prompts, replies, tool calls with their
results, errors and subagents, read from the agent's transcript. The session
is an id or prefix from ` + "`pilothouse ls`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 1 || lines > 1000 {
				return fmt.Errorf("-n must be 1-1000, not %d", lines)
			}
			return runActivity(cmd.Context(), cmd.OutOrStdout(), args[0], lines, follow, catchUp, asJSON)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new activity")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "how many past items to show (1-1000)")
	cmd.Flags().BoolVar(&catchUp, "catch-up", false, "start with a summary of what happened since you last looked")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON (one item per line with -f)")
	return cmd
}

func runActivity(ctx context.Context, w io.Writer, ref string, limit int, follow, catchUp, asJSON bool) error {
	c := usageClient()
	act, err := c.Activity(ctx, ref, api.ActivityQuery{Limit: limit})
	if err != nil {
		return inboxErr(err, ref)
	}
	if asJSON && !follow {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(act)
	}
	if !act.Available {
		fmt.Fprintln(w, usgFirstLine(act.Note, "No activity to show."))
		return nil
	}
	p := &activityPrinter{w: w, json: asJSON, shown: map[string]bool{}}
	if catchUp && act.CatchUp != nil && !asJSON {
		since := "the start"
		if !act.CatchUp.Since.IsZero() {
			since = act.CatchUp.Since.In(usageLocation).Format("15:04")
		}
		fmt.Fprintf(w, "Since %s:\n", since)
		for _, l := range act.CatchUp.Lines {
			fmt.Fprintf(w, "  • %s\n", l)
		}
		fmt.Fprintln(w)
	}
	if len(act.Items) == 0 && !asJSON {
		fmt.Fprintln(w, "Nothing in the transcript yet.")
	}
	for _, it := range act.Items {
		p.print(it)
	}
	if !follow {
		if !asJSON {
			printActivityFooter(w, act)
		}
		return nil
	}
	cursor := act.Cursor
	t := time.NewTicker(activityPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		next, err := c.Activity(ctx, act.SessionID, api.ActivityQuery{Cursor: cursor, Limit: 1000})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return inboxErr(err, ref)
		}
		cursor = next.Cursor
		for _, it := range next.Items {
			p.print(it)
		}
	}
}

// activityPrinter prints items once; a tool call printed while it ran gets a
// second line when it finishes.
type activityPrinter struct {
	w     io.Writer
	json  bool
	shown map[string]bool // id → printed while pending
	turn  int
}

const maxShown = 20_000

func (p *activityPrinter) print(it api.ActivityItem) {
	pending, seen := p.shown[it.ID]
	if seen && (!pending || it.Pending) {
		return
	}
	if len(p.shown) >= maxShown {
		p.shown = map[string]bool{}
	}
	p.shown[it.ID] = it.Pending
	if p.json {
		b, _ := json.Marshal(it)
		fmt.Fprintln(p.w, string(b))
		return
	}
	if p.turn != 0 && it.Turn != p.turn && !seen {
		fmt.Fprintln(p.w)
	}
	if !seen {
		p.turn = it.Turn
	}
	ts := it.At.In(usageLocation).Format("15:04:05")
	indent := ""
	if it.Sidechain {
		indent = "  ↳ "
	}
	fmt.Fprintf(p.w, "%s  %s%s\n", ts, indent, activityLine(it, seen))
}

func activityLine(it api.ActivityItem, finished bool) string {
	switch it.Kind {
	case api.ActivityPrompt:
		s := "› " + inboxClip(oneLineText(it.Text), 120)
		switch it.Source {
		case "pilothouse":
			s += "  (sent from Pilothouse)"
		case "peer":
			s += "  (from another session)"
		}
		return s
	case api.ActivityText:
		return "● " + inboxClip(oneLineText(it.Text), 140)
	case api.ActivityError:
		return "! " + inboxClip(oneLineText(it.Text), 140)
	case api.ActivitySystem:
		return "─ " + inboxClip(oneLineText(it.Text), 140)
	}
	mark := "✓"
	switch {
	case it.Pending:
		mark = "…"
	case it.IsError:
		mark = "✗"
	}
	s := "$ "
	if it.Kind == api.ActivitySubagent {
		s = "⑂ "
	}
	s += inboxClip(oneLineText(usgFirstLine(it.Summary, it.Tool)), 110) + "  " + mark
	if finished {
		s += " (finished)"
	}
	if it.IsError && it.Result != "" {
		s += "  " + inboxClip(oneLineText(usgLastLine(it.Result)), 80)
	}
	return s
}

func printActivityFooter(w io.Writer, act api.Activity) {
	if u := act.Usage; u != nil {
		s := fmt.Sprintf("%s in · %s out · %s cached", usgTokens(u.InputTokens), usgTokens(u.OutputTokens), usgTokens(u.CacheReadTokens+u.CacheWriteTokens))
		if u.CostUSD > 0 {
			s += " · ~" + usgMoney(u.CostUSD)
		}
		if u.Model != "" {
			s += " (" + u.Model + ")"
		}
		fmt.Fprintf(w, "\nUsage  %s\n", s)
	}
	if len(act.Files) > 0 {
		var added, removed int
		var names []string
		for i, f := range act.Files {
			added += f.Added
			removed += f.Removed
			if i < 5 {
				names = append(names, fmt.Sprintf("%s (+%d/−%d)", f.Path, f.Added, f.Removed))
			}
		}
		more := ""
		if len(act.Files) > 5 {
			more = fmt.Sprintf(", %d more", len(act.Files)-5)
		}
		fmt.Fprintf(w, "Files  %d changed (+%d/−%d): %s%s\n", len(act.Files), added, removed, strings.Join(names, ", "), more)
	}
	if act.Note != "" {
		fmt.Fprintf(w, "Note   %s\n", act.Note)
	}
}

func usgFirstLine(s, def string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func usgLastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
