package task

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Bounds on what task.yaml's copy: may bring into one worktree.
const (
	maxCopyFiles = 200
	maxCopyBytes = 50 << 20
)

// copyResult is what copyFiles did. Paths are repo-relative and
// slash-separated.
type copyResult struct {
	Copied  []string
	Skipped []string // "path: reason", for the person
}

// copier copies files from the main checkout (src) into a new worktree
// (dst) under the rules of task.yaml's copy:
//
//   - only paths inside src: a symlink (to a file) is copied as the file
//     it points to, and only when that file is inside src too; symlinked
//     directories are not traversed; .git is never read;
//   - only regular files: devices, FIFOs and sockets are skipped;
//   - never through a symlink in dst, and never over a file the worktree
//     already has (the checkout's tracked files win);
//   - at most maxCopyFiles files and maxCopyBytes bytes.
type copier struct {
	src, dst string // symlinks resolved
	res      copyResult
	seen     map[string]bool
	bytes    int64
	full     bool
}

func copyFiles(src, dst string, patterns []string) (copyResult, error) {
	var err error
	c := &copier{seen: map[string]bool{}}
	if c.src, err = filepath.EvalSymlinks(src); err != nil {
		return c.res, err
	}
	if c.dst, err = filepath.EvalSymlinks(dst); err != nil {
		return c.res, err
	}
	for _, p := range patterns {
		if checkCopyPattern(p) != nil { // validated on load; never trust a caller
			c.skip(p, "not a valid pattern")
			continue
		}
		for _, rel := range c.glob(p) {
			c.copyPath(rel)
		}
	}
	return c.res, nil
}

func (c *copier) skip(rel, why string) {
	if len(c.res.Skipped) < 50 {
		c.res.Skipped = append(c.res.Skipped, rel+": "+why)
	}
}

func hasMeta(seg string) bool { return strings.ContainsAny(seg, `*?[\`) }

// glob expands one pattern segment by segment under c.src, so the
// repository path itself is never read as a pattern, and intermediate
// directories are checked to be real directories (not symlinks).
func (c *copier) glob(pattern string) []string {
	segs := strings.Split(pattern, "/")
	cur := []string{""}
	for i, seg := range segs {
		last := i == len(segs)-1
		var next []string
		for _, dir := range cur {
			var names []string
			if !hasMeta(seg) {
				names = []string{seg}
			} else {
				ents, err := os.ReadDir(filepath.Join(c.src, filepath.FromSlash(dir)))
				if err != nil {
					continue
				}
				for _, e := range ents {
					if ok, _ := filepath.Match(seg, e.Name()); ok {
						names = append(names, e.Name())
					}
				}
			}
			for _, n := range names {
				if n == ".git" {
					continue
				}
				rel := path.Join(dir, n)
				fi, err := os.Lstat(filepath.Join(c.src, filepath.FromSlash(rel)))
				if err != nil {
					continue
				}
				switch {
				case last:
					next = append(next, rel)
				case fi.IsDir():
					next = append(next, rel)
				case fi.Mode()&fs.ModeSymlink != 0:
					c.skip(rel, "symlinked directory, not followed")
				}
			}
		}
		cur = next
	}
	return cur
}

// copyPath copies one match: a file, a symlink to a file inside the
// repository, or a directory (recursively).
func (c *copier) copyPath(rel string) {
	abs := filepath.Join(c.src, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil {
		return
	}
	if !fi.IsDir() {
		c.copyEntry(rel, abs, fi)
		return
	}
	_ = filepath.WalkDir(abs, func(p string, e fs.DirEntry, err error) error {
		if err != nil || c.full {
			if c.full {
				return filepath.SkipAll
			}
			return nil
		}
		if e.IsDir() {
			if e.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		r, err := filepath.Rel(c.src, p)
		if err != nil {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		c.copyEntry(filepath.ToSlash(r), p, info)
		return nil
	})
}

// copyEntry copies a regular file, or the file a symlink points to when it
// stays inside the repository, to the same relative path in dst.
func (c *copier) copyEntry(rel, abs string, fi fs.FileInfo) {
	if c.seen[rel] {
		return
	}
	c.seen[rel] = true
	if c.full {
		return
	}
	from := abs
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(abs)
		if err != nil {
			c.skip(rel, "broken symlink")
			return
		}
		if !inside(c.src, target) || hasGitDir(c.src, target) {
			c.skip(rel, "symlink to a file outside the repository")
			return
		}
		if fi, err = os.Lstat(target); err != nil {
			return
		}
		from = target
	}
	if !fi.Mode().IsRegular() {
		c.skip(rel, "not a regular file")
		return
	}
	if len(c.res.Copied) >= maxCopyFiles || c.bytes+fi.Size() > maxCopyBytes {
		c.full = true
		c.skip(rel, fmt.Sprintf("copy limit reached (%d files, %d MB)", maxCopyFiles, maxCopyBytes>>20))
		return
	}
	n, err := c.copyFile(rel, from)
	switch {
	case errors.Is(err, fs.ErrExist):
		c.skip(rel, "already in the worktree")
	case err != nil:
		c.skip(rel, err.Error())
	default:
		c.bytes += n
		c.res.Copied = append(c.res.Copied, rel)
	}
}

func (c *copier) copyFile(rel, from string) (int64, error) {
	in, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, errors.New("cannot read it")
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return 0, errors.New("not a regular file")
	}
	if err := c.mkdirs(path.Dir(rel)); err != nil {
		return 0, err
	}
	dst := filepath.Join(c.dst, filepath.FromSlash(rel))
	// O_EXCL|O_NOFOLLOW: never replace what the checkout has, never
	// write through a symlink at the destination.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, fi.Mode().Perm())
	if err != nil {
		if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ELOOP) {
			return 0, fs.ErrExist
		}
		return 0, errors.New("cannot create it in the worktree")
	}
	budget := maxCopyBytes - c.bytes
	n, err := io.Copy(out, io.LimitReader(in, budget+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > budget {
		err = errors.New("grew past the copy limit while being copied")
	}
	if err != nil {
		_ = os.Remove(dst)
		return 0, err
	}
	return n, nil
}

// mkdirs creates dir (relative, slash-separated) inside dst one component
// at a time, refusing to go through anything that is not a real directory.
// New directories take the permissions of the matching source directory.
func (c *copier) mkdirs(dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(dir, "/") {
		cur = path.Join(cur, seg)
		p := filepath.Join(c.dst, filepath.FromSlash(cur))
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			perm := fs.FileMode(0o755)
			if sfi, err := os.Lstat(filepath.Join(c.src, filepath.FromSlash(cur))); err == nil && sfi.IsDir() {
				perm = sfi.Mode().Perm() | 0o700 // we still have to write into it
			}
			if err := os.Mkdir(p, perm); err != nil && !errors.Is(err, fs.ErrExist) {
				return errors.New("cannot create its directory in the worktree")
			}
			if fi, err = os.Lstat(p); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s in the worktree is not a directory (not writing through it)", cur)
		}
	}
	return nil
}

// inside reports whether p is root or below it (both symlink-resolved).
func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// hasGitDir reports whether p (inside root) is in root's .git directory.
func hasGitDir(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".git" {
			return true
		}
	}
	return false
}
