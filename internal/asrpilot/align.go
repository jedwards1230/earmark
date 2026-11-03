package asrpilot

// Pair is one column of an alignment of two token sequences: A and B index
// the two sequences, -1 for a gap on that side.
type Pair struct{ A, B int }

// Alignment bounds. Both pilot models emit window-level text (no word
// times), and a Granite window is up to 540 s — ~1500 words a side — so the
// alignment is a Myers O((N+M)·D) diff, D the number of differing tokens,
// not an N·M table. Its trace keeps O(D²) ints.
const (
	// MaxAlignTokens caps each side; a window past it is unmappable.
	MaxAlignTokens = 5000
	// MaxAlignEdits caps D: two texts that differ in more tokens than this
	// share too little to anchor a span (unmappable, ~18 MB of trace at the
	// cap).
	MaxAlignEdits = 1500
)

// Align aligns a and b: the exact matches of a shortest edit script
// (Myers' diff, i.e. a longest common subsequence), with each unmatched run
// between two matches paired position by position as substitutions and the
// longer side's remainder as gaps (before the substitutions in the leading
// run, after them elsewhere). Columns come in order. nil when either
// side exceeds MaxAlignTokens or the texts differ in more than
// MaxAlignEdits tokens.
func Align(a, b []string) []Pair {
	if len(a) > MaxAlignTokens || len(b) > MaxAlignTokens {
		return nil
	}
	matches, ok := diffMatches(a, b, MaxAlignEdits)
	if !ok {
		return nil
	}
	out := make([]Pair, 0, max(len(a), len(b)))
	pi, pj := 0, 0
	gapRun := func(i1, j1 int) {
		if pi == 0 && pj == 0 {
			// The leading run has no match on its left: align it to the
			// right, so its tokens sit next to the first agreed word.
			for d := (i1 - pi) - (j1 - pj); d > 0; d-- {
				out = append(out, Pair{pi, -1})
				pi++
			}
			for d := (j1 - pj) - (i1 - pi); d > 0; d-- {
				out = append(out, Pair{-1, pj})
				pj++
			}
		}
		for ; pi < i1 && pj < j1; pi, pj = pi+1, pj+1 {
			out = append(out, Pair{pi, pj})
		}
		for ; pi < i1; pi++ {
			out = append(out, Pair{pi, -1})
		}
		for ; pj < j1; pj++ {
			out = append(out, Pair{-1, pj})
		}
	}
	for _, mt := range matches {
		gapRun(mt[0], mt[1])
		out = append(out, Pair{mt[0], mt[1]})
		pi, pj = mt[0]+1, mt[1]+1
	}
	gapRun(len(a), len(b))
	return out
}

// diffMatches returns the matched (i, j) pairs of a shortest edit script
// from a to b (Myers 1986, greedy forward pass with a trace), ascending. ok is
// false when more than maxD edits are needed.
func diffMatches(a, b []string, maxD int) (matches [][2]int, ok bool) {
	n, m := len(a), len(b)
	maxD = min(maxD, n+m)
	off := maxD + 1
	v := make([]int, 2*maxD+3) // v[off+k]: furthest x on diagonal k
	var trace [][]int          // trace[d]: v[-d..d] before step d
	found := -1
	for d := 0; d <= maxD && found < 0; d++ {
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = d
				break
			}
		}
	}
	if found < 0 {
		return nil, false
	}
	x, y := n, m
	for d := found; d > 0; d-- {
		prev := trace[d] // v of step d-1, indexed k+d
		k := x - y
		var pk int
		if k == -d || (k != d && prev[k-1+d] < prev[k+1+d]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := prev[pk+d]
		py := px - pk
		for x > px && y > py {
			x, y = x-1, y-1
			matches = append(matches, [2]int{x, y})
		}
		x, y = px, py
	}
	for x > 0 && y > 0 { // d = 0: the common prefix
		x, y = x-1, y-1
		matches = append(matches, [2]int{x, y})
	}
	for l, r := 0, len(matches)-1; l < r; l, r = l+1, r-1 {
		matches[l], matches[r] = matches[r], matches[l]
	}
	return matches, true
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

// levBuf is reusable row storage for levenshteinBuf.
type levBuf struct{ prev, cur []int }

func levenshtein[T comparable](a, b []T) int {
	return levenshteinBuf(a, b, &levBuf{})
}

// levenshteinBuf is the edit distance of a and b in two rows, kept in buf
// across calls.
func levenshteinBuf[T comparable](a, b []T, buf *levBuf) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	if cap(buf.prev) < len(b)+1 {
		buf.prev, buf.cur = make([]int, len(b)+1), make([]int, len(b)+1)
	}
	prev, cur := buf.prev[:len(b)+1], buf.cur[:len(b)+1]
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
