package preview

import (
	"strconv"
	"strings"
)

// A line diff for approval cards: Myers' O(ND) algorithm on interned
// lines after trimming the common prefix and suffix (an Edit changes a few
// lines of a large file, so the middle is usually tiny). The search is
// bounded: past maxCost steps it falls back to replacing the whole middle,
// which is still a correct diff, just not a minimal one.

// maxCost bounds (N+M)·D, the work of one diff, and so the time an
// approval card can cost: a few milliseconds at worst.
const maxCost = 4 << 20

// noEOL marks the interned key of a last line without a final newline,
// so "x" and "x\n" compare unequal, as in diff(1).
const noEOL = "\x00"

// splitText splits s into lines without their "\n". eol reports whether
// s ends with a newline (true for the empty string).
func splitText(s string) (lines []string, eol bool) {
	if s == "" {
		return nil, true
	}
	eol = strings.HasSuffix(s, "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n"), eol
}

// edit is one line of an edit script.
type edit struct {
	kind byte // ' ' kept, '-' removed, '+' added
	a, b int  // line index in a (for ' ' and '-') and in b (for ' ' and '+')
}

// diffLines returns an edit script turning a into b.
func diffLines(a, b []int) []edit {
	n, m := len(a), len(b)
	pre := 0
	for pre < n && pre < m && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < n-pre && suf < m-pre && a[n-1-suf] == b[m-1-suf] {
		suf++
	}
	out := make([]edit, 0, n+m-pre-suf)
	for i := 0; i < pre; i++ {
		out = append(out, edit{' ', i, i})
	}
	out = append(out, myers(a[pre:n-suf], b[pre:m-suf], pre, pre)...)
	for i := 0; i < suf; i++ {
		out = append(out, edit{' ', n - suf + i, m - suf + i})
	}
	return out
}

// myers diffs a and b, whose first lines are line aOff and bOff of the
// whole texts.
func myers(a, b []int, aOff, bOff int) []edit {
	n, m := len(a), len(b)
	replace := func() []edit {
		out := make([]edit, 0, n+m)
		for i := range a {
			out = append(out, edit{'-', aOff + i, bOff})
		}
		for j := range b {
			out = append(out, edit{'+', aOff + n, bOff + j})
		}
		return out
	}
	if n == 0 || m == 0 {
		return replace()
	}
	maxD := min(n+m, maxCost/(n+m), 4096)
	off := maxD + 1
	v := make([]int32, 2*maxD+3)
	// trace[d] holds v for k in [-(d-1), d-1] as it was before step d.
	trace := make([][]int32, 0, 16)
	for d := 0; d <= maxD; d++ {
		if d > 0 {
			trace = append(trace, append([]int32(nil), v[off-(d-1):off+d]...))
		} else {
			trace = append(trace, nil)
		}
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = int(v[off+k+1])
			} else {
				x = int(v[off+k-1]) + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = int32(x)
			if x >= n && y >= m {
				return backtrack(trace, n, m, aOff, bOff)
			}
		}
	}
	return replace()
}

