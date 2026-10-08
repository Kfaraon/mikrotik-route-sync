package bot

import (
	"strings"
	"testing"
)

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: %#v", got)
	}
	if got := splitMessage("abc", 0); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("non-positive chunk: %#v", got)
	}
	got := splitMessage("abcdef", 2)
	if len(got) != 3 || got[0] != "ab" || got[2] != "ef" {
		t.Fatalf("chunks: %#v", got)
	}
	// Многобайтовые руны (кириллица, эмодзи) не разрываются.
	src := "привет🌙мир"
	parts := splitMessage(src, 4)
	if strings.Join(parts, "") != src {
		t.Fatalf("multibyte broken: %#v", parts)
	}
	for _, p := range parts {
		if len([]rune(p)) > 4 {
			t.Fatalf("chunk too long: %q", p)
		}
	}
	big := strings.Repeat("x", 9000)
	if n := len(splitMessage(big, 4000)); n != 3 {
		t.Fatalf("9000/4000 want 3 parts, got %d", n)
	}
}
