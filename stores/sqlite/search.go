package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/erain/glue"
)

// Search implements glue.Searcher against the FTS5 index. Hits are
// ordered by BM25 ascending (lower is better, matching FTS5's
// convention). See docs/adr/0007-memory-layer.md §3.
//
// query is forwarded straight to messages_fts MATCH; FTS5's standard
// syntax applies (bare words, "quoted phrases", AND / OR / NOT).
func (s *Store) Search(ctx context.Context, query string, opts glue.SearchOptions) ([]glue.SearchHit, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("sqlite: nil store")
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("sqlite: search query is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = glue.DefaultSearchLimit
	}
	if opts.Limit > glue.MaxSearchLimit {
		opts.Limit = glue.MaxSearchLimit
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}

	var sinceUnix, untilUnix int64
	if !opts.Since.IsZero() {
		sinceUnix = opts.Since.Unix()
	}
	if !opts.Until.IsZero() {
		untilUnix = opts.Until.Unix()
	}

	const sqlText = `
SELECT m.session_id,
       m.ord,
       m.role,
       m.ts,
       snippet(messages_fts, 0, '<<', '>>', '...', 16) AS snippet,
       bm25(messages_fts) AS score
  FROM messages_fts
  JOIN messages m ON m.rowid = messages_fts.rowid
 WHERE messages_fts MATCH ?
   AND ( ? = '' OR m.session_id = ? )
   AND ( ? = 0  OR m.ts >= ? )
   AND ( ? = 0  OR m.ts <= ? )
 ORDER BY score ASC
 LIMIT ? OFFSET ?
`
	rows, err := s.db.QueryContext(ctx, sqlText,
		query,
		opts.SessionID, opts.SessionID,
		sinceUnix, sinceUnix,
		untilUnix, untilUnix,
		opts.Limit, opts.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search: %w", err)
	}
	defer rows.Close()

	out := make([]glue.SearchHit, 0, opts.Limit)
	for rows.Next() {
		var (
			sessionID string
			ord       int
			role      string
			tsUx      int64
			snippet   string
			score     float64
		)
		if err := rows.Scan(&sessionID, &ord, &role, &tsUx, &snippet, &score); err != nil {
			return nil, fmt.Errorf("sqlite: scan hit: %w", err)
		}
		out = append(out, glue.SearchHit{
			SessionID: sessionID,
			Index:     ord,
			Role:      glue.MessageRole(role),
			Snippet:   snippet,
			Score:     score,
			Timestamp: time.Unix(tsUx, 0).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Compile-time assertion that *Store implements glue.Searcher.
var _ glue.Searcher = (*Store)(nil)
