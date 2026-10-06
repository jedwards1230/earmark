package db

import (
	"reflect"
	"testing"
)

// TestDecodeChapters covers the read-path clean-at-most-once rule: rows stored
// before ingest-time cleaning (no RawTitle anywhere) are cleaned on read, while
// a row already cleaned at ingest (any RawTitle present) is returned exactly as
// stored — re-cleaning it would over-strip titles that merely look like debris.
func TestDecodeChapters(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		stored string
		want   []string
	}{
		{
			name: "pre-cleaning row is cleaned on read",
			stored: `[{"Index":0,"Title":"1 - Project Hail Mary: Dedication","StartSec":0,"EndSec":60},` +
				`{"Index":1,"Title":"2 - Project Hail Mary: Chapter 1","StartSec":60,"EndSec":200}]`,
			want: []string{"Dedication", "Chapter 1"},
		},
		{
			name: "ingest-cleaned row is not cleaned twice",
			stored: `[{"Index":0,"Title":"1 - Intro","RawTitle":"1 - Book: 1 - Intro","StartSec":0,"EndSec":60},` +
				`{"Index":1,"Title":"2 - Body","RawTitle":"2 - Book: 2 - Body","StartSec":60,"EndSec":200}]`,
			want: []string{"1 - Intro", "2 - Body"},
		},
		{
			name:   "clean row passes through",
			stored: `[{"Index":0,"Title":"Prologue","StartSec":0,"EndSec":60},{"Index":1,"Title":"Chapter 1","StartSec":60,"EndSec":200}]`,
			want:   []string{"Prologue", "Chapter 1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeChapters([]byte(tc.stored))
			if err != nil {
				t.Fatalf("decodeChapters: %v", err)
			}
			titles := make([]string, len(got))
			for i, c := range got {
				titles[i] = c.Title
			}
			if !reflect.DeepEqual(titles, tc.want) {
				t.Fatalf("titles = %q, want %q", titles, tc.want)
			}
		})
	}

	if _, err := decodeChapters([]byte(`{not json`)); err == nil {
		t.Fatal("decodeChapters(malformed) = nil error, want error")
	}
}
