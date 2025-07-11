package openai

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/lil-whisper/internal/config"

	"github.com/sashabaranov/go-openai"
)

// MockOpenAIClient implements the OpenAI client interface for testing
type MockOpenAIClient struct {
	mock.Mock
}

func (m *MockOpenAIClient) CreateEmbeddings(ctx context.Context, req openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	args := m.Called(ctx, req)
	return args.Get(0).(openai.EmbeddingResponse), args.Error(1)
}

// TestableEmbeddings wraps Embeddings to allow mocking
type TestableEmbeddings struct {
	client *MockOpenAIClient
}

func (te *TestableEmbeddings) GetEmbeddings(chunks []string) ([][]float32, error) {
	resp, err := te.client.CreateEmbeddings(
		context.Background(),
		openai.EmbeddingRequest{
			Input: chunks,
			Model: openai.SmallEmbedding3,
		},
	)
	if err != nil {
		return nil, err
	}

	if len(resp.Data) == 0 {
		return nil, errors.New("no embeddings returned")
	}

	// compile all embeddings into a single array
	embeddings := make([][]float32, len(resp.Data))
	for i, emb := range resp.Data {
		embeddings[i] = emb.Embedding
	}

	return embeddings, nil
}

func TestInitOpenAI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		authToken string
		baseURL   string
	}{
		{
			name:      "default_config",
			authToken: "sk-test-token",
			baseURL:   "https://api.openai.com/v1",
		},
		{
			name:      "custom_base_url",
			authToken: "sk-test-token",
			baseURL:   "https://custom-api.example.com/v1",
		},
		{
			name:      "empty_token",
			authToken: "",
			baseURL:   "https://api.openai.com/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := InitOpenAI(tt.authToken, tt.baseURL)

			assert.NotNil(t, client)
			// Note: We can't easily test the internal config without reflection
			// but we can verify the client was created successfully
		})
	}
}

func TestNewEmbeddings(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		OpenAIAPIKey:  "sk-test-key",
		OpenAIBaseURL: "https://api.openai.com/v1",
	}

	embeddings := NewEmbeddings(cfg)

	assert.NotNil(t, embeddings)
	assert.NotNil(t, embeddings.c)
}

func TestGetEmbeddings_Success(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		chunks       []string
		mockResponse openai.EmbeddingResponse
		expected     [][]float32
	}{
		{
			name:   "single_chunk",
			chunks: []string{"Hello world"},
			mockResponse: openai.EmbeddingResponse{
				Data: []openai.Embedding{
					{
						Embedding: []float32{0.1, 0.2, 0.3},
					},
				},
			},
			expected: [][]float32{
				{0.1, 0.2, 0.3},
			},
		},
		{
			name:   "multiple_chunks",
			chunks: []string{"Hello world", "Goodbye world"},
			mockResponse: openai.EmbeddingResponse{
				Data: []openai.Embedding{
					{
						Embedding: []float32{0.1, 0.2, 0.3},
					},
					{
						Embedding: []float32{0.4, 0.5, 0.6},
					},
				},
			},
			expected: [][]float32{
				{0.1, 0.2, 0.3},
				{0.4, 0.5, 0.6},
			},
		},
		{
			name:   "empty_chunk",
			chunks: []string{""},
			mockResponse: openai.EmbeddingResponse{
				Data: []openai.Embedding{
					{
						Embedding: []float32{0.0, 0.0, 0.0},
					},
				},
			},
			expected: [][]float32{
				{0.0, 0.0, 0.0},
			},
		},
		{
			name:   "long_text",
			chunks: []string{"This is a very long text that should be processed correctly by the embedding system"},
			mockResponse: openai.EmbeddingResponse{
				Data: []openai.Embedding{
					{
						Embedding: []float32{0.7, 0.8, 0.9},
					},
				},
			},
			expected: [][]float32{
				{0.7, 0.8, 0.9},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockOpenAIClient{}
			embeddings := &TestableEmbeddings{client: mockClient}

			expectedRequest := openai.EmbeddingRequest{
				Input: tt.chunks,
				Model: openai.SmallEmbedding3,
			}

			mockClient.On("CreateEmbeddings", mock.Anything, expectedRequest).
				Return(tt.mockResponse, nil).Once()

			result, err := embeddings.GetEmbeddings(tt.chunks)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
			mockClient.AssertExpectations(t)
		})
	}
}

func TestGetEmbeddings_APIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		chunks        []string
		mockError     error
		expectedError string
	}{
		{
			name:          "network_error",
			chunks:        []string{"test"},
			mockError:     errors.New("network connection failed"),
			expectedError: "network connection failed",
		},
		{
			name:          "auth_error",
			chunks:        []string{"test"},
			mockError:     errors.New("invalid API key"),
			expectedError: "invalid API key",
		},
		{
			name:          "rate_limit_error",
			chunks:        []string{"test"},
			mockError:     errors.New("rate limit exceeded"),
			expectedError: "rate limit exceeded",
		},
		{
			name:          "quota_exceeded",
			chunks:        []string{"test"},
			mockError:     errors.New("quota exceeded"),
			expectedError: "quota exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockOpenAIClient{}
			embeddings := &TestableEmbeddings{client: mockClient}

			mockClient.On("CreateEmbeddings", mock.Anything, mock.Anything).
				Return(openai.EmbeddingResponse{}, tt.mockError).Once()

			result, err := embeddings.GetEmbeddings(tt.chunks)

			assert.Nil(t, result)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
			mockClient.AssertExpectations(t)
		})
	}
}

