package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// discoverIndexed builds a disposable FTS5 projection of eligible delivered
// records. Native traces live in state_meta, not messages_fts. The projection
// keeps their search policy intact without changing operator database objects.
func (s *ProfileStore) discoverIndexed(ctx context.Context, owner string, filter SearchFilter) ([]SearchSession, error) {
	excluded, err := s.excludedLineage(ctx, owner, filter.Exclude)
	if err != nil {
		return nil, err
	}
	if err := s.addLiveLineage(ctx, owner, filter.LiveSessionID, excluded); err != nil {
		return nil, err
	}
	index, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open recall projection: %w", err)
	}
	defer index.Close()
	index.SetMaxOpenConns(1)
	if _, err := index.ExecContext(ctx, `PRAGMA temp_store=MEMORY`); err != nil {
		return nil, fmt.Errorf("configure recall projection: %w", err)
	}
	if _, err := index.ExecContext(ctx, `CREATE VIRTUAL TABLE recall USING fts5(content, tool_name, tool_calls, session_id UNINDEXED, message_id UNINDEXED, role UNINDEXED, started_at UNINDEXED)`); err != nil {
		return nil, fmt.Errorf("initialize recall projection: %w", err)
	}
	if err := s.populateRecall(ctx, index, owner, filter, excluded); err != nil {
		return nil, err
	}
	// Temporal preference gently biases relevance instead of replacing it with
	// chronological ordering. Relevance and message id break deterministic ties.
	order := "bm25(recall), message_id"
	if filter.Sort == "newest" {
		order = "bm25(recall) * (1.0 + 0.15 / (1.0 + max(0, ? - started_at) / 86400.0)), bm25(recall), message_id"
	} else if filter.Sort == "oldest" {
		order = "bm25(recall) * (1.0 + 0.15 * max(0, ? - started_at) / (86400.0 + max(0, ? - started_at))), bm25(recall), message_id"
	}
	args := []any{filter.Query}
	now := float64(s.now().UnixNano()) / 1e9
	if filter.Sort == "newest" || filter.Sort == "oldest" {
		args = append(args, now)
		if filter.Sort == "oldest" {
			args = append(args, now)
		}
	}
	rows, err := index.QueryContext(ctx, `SELECT session_id, message_id, role, snippet(recall,0,'[',']','…',24) FROM recall WHERE recall MATCH ? ORDER BY `+order, args...)
	if err != nil {
		return nil, fmt.Errorf("query recall projection: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	results := make([]SearchSession, 0, filter.Limit)
	for rows.Next() {
		var id, role, snippet string
		var anchor int64
		if err := rows.Scan(&id, &anchor, &role, &snippet); err != nil {
			return nil, err
		}
		root, err := s.lineageRoot(ctx, owner, id)
		if err != nil {
			return nil, err
		}
		if root == "" || seen[root] || excluded[root] {
			continue
		}
		seen[root] = true
		summary, err := s.sessionSummary(ctx, owner, id)
		if err != nil {
			return nil, err
		}
		if summary == nil || summary.MessageCount == 0 {
			continue
		}
		summary.MatchMessageID, summary.MatchedRole, summary.Snippet = anchor, role, snippet
		results = append(results, *summary)
		if len(results) == filter.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *ProfileStore) populateRecall(ctx context.Context, index *sql.DB, owner string, filter SearchFilter, excluded map[string]bool) error {
	roots, err := s.searchLineageRoots(ctx, owner)
	if err != nil {
		return err
	}
	roles := filter.Roles
	if roles == nil {
		roles = map[string]bool{"user": true, "assistant": true}
	}
	query := `SELECT m.session_id,m.id,m.role,COALESCE(m.content,''),COALESCE(m.tool_name,''),COALESCE(m.tool_calls,''),s.started_at,COALESCE(s.title,''),COALESCE(v.value,'')
FROM messages m JOIN sessions s ON s.id=m.session_id
LEFT JOIN state_meta v ON m.role='assistant' AND v.key=('oswald:v1:turn:'||m.session_id||':'||m.id)
WHERE ` + searchEligible + ` AND s.source IN ('discord','imessage')`
	args := []any{owner}
	if filter.After != nil {
		query += " AND s.started_at>=?"
		args = append(args, float64(filter.After.Unix())+float64(filter.After.Nanosecond())/1e9)
	}
	if filter.Before != nil {
		query += " AND s.started_at<?"
		args = append(args, float64(filter.Before.Unix())+float64(filter.Before.Nanosecond())/1e9)
	}
	query += " ORDER BY m.session_id,m.id"
	rows, err := s.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	tx, err := index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO recall(content,tool_name,tool_calls,session_id,message_id,role,started_at) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	previous, skip := "", false
	for rows.Next() {
		var id, role, content, tool, calls, title, encoded string
		var messageID int64
		var started float64
		if err := rows.Scan(&id, &messageID, &role, &content, &tool, &calls, &started, &title, &encoded); err != nil {
			return err
		}
		if id != previous {
			root := roots[id]
			previous, skip = id, root == "" || excluded[root]
			if !skip && title != "" && (roles["user"] || roles["assistant"]) {
				if _, err := insert.ExecContext(ctx, title, "", "", id, messageID, "title", started); err != nil {
					return err
				}
			}
		}
		if skip {
			continue
		}
		if roles[role] {
			if role == "tool" {
				content = truncateRunes(content, 8192)
			}
			if _, err := insert.ExecContext(ctx, content, tool, calls, id, messageID, role, started); err != nil {
				return err
			}
		}
		if roles["tool"] && encoded != "" {
			history, err := searchExchangeHistory(encoded)
			if err != nil {
				return err
			}
			projection := ToolHistorySearchText(history)
			if projection != "" {
				var names []string
				for _, batch := range history.Batches {
					for _, call := range batch.Calls {
						names = append(names, call.Name)
					}
				}
				if _, err := insert.ExecContext(ctx, projection, strings.Join(names, "\n"), "", id, messageID, "tool", started); err != nil {
					return err
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// searchLineageRoots resolves lineage from one read so discovery never performs
// a nested database query while the single profile connection has open rows.
func (s *ProfileStore) searchLineageRoots(ctx context.Context, owner string) (map[string]string, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT id,COALESCE(parent_session_id,'') FROM sessions WHERE profile_name=?`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parents := make(map[string]string)
	for rows.Next() {
		var id, parent string
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		parents[id] = parent
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	roots := make(map[string]string, len(parents))
	for id := range parents {
		current := id
		for depth := 0; depth < 16; depth++ {
			parent, ok := parents[current]
			if !ok {
				current = ""
				break
			}
			if parent == "" || parent == current {
				break
			}
			current = parent
		}
		roots[id] = current
	}
	return roots, nil
}

func searchExchangeHistory(encoded string) (ToolHistory, error) {
	state, err := decodeProfileExchange(encoded)
	if err != nil {
		return ToolHistory{}, err
	}
	if state.Delivery != "delivered" {
		return EmptyToolHistory(), nil
	}
	return DecodeToolHistory(state.History)
}
