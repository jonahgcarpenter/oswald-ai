package memory

import (
	"strings"
	"unicode"
)

const documentSearchExcerptRunes = 1500

type documentSearchToken struct {
	text       string
	start, end int // Rune offsets in the original source.
}

func documentSearchTokens(text []rune) []documentSearchToken {
	var tokens []documentSearchToken
	for i := 0; i < len(text); {
		if !unicode.IsLetter(text[i]) && !unicode.IsNumber(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && (unicode.IsLetter(text[i]) || unicode.IsNumber(text[i]) || unicode.IsMark(text[i])) {
			i++
		}
		tokens = append(tokens, documentSearchToken{strings.ToLower(string(text[start:i])), start, i})
	}
	return tokens
}

func documentSearchTerms(query string) []string {
	tokens := documentSearchTokens([]rune(query))
	if len(tokens) == 0 {
		// Retain literal punctuation searches, without treating SQL wildcards as syntax.
		return []string{query}
	}
	const stopwords = " a an and are as at be been being by can could did do does for from had has have how i in is it its me my of on or our please should that the their them there these they this those to us was we were what when where which who why will with would you your "
	var terms []string
	seen := make(map[string]bool)
	for _, token := range tokens {
		if seen[token.text] || strings.Contains(stopwords, " "+token.text+" ") {
			continue
		}
		seen[token.text] = true
		terms = append(terms, token.text)
		if len(terms) == 16 {
			break
		}
	}
	return terms
}

func documentSearchPredicate(terms []string) (string, []any) {
	clauses := make([]string, 0, len(terms)+1)
	patterns := make([]any, 0, len(terms))
	escape := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for _, term := range terms {
		clauses = append(clauses, `c.text LIKE ? ESCAPE '\'`)
		patterns = append(patterns, "%"+escape.Replace(term)+"%")
	}
	// SQLite LIKE folds ASCII only. Include non-ASCII chunks for Go's Unicode
	// matching rather than silently excluding differently cased Unicode terms.
	clauses = append(clauses, `c.text GLOB '*[^ -~]*'`)
	return strings.Join(clauses, " OR "), patterns
}

func documentSearchMatch(text string, terms []string) (int, string) {
	runes := []rune(text)
	tokens := documentSearchTokens(runes)
	type match struct {
		start, end, term int
		phrase           bool
	}
	var matches []match
	seen := make([]bool, len(terms))
	distinct, phraseBoost := 0, 0
	for i, token := range tokens {
		for term, value := range terms {
			if token.text != value {
				continue
			}
			if !seen[term] {
				seen[term] = true
				distinct++
			}
			phrase := len(terms) > 1 && i+1 >= len(terms)
			if phrase {
				for j, value := range terms {
					if tokens[i+1-len(terms)+j].text != value {
						phrase = false
						break
					}
				}
			}
			if phrase {
				phraseBoost = 10
			}
			matches = append(matches, match{token.start, token.end, term, phrase})
			break
		}
	}
	if len(matches) == 0 && len(terms) == 1 && len(documentSearchTokens([]rune(terms[0]))) == 0 {
		if at := strings.Index(text, terms[0]); at >= 0 {
			start := len([]rune(text[:at]))
			matches = append(matches, match{start: start, end: start + len([]rune(terms[0]))})
			distinct = 1
		}
	}
	if len(matches) == 0 {
		return 0, ""
	}
	// Sliding windows maximize distinct matching terms, not repetition. Keep the
	// earliest equally good window and reserve context on both sides where possible.
	counts := make([]int, len(terms))
	left, unique, phrases, best, start, end := 0, 0, 0, -1, 0, 0
	for right, m := range matches {
		counts[m.term]++
		if counts[m.term] == 1 {
			unique++
		}
		if m.phrase {
			phrases++
		}
		for left < right && m.end-matches[left].start > documentSearchExcerptRunes {
			old := matches[left]
			counts[old.term]--
			if counts[old.term] == 0 {
				unique--
			}
			if old.phrase {
				phrases--
			}
			left++
		}
		score := unique * 100
		if phrases > 0 {
			score += 10
		}
		if score > best {
			best, start, end = score, matches[left].start, m.end
		}
	}
	start = max(0, start-(documentSearchExcerptRunes-(end-start))/2)
	end = min(len(runes), start+documentSearchExcerptRunes)
	start = max(0, end-documentSearchExcerptRunes)
	return distinct*100 + phraseBoost, string(runes[start:end])
}
