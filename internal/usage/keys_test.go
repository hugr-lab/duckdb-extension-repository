package usage_test

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/usage"
)

func TestKeys(t *testing.T) {
	k := &usage.Keys{Max: 3}
	a, b, c, d := [3]string{"a", "", "valid"}, [3]string{"b", "", "valid"}, [3]string{"c", "", "valid"}, [3]string{"d", "", "valid"}
	for range 2 {
		if !k.Admit("s", "2026-10-09", [][3]string{a, b}) {
			t.Fatal("within the budget, and again")
		}
	}
	if k.Admit("s", "2026-10-09", [][3]string{c, d}) {
		t.Fatal("two new keys over the budget: dropped whole")
	}
	if !k.Admit("s", "2026-10-09", [][3]string{a, c}) || k.Admit("s", "2026-10-09", [][3]string{d}) {
		t.Fatal("up to the budget, then none")
	}
	if !k.Admit("other", "2026-10-09", [][3]string{d}) {
		t.Fatal("another scope has its own")
	}
	if !k.Admit("s", "2026-10-10", [][3]string{d, a, b}) {
		t.Fatal("a new day starts again")
	}
	if k.Admit("s", "2026-10-09", [][3]string{d}) {
		t.Fatal("the day before is still bounded")
	}
	if !k.Admit("s", "2026-10-11", nil) || k.Admit("s", "2026-10-09", [][3]string{d}) {
		t.Fatal("two days are kept; an older one is refused")
	}
	if !k.Admit("s", "2026-10-10", [][3]string{a}) || k.Admit("s", "2026-10-10", [][3]string{c}) {
		t.Fatal("the day before the newest is kept, with its keys")
	}
}
