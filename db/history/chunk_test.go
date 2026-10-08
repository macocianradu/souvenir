package history

import (
	"reflect"
	"testing"
)

func TestChunk(t *testing.T) {
	for _, tc := range []struct {
		name          string
		text          string
		size, overlap int
		want          []string
	}{
		{"empty text", "  ", 4, 1, nil},
		{"shorter than a chunk", "a b c", 4, 1, []string{"a b c"}},
		{"exactly one chunk", "a b c d", 4, 1, []string{"a b c d"}},
		{"overlapping chunks", "a b c d e f", 4, 1, []string{"a b c d", "d e f"}},
		{"no overlap", "a b c d e", 2, 0, []string{"a b", "c d", "e"}},
		{"whitespace collapsed", "a\n\tb   c", 4, 1, []string{"a b c"}},
		{"overlap not smaller than size", "a b c", 2, 2, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Chunk(tc.text, tc.size, tc.overlap); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
