package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChunkByToken(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		chunkSize int
		want      []string
	}{
		{
			name:      "simple sentence with chunk size 3",
			content:   "The quick brown fox jumps",
			chunkSize: 3,
			want: []string{
				"The quick brown",
				"fox jumps",
			},
		},
		{
			name:      "longer text with chunk size 5",
			content:   "The quick brown fox jumps over the lazy dog in the garden",
			chunkSize: 5,
			want: []string{
				"The quick brown fox jumps",
				"over the lazy dog in",
				"the garden",
			},
		},
		{
			name:      "single word with small chunk size",
			content:   "hello",
			chunkSize: 2,
			want:      []string{"hello"},
		},
		{
			name:      "empty string",
			content:   "",
			chunkSize: 3,
			want:      []string{""},
		},
		{
			name:      "multiple sentences with chunk size 8",
			content:   "The cat sat on the mat. The dog ran in the park. The bird flew over the trees.",
			chunkSize: 8,
			want: []string{
				"The cat sat on the mat. The",
				"dog ran in the park. The bird",
				"flew over the trees.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Chunker(tt.content, tt.chunkSize, SplitTypeToken)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestChunkByChar(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		chunkSize int
		want      []string
	}{
		{
			name:      "basic three char chunks",
			content:   "hello",
			chunkSize: 3,
			want:      []string{"hel", "lo"},
		},
		{
			name:      "empty string",
			content:   "",
			chunkSize: 2,
			want:      []string{},
		},
		{
			name:      "unicode characters",
			content:   "你好世界",
			chunkSize: 2,
			want:      []string{"你好", "世界"},
		},
		{
			name:      "chunk size larger than content",
			content:   "test",
			chunkSize: 10,
			want:      []string{"test"},
		},
		{
			name:      "multiple sentences with chunk size 20",
			content:   "The sun was bright. The wind was cool. The grass was green.",
			chunkSize: 20,
			want: []string{
				"The sun was bright. ",
				"The wind was cool. T",
				"he grass was green.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Chunker(tt.content, tt.chunkSize, SplitTypeChar)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestChunkByWord(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		chunkSize int
		want      []string
	}{
		{
			name:      "two words per chunk",
			content:   "the quick brown fox jumps",
			chunkSize: 2,
			want:      []string{"the quick", "brown fox", "jumps"},
		},
		{
			name:      "empty string",
			content:   "",
			chunkSize: 2,
			want:      []string{},
		},
		{
			name:      "single word",
			content:   "hello",
			chunkSize: 3,
			want:      []string{"hello"},
		},
		{
			name:      "multiple spaces between words",
			content:   "hello    world   test",
			chunkSize: 2,
			want:      []string{"hello world", "test"},
		},
		{
			name:      "chunk size larger than word count",
			content:   "the quick brown",
			chunkSize: 5,
			want:      []string{"the quick brown"},
		},
		{
			name:      "multiple sentences with chunk size 4",
			content:   "I like red apples. My friend prefers green ones. The store has both.",
			chunkSize: 4,
			want: []string{
				"I like red apples",
				"My friend prefers green",
				"ones The store has",
				"both",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Chunker(tt.content, tt.chunkSize, SplitTypeWord)
			assert.Equal(t, tt.want, got)
		})
	}
}
