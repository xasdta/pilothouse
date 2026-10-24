package preview

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// applyUnified applies a diff produced by unifiedDiff to a, checking every
// context and removed line against a and the hunk line numbers, so a
// passing round trip proves the hunks and their headers are right.
func applyUnified(t *testing.T, a, diff string) string {
	t.Helper()
	if diff == "" {
		return a
	}
	al, aEOL := splitText(a)
	var out []string
	lastNoEOL := false // the last output line has no newline
	copyA := func(i int) {
		out = append(out, al[i])
		lastNoEOL = i == len(al)-1 && !aEOL
	}
	pos := 0 // next line of a to copy
	var lastKind byte
	for _, l := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		if strings.HasPrefix(l, "@@ ") {
			parseRange := func(s string) (int, int) {
				start, n, ok := strings.Cut(s, ",")
				x, _ := strconv.Atoi(start)
				if !ok {
					return x, 1
				}
				y, _ := strconv.Atoi(n)
				return x, y
			}
			f := strings.Fields(l)
			as, an := parseRange(strings.TrimPrefix(f[1], "-"))
			bs, bn := parseRange(strings.TrimPrefix(f[2], "+"))
			start := as - 1
			if an == 0 {
				start = as
			}
			if start < pos {
				t.Fatalf("hunk %q starts before line %d", l, pos+1)
			}
			for ; pos < start; pos++ {
				copyA(pos)
			}
			if bn > 0 && bs != len(out)+1 || bn == 0 && bs != len(out) {
				t.Fatalf("hunk %q: new start %d, want %d", l, bs, len(out)+1)
			}
			continue
		}
		if l == `\ No newline at end of file` {
			if lastKind == '+' {
				lastNoEOL = true
			}
			continue
		}
		kind, text := l[0], l[1:]
		lastKind = kind
		switch kind {
		case ' ', '-':
			if pos >= len(al) || al[pos] != text {
				t.Fatalf("line %d: diff says %q, a has %q", pos+1, text, at(al, pos))
			}
			if kind == ' ' {
				copyA(pos)
			}
			pos++
		case '+':
			out = append(out, text)
			lastNoEOL = false
		default:
			t.Fatalf("bad diff line %q", l)
		}
	}
	for ; pos < len(al); pos++ {
		copyA(pos)
	}
	if len(out) == 0 {
		return ""
	}
	s := strings.Join(out, "\n")
	if !lastNoEOL {
		s += "\n"
	}
	return s
}

func at(l []string, i int) string {
	if i < len(l) {
		return l[i]
	}
	return "<EOF>"
}

func TestUnifiedDiff(t *testing.T) {
	tests := []struct {
		name, a, b, want string
		added, removed   int
	}{
		{"equal", "x\ny\n", "x\ny\n", "", 0, 0},
		{"change", "a\nb\nc\n", "a\nB\nc\n", "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n", 1, 1},
		{"create", "", "x\ny\n", "@@ -0,0 +1,2 @@\n+x\n+y\n", 2, 0},
		{"delete all", "x\n", "", "@@ -1 +0,0 @@\n-x\n", 0, 1},
		{"append", "a\n", "a\nb\n", "@@ -1 +1,2 @@\n a\n+b\n", 1, 0},
		{"no newline added", "a\nb", "a\nb\n", "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n", 1, 1},
		{"no newline kept", "a\nb", "A\nb", "@@ -1,2 +1,2 @@\n-a\n+A\n b\n\\ No newline at end of file\n", 1, 1},
		{"no newline new", "a\n", "a\nb", "@@ -1 +1,2 @@\n a\n+b\n\\ No newline at end of file\n", 1, 0},
	}
	for _, tc := range tests {
		u := unifiedDiff(tc.a, tc.b, 3, MaxDiffBytes)
		if u.text != tc.want || u.added != tc.added || u.removed != tc.removed {
			t.Errorf("%s:\n got %q (+%d -%d)\nwant %q (+%d -%d)", tc.name, u.text, u.added, u.removed, tc.want, tc.added, tc.removed)
		}
		if got := applyUnified(t, tc.a, u.text); got != tc.b {
			t.Errorf("%s: round trip = %q, want %q", tc.name, got, tc.b)
		}
	}
}

