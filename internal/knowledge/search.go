package knowledge

import (
	"math"
	"sort"
	"strings"
)

// BM25 parameters — the standard defaults. k1 controls how quickly term
// frequency saturates, b how strongly length normalises.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Lexical search rather than embeddings, deliberately. It needs no model, no
// vector store and no index to rebuild, it runs over a handful of documents in
// microseconds, and compliance retrieval is unusually keyword-friendly: people
// search for "AC-2", "segmentation", "72 hours". Embeddings can go behind this
// same function later if recall turns out to be the limit; starting there would
// be an infrastructure bet made before there is anything to measure.

// Hit is one scored chunk.
type Hit struct {
	Chunk Chunk    `json:"chunk"`
	Score float64  `json:"score"`
	Terms []string `json:"matched_terms,omitempty"`
}

// stopwords are dropped from queries. The list is short on purpose: dropping
// domain words like "security" or "policy" would gut the signal in a library
// where every document is about them.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "was": true, "were": true, "will": true, "with": true,
	"must": true, "shall": true, "should": true, "what": true, "which": true,
	"how": true, "many": true, "do": true, "does": true, "we": true, "our": true,
}

// Search ranks chunks against a query, best first.
func Search(query string, chunks []Chunk, limit int) []Hit {
	terms := Tokenize(query)
	if len(terms) == 0 || len(chunks) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 6
	}

	wanted := make(map[string]bool, len(terms))
	for _, t := range terms {
		wanted[t] = true
	}

	docFreq := make(map[string]int, len(terms))
	tokenized := make([][]string, len(chunks))
	totalLen := 0
	for i, chunk := range chunks {
		// The heading is scored with the body: a section headed "Incident
		// reporting" whose body says "notify the authority" is still the right
		// chunk for a query about incident reporting.
		tokens := Tokenize(chunk.Heading + " " + chunk.Title + " " + chunk.Body)
		tokenized[i] = tokens
		totalLen += len(tokens)

		seen := make(map[string]bool, len(tokens))
		for _, tok := range tokens {
			if seen[tok] || !wanted[tok] {
				continue
			}
			seen[tok] = true
			docFreq[tok]++
		}
	}
	if totalLen == 0 {
		return nil
	}

	avgLen := float64(totalLen) / float64(len(chunks))
	n := float64(len(chunks))

	hits := make([]Hit, 0, len(chunks))
	for i, chunk := range chunks {
		tokens := tokenized[i]
		freq := make(map[string]int, len(tokens))
		for _, tok := range tokens {
			freq[tok]++
		}

		var score float64
		var matched []string
		for _, term := range terms {
			tf := float64(freq[term])
			if tf == 0 {
				continue
			}
			matched = append(matched, term)
			df := float64(docFreq[term])
			// BM25's IDF with the +1 guard, so a term present in every chunk
			// scores near zero rather than negative.
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			norm := tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*float64(len(tokens))/avgLen))
			score += idf * norm
		}
		if score <= 0 {
			continue
		}
		sort.Strings(matched)
		hits = append(hits, Hit{Chunk: chunk, Score: score, Terms: matched})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		// Ties break on document order, so repeating a question gives the same
		// answer.
		if hits[i].Chunk.DocumentID != hits[j].Chunk.DocumentID {
			return hits[i].Chunk.DocumentID < hits[j].Chunk.DocumentID
		}
		return hits[i].Chunk.Ordinal < hits[j].Chunk.Ordinal
	})

	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Tokenize lowercases and splits on anything that is not a letter, digit or
// hyphen, keeping hyphenated identifiers ("AC-2", "multi-factor") whole because
// they are exactly what people search for.
func Tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, "-")
		if len(f) < 2 || stopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}
