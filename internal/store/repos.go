package store

import (
	"context"
	"fmt"
	"time"
)

// RepoUse is a repository root sessions ran in, and when one last did.
type RepoUse struct {
	Root     string
	LastUsed time.Time
}

// RepoRoots returns the distinct repo roots of recorded sessions, most
// recently used first, at most limit of them.
func (r Reader) RepoRoots(ctx context.Context, limit int) ([]RepoUse, error) {
	rows, err := r.q.QueryContext(ctx, `
SELECT json_extract(data, '$.repo_root') AS root, MAX(updated_at) AS last
FROM sessions
WHERE root IS NOT NULL AND root != ''
GROUP BY root
ORDER BY last DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list repo roots: %w", err)
	}
	defer rows.Close()
	var out []RepoUse
	for rows.Next() {
		var (
			root string
			last int64
		)
		if err := rows.Scan(&root, &last); err != nil {
			return nil, fmt.Errorf("list repo roots: %w", err)
		}
		out = append(out, RepoUse{Root: root, LastUsed: time.Unix(0, last).UTC()})
	}
	return out, rows.Err()
}