func backtrack(trace [][]int32, n, m, aOff, bOff int) []edit {
	var rev []edit
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		k := x - y
		var prevK int
		at := func(k int) int { return int(trace[d][k+d-1]) }
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := 0
		if d > 0 {
			prevX = at(prevK)
		}
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x, y = x-1, y-1
			rev = append(rev, edit{' ', aOff + x, bOff + y})
		}
		if d > 0 {
			if x == prevX {
				y--
				rev = append(rev, edit{'+', aOff + x, bOff + y})
			} else {
				x--
				rev = append(rev, edit{'-', aOff + x, bOff + y})
			}
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// unified is a rendered diff.
type unified struct {
	text           string
	added, removed int
	truncated      bool
}

// unifiedDiff renders a unified diff (hunks only, no ---/+++ header) from
// a to b with ctx lines of context, at most maxBytes long. Added and
// removed always count the whole diff.
func unifiedDiff(a, b string, ctx, maxBytes int) unified {
	a, b, skip := trimCommon(a, b, ctx)
	al, aEOL := splitText(a)
	bl, bEOL := splitText(b)
	ids := map[string]int{}
	intern := func(lines []string, eol bool) []int {
		out := make([]int, len(lines))
		for i, l := range lines {
			if i == len(lines)-1 && !eol {
				l += noEOL
			}
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	ops := diffLines(intern(al, aEOL), intern(bl, bEOL))
	var u unified
	for _, op := range ops {
		switch op.kind {
		case '+':
			u.added++
		case '-':
			u.removed++
		}
	}
	if u.added == 0 && u.removed == 0 {
		return u
	}
	var out strings.Builder
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start, end := max(i-ctx, 0), i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*ctx {
				end = min(end+ctx, len(ops))
				break
			}
			end = run
		}
		hunk := renderHunk(ops[start:end], al, bl, aEOL, bEOL, skip)
		if out.Len()+len(hunk) > maxBytes {
			// Keep whole lines of the hunk that fit.
			room := maxBytes - out.Len()
			if cut := strings.LastIndexByte(hunk[:max(room, 0)], '\n'); cut > 0 && strings.Count(hunk[:cut], "\n") > 0 {
				out.WriteString(hunk[:cut+1])
			}
			u.truncated = true
			break
		}
		out.WriteString(hunk)
		i = end
	}
	u.text = out.String()
	return u
}

// trimCommon cuts the lines a and b share at the start and the end, but
// ctx lines of each, so only the changed middle is split and diffed: an
// Edit of a large file costs what its change costs. skip is the number of
// lines cut from the start.
func trimCommon(a, b string, ctx int) (string, string, int) {
	n := min(len(a), len(b))
	p := 0
	for p < n && a[p] == b[p] {
		p++
	}
	if p == len(a) && p == len(b) {
		return a, b, 0
	}
	cut := strings.LastIndexByte(a[:p], '\n') + 1 // start of the first differing line
	for k := 0; k < ctx && cut > 0; k++ {
		cut = strings.LastIndexByte(a[:cut-1], '\n') + 1
	}
	skip := strings.Count(a[:cut], "\n")
	a, b = a[cut:], b[cut:]

	n = min(len(a), len(b))
	s := 0
	for s < n && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	// The kept tail must start at a line start in both texts, so it starts
	// after a newline inside the common suffix.
	j := strings.IndexByte(a[len(a)-s:], '\n')
	if j < 0 {
		return a, b, skip
	}
	start := len(a) - s + j + 1
	for k := 0; k < ctx && start < len(a); k++ {
		if j := strings.IndexByte(a[start:], '\n'); j >= 0 {
			start += j + 1
		} else {
			start = len(a)
		}
	}
	tail := len(a) - start
	return a[:len(a)-tail], b[:len(b)-tail], skip
}

func renderHunk(ops []edit, al, bl []string, aEOL, bEOL bool, skip int) string {
	var na, nb int
	for _, op := range ops {
		if op.kind != '+' {
			na++
		}
		if op.kind != '-' {
			nb++
		}
	}
	aStart, bStart := skip+ops[0].a+1, skip+ops[0].b+1
	if na == 0 {
		aStart--
	}
	if nb == 0 {
		bStart--
	}
	var h strings.Builder
	h.WriteString("@@ -" + span(aStart, na) + " +" + span(bStart, nb) + " @@\n")
	for _, op := range ops {
		h.WriteByte(op.kind)
		switch op.kind {
		case '+':
			h.WriteString(bl[op.b])
		default:
			h.WriteString(al[op.a])
		}
		h.WriteByte('\n')
		lastA := op.kind != '+' && op.a == len(al)-1 && !aEOL
		lastB := op.kind != '-' && op.b == len(bl)-1 && !bEOL
		if lastA || lastB {
			h.WriteString("\\ No newline at end of file\n")
		}
	}
	return h.String()
}

// span renders a hunk range the way diff(1) does: "5" for one line.
func span(start, n int) string {
	if n == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(n)
}
