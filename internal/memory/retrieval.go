package memory

import (
	"math"
	"sort"
)

// RelevantTurns returns selected turns from retained history.
// Store-level TTL and max-turn pruning still happen before selection.
func (s *Store) RelevantTurns(sessionKey string, queryEmbedding []float64, opts RetrievalOptions) RetrievalResult {
	turns := s.Turns(sessionKey)
	result := RetrievalResult{
		CandidateTurnCount: len(turns),
		Details:            make([]RetrievalDetail, len(turns)),
	}
	if len(turns) == 0 {
		return result
	}

	if opts.RecentTurns < 0 {
		opts.RecentTurns = 0
	}
	if opts.MaxRelevantTurns < 0 {
		opts.MaxRelevantTurns = 0
	}

	recentStart := len(turns) - opts.RecentTurns
	if recentStart < 0 {
		recentStart = 0
	}
	selected := make(map[int]struct{}, len(turns))
	for i, turn := range turns {
		result.Details[i] = RetrievalDetail{
			Index:          i,
			CreatedAt:      turn.CreatedAt,
			UserChars:      len(turn.User.Content),
			AssistantChars: len(turn.Assistant.Content),
		}
	}
	if opts.IncludeRecent {
		for i := recentStart; i < len(turns); i++ {
			selected[i] = struct{}{}
			result.Details[i].Included = true
			result.Details[i].Reason = "recent"
		}
		result.RecentTurnCount = len(turns) - recentStart
	}

	if len(queryEmbedding) > 0 && opts.MaxRelevantTurns > 0 {
		scores := make([]scoredTurn, 0, len(turns))
		for i := range turns {
			score, ok := cosineSimilarity(queryEmbedding, turns[i].Embedding)
			if ok {
				result.Details[i].Similarity = score
				result.Details[i].HasSimilarity = true
			}
			if _, alreadySelected := selected[i]; alreadySelected {
				continue
			}
			if !ok || score < opts.MinSimilarity {
				continue
			}
			scores = append(scores, scoredTurn{index: i, score: score})
		}
		sort.Slice(scores, func(i, j int) bool {
			if scores[i].score == scores[j].score {
				return scores[i].index > scores[j].index
			}
			return scores[i].score > scores[j].score
		})
		if len(scores) > opts.MaxRelevantTurns {
			scores = scores[:opts.MaxRelevantTurns]
		}
		for _, scored := range scores {
			selected[scored.index] = struct{}{}
			result.Details[scored.index].Included = true
			result.Details[scored.index].Reason = "semantic"
			result.SemanticTurnCount++
		}
	}

	result.Turns = make([]Turn, 0, len(selected))
	for i, turn := range turns {
		if _, ok := selected[i]; ok {
			result.Turns = append(result.Turns, turn)
		}
	}
	return result
}

type scoredTurn struct {
	index int
	score float64
}

func cosineSimilarity(a []float64, b []float64) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}

	var dot float64
	var normA float64
	var normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), true
}
