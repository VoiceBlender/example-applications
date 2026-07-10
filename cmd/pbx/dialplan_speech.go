package main

import "strings"

// matchSpeech matches a spoken transcript against a gather node's option
// keywords (its wired output ports). It tries, in order:
//  1. whole-phrase substring (handles multi-word options like "customer service")
//  2. exact word, or number-word ("one" → "1", "for" → "4")
//  3. fuzzy / close-word via edit distance ("sails" → "sales")
//
// Returns the matched option and true on a hit.
func matchSpeech(transcript string, options []string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(transcript))
	if t == "" || len(options) == 0 {
		return "", false
	}
	words := tokenizeWords(t)

	// 1) phrase substring
	for _, opt := range options {
		o := strings.ToLower(strings.TrimSpace(opt))
		if o != "" && strings.Contains(t, o) {
			return opt, true
		}
	}
	// 2) exact word / number-word
	for _, opt := range options {
		o := strings.ToLower(strings.TrimSpace(opt))
		for _, w := range words {
			if w == o || numberWords[w] == o {
				return opt, true
			}
		}
	}
	// 3) fuzzy (close word) — pick the smallest edit distance within tolerance
	best, bestDist := "", 1<<30
	for _, opt := range options {
		o := strings.ToLower(strings.TrimSpace(opt))
		if o == "" {
			continue
		}
		tol := fuzzyTolerance(o)
		for _, w := range words {
			if d := levenshtein(w, o); d <= tol && d < bestDist {
				best, bestDist = opt, d
			}
		}
	}
	if best != "" {
		return best, true
	}
	return "", false
}

// fuzzyTolerance is the max edit distance allowed for a close-word match —
// about 40% of the word length, capped at 3. Words of 3 chars or fewer (e.g.
// single digits) require an exact match to avoid "5"≈"6" / "1"≈"2" mistakes.
func fuzzyTolerance(word string) int {
	n := len(word)
	if n <= 3 {
		return 0
	}
	t := n * 2 / 5 // ~40%
	if t < 1 {
		t = 1
	}
	if t > 3 {
		t = 3
	}
	return t
}

// tokenizeWords splits a lower-cased string into alphanumeric words.
func tokenizeWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

var numberWords = map[string]string{
	"zero": "0", "oh": "0", "one": "1", "two": "2", "to": "2", "too": "2",
	"three": "3", "four": "4", "for": "4", "five": "5", "six": "6",
	"seven": "7", "eight": "8", "nine": "9",
}

// levenshtein is the edit distance between two strings.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