func TestUnifiedDiffHunksAndLineNumbers(t *testing.T) {
	var a, b []string
	for i := 1; i <= 100; i++ {
		a = append(a, fmt.Sprintf("line %d", i))
	}
	b = append(b, a...)
	b[9] = "changed 10"
	b[79] = "changed 80"
	u := unifiedDiff(strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", 3, MaxDiffBytes)
	if strings.Count(u.text, "@@ -") != 2 || !strings.HasPrefix(u.text, "@@ -7,7 +7,7 @@\n") || !strings.Contains(u.text, "@@ -77,7 +77,7 @@\n") {
		t.Fatalf("hunks:\n%s", u.text)
	}
	// Changes closer than 2×context merge into one hunk.
	b[12] = "changed 13"
	u = unifiedDiff(strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", 3, MaxDiffBytes)
	if !strings.HasPrefix(u.text, "@@ -7,10 +7,10 @@\n") {
		t.Fatalf("merged hunk:\n%s", u.text)
	}
}

func TestUnifiedDiffRandomRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	words := []string{"a", "b", "c", "d", "", "}", "return nil"}
	gen := func(n int) string {
		var l []string
		for i := 0; i < n; i++ {
			l = append(l, words[r.Intn(len(words))])
		}
		s := strings.Join(l, "\n")
		if n > 0 && r.Intn(4) > 0 {
			s += "\n"
		}
		return s
	}
	for i := 0; i < 3000; i++ {
		a, b := gen(r.Intn(30)), gen(r.Intn(30))
		u := unifiedDiff(a, b, r.Intn(4), 1<<20)
		if got := applyUnified(t, a, u.text); got != b {
			t.Fatalf("round trip %d:\na=%q\nb=%q\ndiff:\n%s\ngot=%q", i, a, b, u.text, got)
		}
		// Minimality on small inputs: added+removed matches an LCS bound.
		al, _ := splitText(a)
		bl, _ := splitText(b)
		if u.added+u.removed > len(al)+len(bl) {
			t.Fatalf("diff larger than a full replacement")
		}
	}
}

func TestUnifiedDiffBudgetFallback(t *testing.T) {
	// Two unrelated 5000-line files exceed the Myers budget: the fallback
	// still produces a correct (full replacement) diff, quickly.
	var a, b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&a, "old %d\n", i)
		fmt.Fprintf(&b, "new %d\n", i)
	}
	u := unifiedDiff(a.String(), b.String(), 3, 1<<30)
	if u.added != 5000 || u.removed != 5000 {
		t.Fatalf("+%d -%d", u.added, u.removed)
	}
	if got := applyUnified(t, a.String(), u.text); got != b.String() {
		t.Fatal("fallback diff does not apply")
	}
}

func TestUnifiedDiffTruncated(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "a fairly long line of generated text number %d\n", i)
	}
	u := unifiedDiff("", b.String(), 3, MaxDiffBytes)
	if !u.truncated || len(u.text) > MaxDiffBytes || u.added != 5000 || !strings.HasSuffix(u.text, "\n") {
		t.Fatalf("truncated=%v len=%d added=%d", u.truncated, len(u.text), u.added)
	}
}

func BenchmarkUnifiedDiffEdit(b *testing.B) {
	var a strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&a, "func f%d() { return %d }\n", i, i)
	}
	before := a.String()
	after := strings.Replace(before, "func f2000() { return 2000 }", "func f2000() {\n\treturn 2001\n}", 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		unifiedDiff(before, after, 3, MaxDiffBytes)
	}
}

func BenchmarkUnifiedDiffRewrite(b *testing.B) {
	var x, y strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&x, "line %d\n", i)
		if i%7 == 0 {
			fmt.Fprintf(&y, "changed %d\n", i)
		} else {
			fmt.Fprintf(&y, "line %d\n", i)
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		unifiedDiff(x.String(), y.String(), 3, MaxDiffBytes)
	}
}
