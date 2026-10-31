package transcript

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// OpenCode keeps its sessions in a SQLite database rather than a JSONL file.
// ReadOpenCode reads the messages and parts OpenCode has already stored for
// one session. It is read-only and notices the same fields the CLI shows.
func OpenCodeDB() string {
	if p := os.Getenv("OPENCODE_DB"); p != "" {
		return p
	}
	if d := os.Getenv("OPENCODE_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode.db")
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(paths.UserHome(), ".local", "share")
	}
	return filepath.Join(base, "opencode", "opencode.db")
}

type ocPart struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Tool   string `json:"tool"`
	CallID string `json:"callID"`
	State  struct {
		Status   string          `json:"status"`
		Input    json.RawMessage `json:"input"`
		Output   string          `json:"output"`
		Title    string          `json:"title"`
		Metadata struct {
			Exit *int `json:"exit"`
		} `json:"metadata"`
	} `json:"state"`
	Time struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"time"`
}

// ReadOpenCode reads a session's messages and tool calls. The cursor is a
// high-water mark of part update time, so a tool result that arrives later is
// returned even though its position in the feed does not move.
func ReadOpenCode(dbPath, sessionID, cwd, cursor string, limit int, now time.Time) (Result, error) {
	if sessionID == "" {
		return Result{}, fmt.Errorf("no OpenCode session id")
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	db, err := openOpenCode(dbPath)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()

	usage, err := readOpenCodeUsage(db, sessionID)
	if err != nil {
		return Result{}, err
	}
	turns, err := readOpenCodeTurns(db, sessionID)
	if err != nil {
		return Result{}, err
	}

	rows, err := openCodeRows(db, sessionID, cursor, limit)
	if err != nil {
		return Result{}, err
	}
	items := make([]api.ActivityItem, 0, len(rows))
	for _, r := range rows {
		var p ocPart
		if json.Unmarshal([]byte(r.data), &p) != nil {
			continue
		}
		at := time.UnixMilli(r.created).UTC()
		switch p.Type {
		case "text":
			text := strings.TrimSpace(p.Text)
			if text == "" {
				continue
			}
			kind, source := api.ActivityText, ""
			if r.role == "user" {
				kind, source = api.ActivityPrompt, "human"
			}
			items = append(items, api.ActivityItem{
				ID: "oc-" + r.id, Kind: kind, At: at, Turn: turns[r.messageID],
				Text: capHead(text, maxText), Source: source,
			})
		case "tool":
			items = append(items, openCodeTool(r.id, at, turns[r.messageID], cwd, p))
		case "compaction":
			items = append(items, api.ActivityItem{
				ID: "oc-" + r.id, Kind: api.ActivitySystem, At: at, Turn: turns[r.messageID],
				Text: "Compacted the conversation",
			})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].At.Equal(items[j].At) {
			return items[i].ID < items[j].ID
		}
		return items[i].At.Before(items[j].At)
	})
	next, err := openCodeCursor(db, sessionID, cursor)
	if err != nil {
		return Result{}, err
	}
	res := Result{Items: items, Cursor: next, Usage: usage}
	if cursor == "" { // a cursor read has only the newest parts: too few for a receipt
		res.Receipt = ReceiptOf(items, nil)
	}
	return res, nil
}

type ocRow struct {
	id, messageID, role, data string
	created, updated          int64
}

func openOpenCode(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMissing
		}
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func openCodeRows(db *sql.DB, sessionID, cursor string, limit int) ([]ocRow, error) {
	base := `SELECT p.id, p.message_id, p.time_created, p.time_updated, p.data, m.data
		FROM part p JOIN message m ON m.id = p.message_id
		WHERE p.session_id = ?`
	var (
		rows *sql.Rows
		err  error
	)
	if at, ok := openCodeCursorTime(cursor); ok {
		rows, err = db.Query(base+` AND p.time_updated >= ? ORDER BY p.time_updated ASC, p.id ASC LIMIT ?`, sessionID, at, limit)
	} else {
		rows, err = db.Query(base+` ORDER BY p.time_created DESC, p.id DESC LIMIT ?`, sessionID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ocRow
	for rows.Next() {
		var (
			r       ocRow
			pdata   string
			mdata   string
			created int64
			updated int64
		)
		if err := rows.Scan(&r.id, &r.messageID, &created, &updated, &pdata, &mdata); err != nil {
			return nil, err
		}
		var m struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal([]byte(mdata), &m)
		r.role, r.data, r.created, r.updated = m.Role, pdata, created, updated
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, ok := openCodeCursorTime(cursor); !ok {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

func openCodeCursor(db *sql.DB, sessionID, cursor string) (string, error) {
	at, ok := openCodeCursorTime(cursor)
	var max sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(time_updated) FROM part WHERE session_id = ?`, sessionID).Scan(&max); err != nil {
		return "", err
	}
	if !ok || max.Int64 > at {
		at = max.Int64
	}
	if at <= 0 {
		return cursor, nil
	}
	return strconv.FormatInt(at+1, 10), nil
}

