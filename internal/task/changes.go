package task

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/xasdta/pilothouse/internal/api"
)

// Bounds on a task diff for the inbox.
const (
	maxDiffFiles     = 300
	maxFileDiff      = 64 << 10 // per file
	maxTotalDiff     = 1 << 20  // all files together
	maxGitMeta       = 1 << 20  // --raw, --numstat and ls-files output
	maxPatchRead     = 2 << 20  // the patch itself
	maxUntrackedRead = 1 << 20  // read of one untracked file
	binarySniff      = 8000     // git's own heuristic: a NUL in the first 8000 bytes
)

// Changes is what a task changed relative to where its branch left the
// base, as file previews for the inbox: committed, uncommitted and
// untracked work in the worktree, without the files task.yaml copied in.
// Once the worktree is gone it is the branch's commits. Every git call is
// read-only and bounded; untracked files are read without following
// symlinks.
func Changes(ctx context.Context, t api.Task) (api.TaskDiff, error) {
	d := api.TaskDiff{TaskID: t.ID, Files: []api.FilePreview{}}
	dir, revs := t.Worktree, []string{}
	worktree := dirExists(t.Worktree)
	if worktree {
		mb, err := gitOutput(ctx, t.Worktree, "merge-base", t.BaseRef, "HEAD")
		if err != nil {
			return d, fmt.Errorf("can't find where %s left %s: %w", t.Branch, t.BaseRef, err)
		}
		revs = []string{mb} // the commit against the working tree
	} else {
		dir = t.Repo
		if !gitOK(ctx, t.Repo, "show-ref", "--verify", "--quiet", "refs/heads/"+t.Branch) {
			return d, fmt.Errorf("the task's worktree and its branch %s are gone", t.Branch)
		}
		mb, err := gitOutput(ctx, t.Repo, "merge-base", t.BaseRef, t.Branch)
		if err != nil {
			return d, fmt.Errorf("can't find where %s left %s: %w", t.Branch, t.BaseRef, err)
		}
		revs = []string{mb, "refs/heads/" + t.Branch}
	}

	diffArgs := func(format ...string) []string {
		a := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M"}, format...)
		return append(append(a, revs...), "--")
	}
	raw, rawCut, err := gitRead(ctx, dir, maxGitMeta, diffArgs("--raw", "-z")...)
	if err != nil {
		return d, err
	}
	num, _, err := gitRead(ctx, dir, maxGitMeta, diffArgs("--numstat", "-z")...)
	if err != nil {
		return d, err
	}
	files := parseRaw(raw)
	counts := parseNumstat(num)
	patch, patchCut, err := gitRead(ctx, dir, maxPatchRead, diffArgs("--patch", "--unified=3")...)
	if err != nil {
		return d, err
	}
	sections := splitPatch(patch)
	d.Truncated = rawCut || patchCut

	total := 0
	for i := range files {
		f := &files[i]
		if i < len(counts) && counts[i].path == f.Path {
			f.Added, f.Removed = counts[i].added, counts[i].removed
		}
		switch {
		case i >= len(sections):
			f.Truncated = f.Added+f.Removed > 0
		case total >= maxTotalDiff:
			f.Truncated, d.Truncated = true, true
		default:
			f.Diff, f.Truncated = hunks(sections[i], maxFileDiff)
			if i == len(sections)-1 && patchCut {
				f.Truncated = true
			}
			total += len(f.Diff)
		}
	}

	if worktree {
		copied := map[string]bool{}
		if t.Setup != nil {
			for _, c := range t.Setup.Copied {
				copied[c] = true
			}
		}
		ls, lsCut, err := gitRead(ctx, t.Worktree, maxGitMeta, "ls-files", "--others", "--exclude-standard", "-z")
		if err != nil {
			return d, err
		}
		d.Truncated = d.Truncated || lsCut
		for _, rel := range strings.Split(string(ls), "\x00") {
			if rel == "" || copied[rel] {
				continue
			}
			if len(files) >= maxDiffFiles {
				d.Truncated = true
				break
			}
			fp, ok := untrackedPreview(t.Worktree, rel, total < maxTotalDiff)
			if !ok {
				continue
			}
			total += len(fp.Diff)
			files = append(files, fp)
		}
	}
	if len(files) > maxDiffFiles {
		files, d.Truncated = files[:maxDiffFiles], true
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		d.Added += f.Added
		d.Removed += f.Removed
	}
	d.Files = files
	return d, nil
}

// gitRead runs a read-only git command and returns at most limit bytes of
// its output; cut says there was more (git is then stopped). Whatever can
// write to the repository can also configure it, so external diff
// drivers, textconv and the fsmonitor hook are off, and git takes no
// optional locks next to an agent that may be using the same repository.
func gitRead(ctx context.Context, dir string, limit int64, args ...string) (out []byte, cut bool, err error) {
	full := append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "GIT_EXTERNAL_DIFF=")
	var errb bytes.Buffer
	cmd.Stderr = &limitedBuffer{b: &errb, left: 16 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, gitError(args, err, "")
	}
	out, rerr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(out)) > limit {
		out, cut = out[:limit], true
		_ = cmd.Process.Kill()
	}
	werr := cmd.Wait()
	switch {
	case rerr != nil:
		return nil, false, rerr
	case cut:
		return out, true, nil
	case werr != nil:
		return nil, false, gitError(args, werr, errb.String())
	}
	return out, false, nil
}

type limitedBuffer struct {
	b    *bytes.Buffer
	left int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if n := min(len(p), l.left); n > 0 {
		l.b.Write(p[:n])
		l.left -= n
	}
	return len(p), nil
}

