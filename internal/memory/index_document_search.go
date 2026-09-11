package memory

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// SearchUserDocuments combines authoritative canonical lexical matches with
// optional derived FTS/vector channels. Every result is rechecked against the
// current private library, including when either index is stale or unavailable.
func (s *Store) SearchUserDocuments(ctx context.Context, userID, query, documentID string, limit int) (out []DocumentSearchResult, err error) {
	started := time.Now()
	defer func() {
		s.documentMeasured(ctx, "document_search", started, &err, config.F("returned_count", len(out)))
	}()
	lexical, err := s.searchUserDocumentsLexical(ctx, userID, query, documentID, 10)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	type key struct {
		document string
		ordinal  int
	}
	scores := make(map[key]float64)
	for i, r := range lexical {
		scores[key{r.Document.ID, r.Chunk.Ordinal}] += 1 / float64(60+i+1)
	}
	terms := documentSearchTerms(strings.TrimSpace(query))
	for _, kind := range []string{IndexKindUserDocumentFTS, IndexKindUserDocumentVector} {
		if kind == IndexKindUserDocumentVector && (s.embedder == nil || s.embedModel == "") {
			continue
		}
		revision, e := s.LiveIndexRevision(ctx, kind)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e == nil {
			e = validateRevisionTableIdentity(revision)
		}
		if e != nil {
			s.documentIndexSearchWarning(ctx, e)
			continue
		}
		var rows *sql.Rows
		if kind == IndexKindUserDocumentFTS {
			if len(terms) == 0 {
				continue
			}
			quoted := make([]string, len(terms))
			for i, term := range terms {
				quoted[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
			}
			rows, e = s.sql.QueryContext(ctx, `SELECT c.document_id,c.ordinal`+documentIndexFrom+`JOIN `+revision.TableName+` idx ON idx.rowid=key.id AND idx.canonical_user_id=d.canonical_user_id AND idx.text=c.text WHERE `+documentIndexEligible+` AND d.canonical_user_id=? AND (?='' OR d.id=?) AND `+revision.TableName+` MATCH ? ORDER BY bm25(`+revision.TableName+`),key.id LIMIT 10`, time.Now().UnixMilli(), userID, documentID, documentID, strings.Join(quoted, " OR "))
		} else {
			// Query the live model, not a replacement still being built.
			var vector []float64
			vector, e = s.embedWithModel(ctx, revision.Model, query)
			if e == nil && len(vector) != revision.Dimension {
				e = ErrDerivedIndexDegraded
			}
			if e != nil {
				s.documentIndexSearchWarning(ctx, e)
				continue
			}
			var data []byte
			data, e = serializeVector(vector)
			if e != nil {
				s.documentIndexSearchWarning(ctx, e)
				continue
			}
			// Exact distance over a quota-bounded private library avoids a global
			// top-k pool starving a tenant or an explicitly selected document.
			rows, e = s.sql.QueryContext(ctx, `SELECT c.document_id,c.ordinal`+documentIndexFrom+`JOIN `+revision.TableName+` idx ON idx.rowid=key.id AND idx.canonical_user_id=d.canonical_user_id AND idx.canonical_version=c.text WHERE `+documentIndexEligible+` AND d.canonical_user_id=? AND (?='' OR d.id=?) AND idx.embedding_model=? ORDER BY vec_distance_L2(idx.embedding,?),key.id LIMIT 10`, time.Now().UnixMilli(), userID, documentID, documentID, revision.Model, data)
		}
		if e != nil {
			s.documentIndexSearchWarning(ctx, e)
			continue
		}
		var channel []key
		for rows.Next() {
			var k key
			if e = rows.Scan(&k.document, &k.ordinal); e != nil {
				break
			}
			channel = append(channel, k)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			s.documentIndexSearchWarning(ctx, e)
			continue
		}
		weight := 1.0
		if kind == IndexKindUserDocumentFTS {
			weight = 0.25
		}
		for i, k := range channel {
			scores[k] += weight / float64(60+i+1)
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	keys := make([]key, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if scores[keys[i]] != scores[keys[j]] {
			return scores[keys[i]] > scores[keys[j]]
		}
		if keys[i].document != keys[j].document {
			return keys[i].document < keys[j].document
		}
		return keys[i].ordinal < keys[j].ordinal
	})
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, DocumentScope{UserID: userID}); err != nil {
		return nil, err
	}
	for _, k := range keys {
		var r DocumentSearchResult
		r.Document, err = scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM user_documents d WHERE d.id=? AND d.canonical_user_id=? AND d.status IN ('ready','partial') AND d.expires_at>?`, k.document, userID, time.Now().UnixMilli()))
		if errors.Is(err, ErrDocumentNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = tx.QueryRowContext(ctx, `SELECT ordinal,text,locator,method FROM user_document_chunks WHERE document_id=? AND ordinal=?`, k.document, k.ordinal).Scan(&r.Chunk.Ordinal, &r.Chunk.Text, &r.Chunk.Locator, &r.Chunk.Method)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		_, excerpt := documentSearchMatch(r.Chunk.Text, terms)
		if excerpt == "" {
			runes := []rune(r.Chunk.Text)
			excerpt = string(runes[:min(len(runes), documentSearchExcerptRunes)])
		}
		r.Chunk.Text = excerpt
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *Store) documentIndexSearchWarning(ctx context.Context, err error) {
	if s.log != nil && ctx.Err() == nil {
		s.log.Server("memory").With(requestctx.LogFields(ctx)...).Warn("user_document.search.index_degraded", "document search index unavailable", config.F("status", "degraded"), config.ErrorField(err))
	}
}