func openCodeCursorTime(cursor string) (int64, bool) {
	n, err := strconv.ParseInt(cursor, 10, 64)
	return n, err == nil && n > 0
}

func readOpenCodeUsage(db *sql.DB, sessionID string) (api.Usage, error) {
	var (
		u     api.Usage
		model string
	)
	err := db.QueryRow(`SELECT cost, tokens_input, tokens_output, tokens_reasoning,
		tokens_cache_read, tokens_cache_write, model FROM session WHERE id = ?`, sessionID).
		Scan(&u.CostUSD, &u.InputTokens, &u.OutputTokens, &u.ReasoningTokens,
			&u.CacheReadTokens, &u.CacheWriteTokens, &model)
	if err == sql.ErrNoRows {
		return api.Usage{}, ErrMissing
	}
	if err != nil {
		return api.Usage{}, err
	}
	var m struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(model), &m)
	u.Model = m.ID
	if err := db.QueryRow(`SELECT COUNT(*) FROM message WHERE session_id = ?
		AND json_extract(data, '$.role') = 'user'`, sessionID).Scan(&u.Turns); err != nil {
		return api.Usage{}, err
	}
	return u, nil
}

func readOpenCodeTurns(db *sql.DB, sessionID string) (map[string]int, error) {
	rows, err := db.Query(`SELECT id, data FROM message WHERE session_id = ? ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := map[string]int{}
	turn := 0
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var m struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal([]byte(data), &m)
		if m.Role == "user" {
			turn++
		}
		turns[id] = turn
	}
	return turns, rows.Err()
}

func ocPath(cwd, path, title, fallback string) string {
	if path == "" {
		path = strings.TrimSpace(title)
	}
	if path == "" {
		return fallback
	}
	return hookkit.RelPath(cwd, path)
}

func openCodeTool(partID string, at time.Time, turn int, cwd string, p ocPart) api.ActivityItem {
	it := api.ActivityItem{
		ID: "oc-" + partID, Kind: api.ActivityTool, At: at, Turn: turn,
		ToolKind: api.ToolOther,
	}
	var in map[string]any
	_ = json.Unmarshal(p.State.Input, &in)
	field := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	switch strings.ToLower(p.Tool) {
	case "bash", "shell", "terminal":
		it.ToolKind = api.ToolShell
		cmd := field("command")
		if cmd == "" {
			cmd = strings.TrimSpace(p.State.Title)
		}
		if cmd != "" {
			it.Summary = "Run: " + hookkit.OneLine(cmd, 160)
		} else {
			it.Summary = "Run a command"
		}
	case "read":
		it.ToolKind = api.ToolRead
		it.Summary = "Read: " + ocPath(cwd, field("filePath", "path"), p.State.Title, "a file")
	case "write":
		it.ToolKind = api.ToolEdit
		it.Summary = "Write: " + ocPath(cwd, field("filePath", "path"), p.State.Title, "a file")
	case "edit", "patch", "multiedit":
		it.ToolKind = api.ToolEdit
		it.Summary = "Edit: " + ocPath(cwd, field("filePath", "path"), p.State.Title, "a file")
	case "grep", "search":
		it.ToolKind = api.ToolRead
		it.Summary = "Search: " + field("pattern", "query")
	case "glob", "list", "ls":
		it.ToolKind = api.ToolRead
		it.Summary = "List: " + field("pattern", "path")
	case "webfetch", "fetch":
		it.ToolKind = api.ToolWeb
		it.Summary = "Fetch: " + field("url", "query")
	case "task":
		it.Summary = "Subagent: " + field("description", "prompt")
	default:
		it.Summary = strings.TrimSpace(p.State.Title)
		if it.Summary == "" {
			it.Summary = "Tool: " + p.Tool
		}
	}
	if it.Summary == "" {
		it.Summary = "Tool: " + p.Tool
	}
	status := strings.ToLower(p.State.Status)
	if status == "" || status == "pending" || status == "running" || status == "in_progress" {
		it.Pending = true
		return it
	}
	it.Result = capTail(strings.TrimSpace(p.State.Output), maxResult)
	it.IsError = status == "error" || status == "failed" || (p.State.Metadata.Exit != nil && *p.State.Metadata.Exit != 0)
	return it
}