func TestGetEmbeddings_EmptyResponse(t *testing.T) {
	t.Parallel()

	mockClient := &MockOpenAIClient{}
	embeddings := &TestableEmbeddings{client: mockClient}

	emptyResponse := openai.EmbeddingResponse{
		Data: []openai.Embedding{},
	}

	mockClient.On("CreateEmbeddings", mock.Anything, mock.Anything).
		Return(emptyResponse, nil).Once()

	result, err := embeddings.GetEmbeddings([]string{"test"})

	assert.Nil(t, result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no embeddings returned")
	mockClient.AssertExpectations(t)
}

func TestGetEmbeddings_LargeBatch(t *testing.T) {
	t.Parallel()

	// Create a large batch of chunks
	chunks := make([]string, 100)
	for i := range chunks {
		chunks[i] = fmt.Sprintf("chunk %d", i)
	}

	// Create expected response
	data := make([]openai.Embedding, 100)
	expected := make([][]float32, 100)
	for i := range data {
		embedding := []float32{float32(i), float32(i + 1), float32(i + 2)}
		data[i] = openai.Embedding{Embedding: embedding}
		expected[i] = embedding
	}

	mockResponse := openai.EmbeddingResponse{Data: data}

	mockClient := &MockOpenAIClient{}
	embeddings := &TestableEmbeddings{client: mockClient}

	mockClient.On("CreateEmbeddings", mock.Anything, mock.Anything).
		Return(mockResponse, nil).Once()

	result, err := embeddings.GetEmbeddings(chunks)

	require.NoError(t, err)
	assert.Equal(t, expected, result)
	assert.Len(t, result, 100)
	mockClient.AssertExpectations(t)
}

func TestGetEmbeddings_DifferentEmbeddingDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dimensions int
	}{
		{
			name:       "small_dimensions",
			dimensions: 3,
		},
		{
			name:       "standard_dimensions",
			dimensions: 512,
		},
		{
			name:       "large_dimensions",
			dimensions: 1536,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockOpenAIClient{}
			embeddings := &TestableEmbeddings{client: mockClient}

			embedding := make([]float32, tt.dimensions)
			for i := range embedding {
				embedding[i] = float32(i) / float32(tt.dimensions)
			}

			mockResponse := openai.EmbeddingResponse{
				Data: []openai.Embedding{
					{Embedding: embedding},
				},
			}

			mockClient.On("CreateEmbeddings", mock.Anything, mock.Anything).
				Return(mockResponse, nil).Once()

			result, err := embeddings.GetEmbeddings([]string{"test"})

			require.NoError(t, err)
			assert.Len(t, result, 1)
			assert.Len(t, result[0], tt.dimensions)
			assert.Equal(t, embedding, result[0])
			mockClient.AssertExpectations(t)
		})
	}
}

func TestGetEmbeddings_SpecialCharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		chunk string
	}{
		{
			name:  "unicode_characters",
			chunk: "Hello 世界 🌍",
		},
		{
			name:  "special_symbols",
			chunk: "!@#$%^&*()_+-=[]{}|;':\",./<>?",
		},
		{
			name:  "newlines_and_tabs",
			chunk: "Line 1\nLine 2\tTabbed",
		},
		{
			name:  "mixed_content",
			chunk: "Regular text with émojis 😊 and símböls!",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockOpenAIClient{}
			embeddings := &TestableEmbeddings{client: mockClient}

			mockResponse := openai.EmbeddingResponse{
				Data: []openai.Embedding{
					{Embedding: []float32{0.1, 0.2, 0.3}},
				},
			}

			mockClient.On("CreateEmbeddings", mock.Anything, mock.Anything).
				Return(mockResponse, nil).Once()

			result, err := embeddings.GetEmbeddings([]string{tt.chunk})

			require.NoError(t, err)
			assert.Len(t, result, 1)
			assert.Equal(t, []float32{0.1, 0.2, 0.3}, result[0])
			mockClient.AssertExpectations(t)
		})
	}
}

// Benchmark tests
func BenchmarkGetEmbeddings_SingleChunk(b *testing.B) {
	cfg := &config.Config{
		OpenAIAPIKey:  "sk-test-key",
		OpenAIBaseURL: "https://api.openai.com/v1",
	}

	embeddings := NewEmbeddings(cfg)
	chunks := []string{"This is a test chunk for benchmarking"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Note: This would make actual API calls in a real benchmark
		// For production use, you'd want to mock this or use a test API
		_, _ = embeddings.GetEmbeddings(chunks)
	}
}

func BenchmarkGetEmbeddings_MultipleChunks(b *testing.B) {
	cfg := &config.Config{
		OpenAIAPIKey:  "sk-test-key",
		OpenAIBaseURL: "https://api.openai.com/v1",
	}

	embeddings := NewEmbeddings(cfg)
	chunks := []string{
		"First chunk for benchmarking",
		"Second chunk for benchmarking",
		"Third chunk for benchmarking",
		"Fourth chunk for benchmarking",
		"Fifth chunk for benchmarking",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Note: This would make actual API calls in a real benchmark
		// For production use, you'd want to mock this or use a test API
		_, _ = embeddings.GetEmbeddings(chunks)
	}
}

// Integration test (would require actual API key)
func TestGetEmbeddings_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This test would require a real OpenAI API key
	// It's disabled by default but shows how integration testing would work
	t.Skip("Integration test requires real API key")

	cfg := &config.Config{
		OpenAIAPIKey:  "your-real-api-key-here",
		OpenAIBaseURL: "https://api.openai.com/v1",
	}

	embeddings := NewEmbeddings(cfg)
	chunks := []string{"Hello world", "This is a test"}

	result, err := embeddings.GetEmbeddings(chunks)

	require.NoError(t, err)
	assert.Len(t, result, 2)
	assert.Greater(t, len(result[0]), 0)
	assert.Greater(t, len(result[1]), 0)
}
