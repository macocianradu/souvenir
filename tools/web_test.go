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
