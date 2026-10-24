package transcript

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// Read limits. Transcripts grow without bound (a long Claude session is
// easily 100 MB, most of it tool output), so every read is bounded.
const (
	// DefaultMaxLine: longer lines are skipped without being buffered.
	DefaultMaxLine = 8 << 20
	// DefaultFirstScan: a file seen for the first time is parsed in full
	// from at most this far before its end, so opening a huge transcript
	// takes bounded time. Before that, only usage and prompts are counted
	// (the head pass), up to DefaultMaxHead more bytes.
	DefaultFirstScan = 64 << 20
	DefaultMaxHead   = 1 << 30
	// DefaultMaxRead bounds the bytes one refresh reads; the rest is read
	// on the next refresh.
	DefaultMaxRead = 64 << 20

	headLen   = 256 // bytes compared to notice a file rewritten in place
	chunkSize = 64 << 10
)

// tail remembers how far a file has been read.
type tail struct {
	info    os.FileInfo // identity of the file last read (os.SameFile)
	off     int64       // offset just after the last complete line
	head    []byte      // first bytes of the file when first read
	skip    bool        // inside an over-long line: drop bytes up to '\n'
	partial bool        // the file's start was never read, not even by the head pass
	skipped int         // over-long lines dropped
}

type tailLimits struct {
	maxLine   int
	firstScan int64
	maxHead   int64
	maxRead   int64
}

// lineFunc receives a complete line (without its newline) and the offset
// where it starts.
type lineFunc func(line []byte, off int64)

// errReset is returned when the file was replaced, truncated or rewritten
// since the last read: whatever was derived from it is stale. The tail is
// rewound, so the next read starts over.
var errReset = errors.New("transcript was replaced")

// read calls fn with every complete line appended since the last read. A
// trailing line without a newline is left for the next read: the agent is
// still writing it. On the first read of a file bigger than firstScan,
// headFn (when set) first gets the lines before the part fn reads.
func (t *tail) read(f *os.File, st os.FileInfo, lim tailLimits, fn, headFn lineFunc) error {
	size := st.Size()
	if t.info != nil {
		if !os.SameFile(t.info, st) || size < t.off || !t.sameHead(f) {
			*t = tail{}
			return errReset
		}
	}
	first := t.info == nil
	t.info = st
	if t.head == nil && size > 0 {
		t.head = readAt(f, 0, min(headLen, size))
	}
	if first && size > lim.firstScan {
		cut := size - lim.firstScan
		start, ok := nextLineStart(f, cut, size, lim.maxLine)
		if ok {
			t.off = start
		} else {
			t.off, t.skip = cut, true // an over-long line straddles the cut
			start = cut
		}
		from := max(0, cut-lim.maxHead)
		t.partial = from > 0 || headFn == nil
		if headFn != nil {
			sc := scanner{maxLine: lim.maxLine, skip: from > 0 && !bytes.Equal(readAt(f, from-1, 1), []byte{'\n'})}
			if err := sc.scan(f, from, start, headFn); err != nil {
				return err
			}
			t.skipped += sc.skipped
		}
	}
	if t.off >= size {
		return nil
	}
	sc := scanner{maxLine: lim.maxLine, skip: t.skip}
	err := sc.scan(f, t.off, min(size, t.off+lim.maxRead), fn)
	t.off, t.skip = sc.end, sc.skip
	t.skipped += sc.skipped
	return err
}

// nextLineStart finds the first line start at or after cut (cut itself
// when the byte before it is a newline). ok is false when no newline
// follows within maxLine bytes.
func nextLineStart(f *os.File, cut, size int64, maxLine int) (int64, bool) {
	if bytes.Equal(readAt(f, cut-1, 1), []byte{'\n'}) {
		return cut, true
	}
	buf := make([]byte, chunkSize)
	for pos := cut; pos < size && pos-cut <= int64(maxLine); {
		n, err := f.ReadAt(buf[:min(int64(len(buf)), size-pos)], pos)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1, true
		}
		pos += int64(n)
		if err != nil || n == 0 {
			break
		}
	}
	return 0, false
}

// scanner splits [from, to) of a file into lines.
type scanner struct {
	maxLine int
	skip    bool  // in: the range starts inside a line to drop; out: it ends inside an over-long line
	end     int64 // out: offset after the last complete line (or `to` while skipping)
	skipped int   // out: over-long lines dropped
}

func (s *scanner) scan(f *os.File, from, to int64, fn lineFunc) error {
	s.end = from
	if from >= to {
		return nil
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	var (
		buf   = make([]byte, chunkSize)
		line  []byte
		pos   = from // offset of the next unread byte
		start = from // offset of the current line's first byte
	)
	for pos < to {
		n, err := f.Read(buf[:min(int64(len(buf)), to-pos)])
		chunk := buf[:n]
		for len(chunk) > 0 {
			i := bytes.IndexByte(chunk, '\n')
			if i < 0 {
				if !s.skip {
					if len(line)+len(chunk) > s.maxLine {
						s.skip, line = true, nil
						s.skipped++
					} else {
						line = append(line, chunk...)
					}
				}
				break
			}
			if !s.skip {
				if len(line)+i > s.maxLine {
					s.skipped++
				} else {
					line = append(line, chunk[:i]...)
					if l := bytes.TrimRight(line, "\r"); len(bytes.TrimSpace(l)) > 0 {
						fn(l, start)
					}
				}
			}
			s.skip, line = false, line[:0]
			lineEnd := pos + int64(i) + 1
			pos, start, chunk = lineEnd, lineEnd, chunk[i+1:]
			s.end = lineEnd
		}
		pos += int64(len(chunk))
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if n == 0 {
			break
		}
	}
	if s.skip {
		// Inside an over-long line: nothing before pos is worth reading
		// again.
		s.end = pos
	}
	return nil
}

// sameHead reports whether the file still starts with the bytes it
// started with when first read.
func (t *tail) sameHead(f *os.File) bool {
	if len(t.head) == 0 {
		return true
	}
	return bytes.Equal(readAt(f, 0, int64(len(t.head))), t.head)
}

func readAt(f *os.File, off, n int64) []byte {
	if off < 0 || n <= 0 {
		return nil
	}
	b := make([]byte, n)
	m, _ := f.ReadAt(b, off)
	return b[:m]
}
