package asrpilot

// Pair is one column of a global alignment of two token sequences: A and B
// index the two sequences, -1 for a gap on that side.
type Pair struct{ A, B int }

// Alignment scores (Needleman-Wunsch). An exact match is worth more than a
// gap pair, so two agreeing words are always aligned with each other; a
// near match (similarity ≥ nearMatchSim, e.g. "colour"/"color") is preferred
// to a plain substitution but is never an anchor.
const (
	scoreMatch    = 3
	scoreNear     = 1
	scoreMismatch = -1
	scoreGap      = -2
	nearMatchSim  = 0.8
)

// MaxAlignTokens caps each side of Align; the vote's windows are a few
// seconds to ~40 s of speech, far below it.
const MaxAlignTokens = 2000

// Align globally aligns a and b (Needleman-Wunsch, linear gap cost) and
// returns the alignment columns in order. Ties prefer a diagonal step, then a
// gap in b, so the result is deterministic. Each side is capped at
// MaxAlignTokens (callers check; Align returns nil past it).
func Align(a, b []string) []Pair {
	n, m := len(a), len(b)
	if n > MaxAlignTokens || m > MaxAlignTokens {
		return nil
	}
	w := m + 1
	score := make([]int, (n+1)*w)
	for i := 1; i <= n; i++ {
		score[i*w] = i * scoreGap
	}
	for j := 1; j <= m; j++ {
		score[j] = j * scoreGap
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			best := score[(i-1)*w+j-1] + pairScore(a[i-1], b[j-1])
			if s := score[(i-1)*w+j] + scoreGap; s > best {
				best = s
			}
			if s := score[i*w+j-1] + scoreGap; s > best {
				best = s
			}
			score[i*w+j] = best
		}
	}
	out := make([]Pair, 0, n+m)
	for i, j := n, m; i > 0 || j > 0; {
		switch {
		case i > 0 && j > 0 && score[i*w+j] == score[(i-1)*w+j-1]+pairScore(a[i-1], b[j-1]):
			out = append(out, Pair{i - 1, j - 1})
			i, j = i-1, j-1
		case i > 0 && score[i*w+j] == score[(i-1)*w+j]+scoreGap:
			out = append(out, Pair{i - 1, -1})
			i--
		default:
			out = append(out, Pair{-1, j - 1})
			j--
		}
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

func pairScore(x, y string) int {
	switch {
	case x == y:
		return scoreMatch
	case Similarity(x, y) >= nearMatchSim:
		return scoreNear
	default:
		return scoreMismatch
	}
}

// Similarity is 1 − Levenshtein(a, b) / max(len(a), len(b)) over runes; 1 for
// two empty strings.
func Similarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	mx := max(len(ra), len(rb))
	if mx == 0 {
		return 1
	}
	return 1 - float64(levenshtein(ra, rb))/float64(mx)
}

func levenshtein[T comparable](a, b []T) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := prev[j-1]
			if a[i-1] != b[j-1] {
				c = 1 + min(prev[j-1], prev[j], cur[j-1])
			}
			cur[j] = c
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// WordErrors is the word-level edit distance (substitutions + insertions +
// deletions) turning ref into hyp — the numerator of a word error rate with
// ref as the reference. Linear memory, so a whole track compares in place.
func WordErrors(ref, hyp []string) int {
	// Interned to ints: a track is tens of thousands of words a side.
	ids := make(map[string]int, 1024)
	intern := func(ws []string) []int {
		out := make([]int, len(ws))
		for i, w := range ws {
			id, ok := ids[w]
			if !ok {
				id = len(ids)
				ids[w] = id
			}
			out[i] = id
		}
		return out
	}
	return levenshtein(intern(ref), intern(hyp))
}
