package memory

import "testing"

func TestAnyOf(t *testing.T) {
	for query, want := range map[string]string{
		"net salary monthly":      "net or salary or monthly",
		`net "monthly pay" raise`: `net or "monthly pay" or raise`,
		"salary or raise":         "salary or raise",
		"  salary\traise ":        "salary or raise",
		"salary":                  "",
		"salary -bonus":           "",
		"":                        "",
	} {
		if got := anyOf(query); got != want {
			t.Errorf("anyOf(%q) = %q, want %q", query, got, want)
		}
	}
}
