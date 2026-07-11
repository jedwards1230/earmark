package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// segWithWords is a fixture: one segment carrying per-word tokens (with a score
// and a speaker on one word, both nil on another) so the mapping of every
// db.Word field is exercised.
func segWithWords() db.Segment {
	score := 0.97
	spk := "SPEAKER_00"
	return db.Segment{
		ID: 0, Start: 0, End: 2, Text: "hello world", Speaker: &spk,
		Words: []db.Word{
			{Word: "hello", Start: 0.0, End: 0.8, Score: &score, Speaker: &spk},
			{Word: "world", Start: 0.9, End: 2.0},
		},
	}
}

func trackWithWords() *db.TrackDetail {
	return &db.TrackDetail{
		ID: "job-w", FilePath: "/books/x/y/z.m4b", Status: "done",
		HasTranscript: true, Language: "en", ModelName: "parakeet", DurationSeconds: 2,
		Segments: []db.Segment{segWithWords()},
	}
}

// TestGetTranscriptDefaultOmitsWords asserts the default response (no
// includeWordTimestamps param) omits the `words` field entirely — the structured
// payload marshals byte-identically to the pre-word-timestamp shape. This is the
// back-compat guard.
func TestGetTranscriptDefaultOmitsWords(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-w").Return(trackWithWords(), nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-w",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	out, ok := res.StructuredContent.(TranscriptOutput)
	require.True(t, ok, "structuredContent should be a TranscriptOutput, got %T", res.StructuredContent)
	require.Len(t, out.Segments, 1)
	assert.Nil(t, out.Segments[0].Words, "default response must not carry word timestamps")

	// Byte-identical proof: the marshalled segment has no `words` key at all.
	raw, err := json.Marshal(out.Segments[0])
	require.NoError(t, err)
	assert.JSONEq(t, `{"start":0,"end":2,"text":"hello world"}`, string(raw))
	assert.NotContains(t, string(raw), "words")

	mockDB.AssertExpectations(t)
}

// TestGetTranscriptIncludeWords asserts that with includeWordTimestamps=true each
// segment carries its words[] with the right shape (word/start/end, plus
// score/speaker when the backend supplies them).
func TestGetTranscriptIncludeWords(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-w").Return(trackWithWords(), nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID":               "job-w",
		"includeWordTimestamps": true,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	out, ok := res.StructuredContent.(TranscriptOutput)
	require.True(t, ok, "structuredContent should be a TranscriptOutput, got %T", res.StructuredContent)
	require.Len(t, out.Segments, 1)
	require.Len(t, out.Segments[0].Words, 2, "both word tokens should be present")

	w0 := out.Segments[0].Words[0]
	assert.Equal(t, "hello", w0.Word)
	assert.InDelta(t, 0.0, w0.Start, 1e-9)
	assert.InDelta(t, 0.8, w0.End, 1e-9)
	require.NotNil(t, w0.Score)
	assert.InDelta(t, 0.97, *w0.Score, 1e-9)
	require.NotNil(t, w0.Speaker)
	assert.Equal(t, "SPEAKER_00", *w0.Speaker)

	w1 := out.Segments[0].Words[1]
	assert.Equal(t, "world", w1.Word)
	assert.Nil(t, w1.Score, "score is nil when the backend omits it")
	assert.Nil(t, w1.Speaker, "speaker is nil when the backend omits it")

	// The human-readable text fallback is unchanged by the flag (still per-segment).
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "hello world")

	mockDB.AssertExpectations(t)
}

// TestTranscriptWordsMapper covers the pure mapper: empty/nil input yields nil
// (so the `words` field is omitted, not an empty array).
func TestTranscriptWordsMapper(t *testing.T) {
	assert.Nil(t, transcriptWords(nil))
	assert.Nil(t, transcriptWords([]db.Word{}))

	got := transcriptWords([]db.Word{{Word: "x", Start: 1, End: 2}})
	require.Len(t, got, 1)
	assert.Equal(t, "x", got[0].Word)
	assert.Nil(t, got[0].Score)
}
