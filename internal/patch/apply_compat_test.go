package patch

// Apply is a TEST-ONLY shim over the production replay path.
//
// The single-patch Apply() used to be production code (anchor.go). It was
// deleted when corrections became a replayable overlay: chunks are a derived
// projection, so nothing in the service rewrites one chunk's stored text any
// more. What must NOT be lost with it is anchor_live_test.go — those two tests
// are verbatim captures of what real judge models emitted (including a
// correct finding carrying a WRONG offset), and they are the regression guard
// on Locate's resolution ladder.
//
// Rather than rewrite those fixtures against the new API — which would mean
// editing the very assertions that encode the observed model behaviour — this
// shim keeps the old signature and routes it through ApplyCorrections. The
// live tests therefore still exercise the REAL production code path
// (resolvePatch → Locate → splice), unmodified.
//
// It lives in a _test.go file on purpose: it must never be reachable from the
// service.
func Apply(chunkText, wantHash string, a Anchor, correction string) (string, Span, error) {
	out, err := ApplyCorrections(chunkText, []Patch{{
		ID:         "compat",
		Anchor:     a,
		Correction: correction,
		ChunkHash:  wantHash,
	}})
	if err != nil {
		return "", Span{}, err
	}
	span, err := Locate(chunkText, a)
	if err != nil {
		return "", Span{}, err
	}
	return out, span, nil
}
