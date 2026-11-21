package expand

import "testing"

func TestExpand(t *testing.T) {
	env := MapLookup(map[string]string{"A": "a", "EMPTY": "", "TAG": "1.2"})
	cases := map[string]string{
		"plain":               "plain",
		"$A":                  "a",
		"${A}":                "a",
		"x${A}y":              "xay",
		"${B:-def}":           "def",
		"${EMPTY:-def}":       "def",
		"${EMPTY-def}":        "",
		"${B-def}":            "def",
		"${A:+alt}":           "alt",
		"${EMPTY:+alt}":       "",
		"${B+alt}":            "",
		"${A+alt}":            "alt",
		"$$A":                 "$A",
		"nginx:${TAG}":        "nginx:1.2",
		"$UNSET":              "",
		"${B:-${A}}":          "a",
		"${B:-nested-${TAG}}": "nested-1.2",
		"$":                   "$",
		"$1":                  "$1",
		"${unterminated":      "${unterminated",
	}
	for in, want := range cases {
		if got := Expand(in, env); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChain(t *testing.T) {
	l := Chain(MapLookup(map[string]string{"A": "1"}), MapLookup(map[string]string{"A": "2", "B": "3"}))
	if v, _ := l("A"); v != "1" {
		t.Fatalf("A = %q", v)
	}
	if v, _ := l("B"); v != "3" {
		t.Fatalf("B = %q", v)
	}
	if _, ok := l("C"); ok {
		t.Fatal("C should be unset")
	}
}
