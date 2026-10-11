package ui

import (
	"strings"
	"testing"
)

// Spans written as [b]…[/b] and so on, so the cases read without escape codes.
var readable = strings.NewReplacer(
	boldSpan.on, "[b]", boldSpan.off, "[/b]",
	italicSpan.on, "[i]", italicSpan.off, "[/i]",
	strikeSpan.on, "[s]", strikeSpan.off, "[/s]",
	codeSpan.on, "[c]", codeSpan.off, "[/c]",
)

func TestFormatMarkdown(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"bold":                     {"a **b** c", "a [b]b[/b] c"},
		"bold underscores":         {"__b__", "[b]b[/b]"},
		"italic":                   {"a *b* c", "a [i]b[/i] c"},
		"italic underscores":       {"_b_.", "[i]b[/i]."},
		"bold italic":              {"***b***", "[b][i]b[/i][/b]"},
		"strike":                   {"~b~ and ~~c~~", "[s]b[/s] and [s]c[/s]"},
		"nested":                   {"**bold *and italic* too**", "[b]bold [i]and italic[/i] too[/b]"},
		"code":                     {"run `a *b* c`", "run [c]a *b* c[/c]"},
		"code inside bold":         {"**see `x*`**", "[b]see [c]x*[/c][/b]"},
		"heading":                  {"### **Tools**", "[b][b]Tools[/b][/b]"},
		"escaped":                  {`\*not italic\*`, "*not italic*"},
		"snake_case":               {"call calendar_list or memory_save", "call calendar_list or memory_save"},
		"approximate":              {"~5 min and ~10 min", "~5 min and ~10 min"},
		"arithmetic":               {"2*3*4", "2*3*4"},
		"unclosed while streaming": {"so **bol", "so **bol"},
		"space after opener":       {"a * b * c", "a * b * c"},
		"hash without space":       {"#hashtag", "#hashtag"},
		"fenced block":             {"```\n**x**\n```\n**y**", "```\n**x**\n```\n[b]y[/b]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := readable.Replace(formatMarkdown(c.in)); got != c.want {
				t.Errorf("formatMarkdown(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
