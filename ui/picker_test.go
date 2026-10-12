package ui

import "testing"

func TestPickerRemove_KeepsTheFullListIntact(t *testing.T) {
	p := newPicker()
	p, _ = p.Update(modelsLoadedMsg{models: []modelItem{{id: "a"}, {id: "b"}, {id: "c"}}, searchable: true})

	p.remove("a")

	ids := func(items []modelItem) (out []string) {
		for _, it := range items {
			out = append(out, it.id)
		}
		return out
	}
	var shown, all []modelItem
	for _, it := range p.list.Items() {
		shown = append(shown, it.(modelItem))
	}
	for _, it := range p.allItems {
		if it == nil {
			t.Fatal("nil left in allItems")
		}
		all = append(all, it.(modelItem))
	}
	if got := ids(shown); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("shown %v", got)
	}
	if got := ids(all); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("all %v", got)
	}
}
