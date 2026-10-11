package ui

import (
	"slices"
	"strings"
	"unicode"
)

type span struct{ on, off string }

var (
	boldSpan   = span{"\x1b[1m", "\x1b[22m"}
	italicSpan = span{"\x1b[3m", "\x1b[23m"}
	strikeSpan = span{"\x1b[9m", "\x1b[29m"}
	codeSpan   = span{"\x1b[36m", "\x1b[39m"}
)

func (s span) wrap(text string) string { return s.on + text + s.off }

var delimiters = []struct {
	mark  string
	spans []span
}{
	{"***", []span{boldSpan, italicSpan}},
	{"___", []span{boldSpan, italicSpan}},
	{"**", []span{boldSpan}},
	{"__", []span{boldSpan}},
	{"~~", []span{strikeSpan}},
	{"*", []span{italicSpan}},
	{"_", []span{italicSpan}},
	{"~", []span{strikeSpan}},
}

func formatMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if heading, ok := headingText(line); ok {
			lines[i] = boldSpan.wrap(formatInline(heading))
			continue
		}
		lines[i] = formatInline(line)
	}
	return strings.Join(lines, "\n")
}

func headingText(line string) (string, bool) {
	level := len(line) - len(strings.TrimLeft(line, "#"))
	if level == 0 || level > 6 || !strings.HasPrefix(line[level:], " ") {
		return "", false
	}
	return strings.TrimSpace(line[level:]), true
}

func formatInline(line string) string {
	r := []rune(line)
	var b strings.Builder
	for i := 0; i < len(r); {
		if r[i] == '\\' && i+1 < len(r) && strings.ContainsRune("*_~`\\", r[i+1]) {
			b.WriteRune(r[i+1])
			i += 2
			continue
		}
		if r[i] == '`' {
			if end := indexRune(r, '`', i+1); end > i+1 {
				b.WriteString(codeSpan.wrap(string(r[i+1 : end])))
				i = end + 1
				continue
			}
		}
		if text, next, ok := emphasis(r, i); ok {
			b.WriteString(text)
			i = next
			continue
		}
		b.WriteRune(r[i])
		i++
	}
	return b.String()
}

func emphasis(r []rune, i int) (string, int, bool) {
	for _, d := range delimiters {
		mark := []rune(d.mark)
		if !hasAt(r, i, mark) || !canOpen(r, i, len(mark)) {
			continue
		}
		for j := i + len(mark) + 1; j+len(mark) <= len(r); j++ {
			if r[j] == '`' {
				if end := indexRune(r, '`', j+1); end > 0 {
					j = end
					continue
				}
			}
			if hasAt(r, j, mark) && canClose(r, j, len(mark)) {
				text := formatInline(string(r[i+len(mark) : j]))
				for _, v := range slices.Backward(d.spans) {
					text = v.wrap(text)
				}
				return text, j + len(mark), true
			}
		}
	}
	return "", 0, false
}

func hasAt(r []rune, i int, mark []rune) bool {
	if i+len(mark) > len(r) {
		return false
	}
	for k, c := range mark {
		if r[i+k] != c {
			return false
		}
	}
	return (i == 0 || r[i-1] != mark[0]) && (i+len(mark) == len(r) || r[i+len(mark)] != mark[0])
}

func canOpen(r []rune, i, n int) bool {
	return i+n < len(r) && !unicode.IsSpace(r[i+n]) && (i == 0 || !isWordRune(r[i-1]))
}

func canClose(r []rune, j, n int) bool {
	return !unicode.IsSpace(r[j-1]) && (j+n == len(r) || !isWordRune(r[j+n]))
}

func isWordRune(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c)
}

func indexRune(r []rune, c rune, from int) int {
	for k := from; k < len(r); k++ {
		if r[k] == c {
			return k
		}
	}
	return -1
}
