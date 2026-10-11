package tools

import (
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/web"
)

func TestFormatResults_NumbersAndLimitsResults(t *testing.T) {
	found := web.Results{
		Answers: []string{"42"},
		Results: []web.Result{{Title: "A", Url: "https://a.test", Content: "first"}, {Title: "B", Url: "https://b.test"}},
	}
	out := formatResults(found, 1)
	if !strings.HasPrefix(out, "Answer: 42\n\n1. A\nhttps://a.test\nfirst") || strings.Contains(out, "b.test") {
		t.Errorf("%q", out)
	}
	if formatResults(web.Results{}, 5) != "No results found." {
		t.Error("empty results")
	}
}

func TestFormatPage_PagesThroughLongContent(t *testing.T) {
	page := web.Page{Url: "https://x.test", Title: "T", Content: "abcdefghij"}
	first := formatPage(page, 0, 4)
	if !strings.Contains(first, "abcd\n\n[Characters 0 to 4 of 10. Call web_fetch with start=4 for more.]") {
		t.Errorf("%q", first)
	}
	last := formatPage(page, 8, 4)
	if !strings.HasSuffix(last, "ij\n\n[Characters 8 to 10 of 10.]") {
		t.Errorf("%q", last)
	}
	if whole := formatPage(page, 0, 100); strings.Contains(whole, "[Characters") {
		t.Errorf("%q", whole)
	}
	if past := formatPage(page, 20, 4); !strings.Contains(past, "only 10 characters") {
		t.Errorf("%q", past)
	}
}
