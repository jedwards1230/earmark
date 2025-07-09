package tokenizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetTokens(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantErr  bool
		validate func(t *testing.T, tokens []int)
	}{
		{
			name:    "simple_text",
			content: "Hello, world!",
			wantErr: false,
			validate: func(t *testing.T, tokens []int) {
				assert.NotEmpty(t, tokens, "tokens should not be empty")
				assert.Greater(t, len(tokens), 0, "should have at least one token")
			},
		},
		{
			name:    "empty_string",
			content: "",
			wantErr: false,
			validate: func(t *testing.T, tokens []int) {
				assert.Empty(t, tokens, "empty string should produce no tokens")
			},
		},
		{
			name:    "single_word",
			content: "hello",
			wantErr: false,
			validate: func(t *testing.T, tokens []int) {
				assert.NotEmpty(t, tokens, "single word should produce tokens")
				assert.Greater(t, len(tokens), 0, "should have at least one token")
			},
		},
		{
			name:    "multiple_words",
			content: "This is a longer sentence with multiple words.",
			wantErr: false,
			validate: func(t *testing.T, tokens []int) {
				assert.NotEmpty(t, tokens, "multiple words should produce tokens")
				assert.Greater(t, len(tokens), 1, "should have multiple tokens")
			},
		},
		{
			name:    "special_characters",
			content: "!@#$%^&*()_+-=[]{}|;':\",./<>?",
			wantErr: false,
			validate: func(t *testing.T, tokens []int) {
				assert.NotEmpty(t, tokens, "special characters should produce tokens")
			},
		},
		{
			name:    "unicode_text",
			content: "Hello 世界! 🌍",
			wantErr: false,
			validate: func(t *testing.T, tokens []int) {
				assert.NotEmpty(t, tokens, "unicode text should produce tokens")
			},
		},
		{
			name:    "long_text",
			content: "This is a very long text that contains many words and should be tokenized into multiple tokens to test the tokenizer's ability to handle longer content properly.",
			wantErr: false,
			validate: func(t *testing.T, tokens []int) {
				assert.NotEmpty(t, tokens, "long text should produce tokens")
				assert.Greater(t, len(tokens), 10, "long text should produce many tokens")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, err := GetTokens(tt.content)

			if tt.wantErr {
				assert.Error(t, err, "expected error but got none")
				return
			}

			require.NoError(t, err, "unexpected error: %v", err)
			tt.validate(t, tokens)
		})
	}
}

func TestDecodeTokens(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []int
		wantErr bool
	}{
		{
			name:    "empty_tokens",
			tokens:  []int{},
			wantErr: false,
		},
		{
			name:    "nil_tokens",
			tokens:  nil,
			wantErr: false,
		},
		// Note: We can't easily test specific token values without knowing
		// the exact tokenization, so we'll test the round-trip functionality
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := DecodeTokens(tt.tokens)

			if tt.wantErr {
				assert.Error(t, err, "expected error but got none")
				return
			}

			require.NoError(t, err, "unexpected error: %v", err)

			// For empty/nil tokens, result should be empty
			if len(tt.tokens) == 0 {
				assert.Empty(t, result, "empty tokens should produce empty string")
			}
		})
	}
}

func TestTokenizeRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string // Some tokenizers may not preserve exact whitespace
	}{
		{
			name:     "simple_text",
			content:  "Hello, world!",
			expected: "Hello, world!",
		},
		{
			name:     "empty_string",
			content:  "",
			expected: "",
		},
		{
			name:     "single_word",
			content:  "hello",
			expected: "hello",
		},
		{
			name:     "unicode_text",
			content:  "Hello 世界",
			expected: "Hello 世界",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode the content to tokens
			tokens, err := GetTokens(tt.content)
			require.NoError(t, err, "failed to tokenize content: %v", err)

			// Decode the tokens back to string
			result, err := DecodeTokens(tokens)
			require.NoError(t, err, "failed to decode tokens: %v", err)

			// The result should match the expected content
			// Note: Some tokenizers may normalize whitespace or handle edge cases differently
			assert.Equal(t, tt.expected, result, "round-trip should preserve content")
		})
	}
}

func TestInitTokenizer(t *testing.T) {
	t.Run("initialization_succeeds", func(t *testing.T) {
		tkm, err := initTokenizer()
		require.NoError(t, err, "tokenizer initialization should succeed")
		assert.NotNil(t, tkm, "tokenizer should not be nil")
	})

	t.Run("multiple_initializations", func(t *testing.T) {
		// Test that multiple initializations work (important for concurrent usage)
		tkm1, err1 := initTokenizer()
		require.NoError(t, err1, "first initialization should succeed")
		assert.NotNil(t, tkm1, "first tokenizer should not be nil")

		tkm2, err2 := initTokenizer()
		require.NoError(t, err2, "second initialization should succeed")
		assert.NotNil(t, tkm2, "second tokenizer should not be nil")
	})
}

func TestTokenizerEdgeCases(t *testing.T) {
	t.Run("very_long_text", func(t *testing.T) {
		// Test with a very long string to ensure no issues with large inputs
		longText := ""
		for i := 0; i < 1000; i++ {
			longText += "This is a test sentence. "
		}

		tokens, err := GetTokens(longText)
		require.NoError(t, err, "should handle very long text")
		assert.NotEmpty(t, tokens, "should produce tokens for long text")
		assert.Greater(t, len(tokens), 1000, "long text should produce many tokens")

		// Test decoding the long text
		decoded, err := DecodeTokens(tokens)
		require.NoError(t, err, "should decode very long text")
		assert.NotEmpty(t, decoded, "decoded text should not be empty")
	})

	t.Run("newlines_and_whitespace", func(t *testing.T) {
		content := "Line 1\nLine 2\n\tTabbed line\n   Spaced line"
		tokens, err := GetTokens(content)
		require.NoError(t, err, "should handle newlines and whitespace")
		assert.NotEmpty(t, tokens, "should produce tokens")

		decoded, err := DecodeTokens(tokens)
		require.NoError(t, err, "should decode newlines and whitespace")
		// Note: exact whitespace preservation depends on the tokenizer
		assert.Contains(t, decoded, "Line 1", "should contain original content")
		assert.Contains(t, decoded, "Line 2", "should contain original content")
	})
}
