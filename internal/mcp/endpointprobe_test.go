package mcp

import "testing"

// modelLoaded must tolerate ollama's `:tag` suffix so a working endpoint that
// lists `nomic-embed-text:latest` isn't reported as model_not_loaded when the
// config names the model bare (`nomic-embed-text`), and vice-versa, while a
// genuine miss still returns false.
func TestModelLoaded(t *testing.T) {
	// fetch() lower-cases the keys; mirror that here.
	withLatest := map[string]bool{"nomic-embed-text:latest": true}
	bare := map[string]bool{"nomic-embed-text": true}
	tagged := map[string]bool{"qwen2.5:7b-instruct": true}
	other := map[string]bool{"llama3:latest": true, "mistral": true}

	cases := []struct {
		name   string
		models map[string]bool
		model  string
		want   bool
	}{
		{"exact match", bare, "nomic-embed-text", true},
		{"configured bare vs returned :latest", withLatest, "nomic-embed-text", true},
		{"configured :latest vs returned bare", bare, "nomic-embed-text:latest", true},
		{"both :latest", withLatest, "nomic-embed-text:latest", true},
		{"case-insensitive", withLatest, "Nomic-Embed-Text", true},
		{"configured bare vs returned other tag", tagged, "qwen2.5", true},
		{"genuine miss", other, "nomic-embed-text", false},
		{"miss against tagged", tagged, "nomic-embed-text", false},
		// A bare prefix that isn't a tag boundary must NOT match (substring guard).
		{"prefix without colon is not a match", map[string]bool{"nomic-embed-text-v2": true}, "nomic-embed-text", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelLoaded(tc.models, tc.model); got != tc.want {
				t.Errorf("modelLoaded(%v, %q) = %v, want %v", tc.models, tc.model, got, tc.want)
			}
		})
	}
}