// parseRaw reads `git diff --raw -z`: ":srcmode dstmode srcsha dstsha
// status\0path\0" (two paths for renames and copies).
func parseRaw(b []byte) []api.FilePreview {
	var out []api.FilePreview
	tok := strings.Split(string(b), "\x00")
	for i := 0; i < len(tok); i++ {
		meta := tok[i]
		if !strings.HasPrefix(meta, ":") {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 5 || i+1 >= len(tok) {
			break
		}
		status := fields[4]
		f := api.FilePreview{Path: tok[i+1], Op: api.FileEdit}
		i++
		switch status[0] {
		case 'A', 'C':
			f.Op = api.FileCreate
		case 'D':
			f.Op = api.FileDelete
		}
		if status[0] == 'R' || status[0] == 'C' {
			if i+1 >= len(tok) {
				break
			}
			f.Path = tok[i+1] // the new name
			if status[0] == 'R' {
				f.Op = api.FileRename
			}
			i++
		}
		f.Lang = langFor(f.Path)
		out = append(out, f)
	}
	return out
}

type numstat struct {
	path           string
	added, removed int
}

// parseNumstat reads `git diff --numstat -z`: "added\tremoved\tpath\0", or
// "added\tremoved\t\0src\0dst\0" for renames; "-" counts are binary.
func parseNumstat(b []byte) []numstat {
	var out []numstat
	tok := strings.Split(string(b), "\x00")
	for i := 0; i < len(tok); i++ {
		parts := strings.SplitN(tok[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		n := numstat{path: parts[2]}
		n.added, _ = strconv.Atoi(parts[0])
		n.removed, _ = strconv.Atoi(parts[1])
		if n.path == "" && i+2 < len(tok) {
			n.path = tok[i+2]
			i += 2
		}
		out = append(out, n)
	}
	return out
}

// splitPatch cuts a patch into one section per file, in git's order (the
// same order as --raw and --numstat).
func splitPatch(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i < len(b); {
		if bytes.HasPrefix(b[i:], []byte("diff --git ")) {
			if start >= 0 {
				out = append(out, b[start:i])
			}
			start = i
		}
		nl := bytes.IndexByte(b[i:], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	if start >= 0 {
		out = append(out, b[start:])
	}
	return out
}

// hunks is a file section without its header (diff --git, index, ---/+++),
// cut at a line boundary to at most limit bytes.
func hunks(section []byte, limit int) (string, bool) {
	at := bytes.Index(section, []byte("\n@@"))
	if at < 0 {
		return "", false // binary, mode-only or empty
	}
	h := section[at+1:]
	if len(h) <= limit {
		return string(h), false
	}
	h = h[:limit]
	if nl := bytes.LastIndexByte(h, '\n'); nl > 0 {
		h = h[:nl+1]
	}
	return string(h), true
}

// untrackedPreview shows a new file the agent has not added to git. A
// symlink is shown as its target (like git does), never followed; other
// non-regular files are skipped.
func untrackedPreview(root, rel string, withDiff bool) (api.FilePreview, bool) {
	fp := api.FilePreview{Path: rel, Op: api.FileCreate, Lang: langFor(rel)}
	p := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(p)
	if err != nil {
		return fp, false
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return fp, false
		}
		fp.Added = 1
		if withDiff {
			fp.Diff = "@@ -0,0 +1 @@\n+" + target + "\n\\ No newline at end of file\n"
		}
		return fp, true
	}
	if !fi.Mode().IsRegular() {
		return fp, false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fp, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxUntrackedRead+1))
	if err != nil {
		return fp, false
	}
	if len(b) > maxUntrackedRead {
		b, fp.Truncated = b[:maxUntrackedRead], true
	}
	if bytes.IndexByte(b[:min(len(b), binarySniff)], 0) >= 0 {
		return fp, true // binary: no lines to count
	}
	if len(b) == 0 {
		return fp, true
	}
	text := string(b)
	noEOL := !strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	fp.Added = len(lines)
	if !withDiff {
		fp.Truncated = true
		return fp, true
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ -0,0 +1,%d @@\n", len(lines))
	for i, l := range lines {
		if sb.Len()+len(l)+2 > maxFileDiff {
			fp.Truncated = true
			break
		}
		sb.WriteString("+")
		sb.WriteString(l)
		sb.WriteString("\n")
		if i == len(lines)-1 && noEOL && !fp.Truncated {
			sb.WriteString("\\ No newline at end of file\n")
		}
	}
	fp.Diff = sb.String()
	return fp, true
}

// langFor is a highlighting hint from a file's extension.
func langFor(p string) string {
	base := strings.ToLower(path.Base(p))
	switch base {
	case "makefile":
		return "make"
	case "dockerfile":
		return "docker"
	}
	switch strings.TrimPrefix(path.Ext(base), ".") {
	case "go":
		return "go"
	case "ts", "tsx", "mts", "cts":
		return "ts"
	case "js", "jsx", "mjs", "cjs":
		return "js"
	case "py":
		return "py"
	case "rb":
		return "rb"
	case "rs":
		return "rs"
	case "java":
		return "java"
	case "kt", "kts":
		return "kt"
	case "swift":
		return "swift"
	case "c", "h":
		return "c"
	case "cc", "cpp", "cxx", "hpp", "hh":
		return "cpp"
	case "cs":
		return "cs"
	case "php":
		return "php"
	case "sh", "bash", "zsh":
		return "sh"
	case "md", "markdown":
		return "md"
	case "json":
		return "json"
	case "yaml", "yml":
		return "yaml"
	case "toml":
		return "toml"
	case "html", "htm":
		return "html"
	case "css", "scss":
		return "css"
	case "sql":
		return "sql"
	}
	return ""
}
