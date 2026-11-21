// Package expand implements shell-style variable expansion as used by
// Dockerfiles (ARG substitution in FROM lines) and docker compose files.
//
// Supported forms: $VAR, ${VAR}, ${VAR:-default}, ${VAR-default},
// ${VAR:+alt}, ${VAR+alt}. A literal dollar sign is written as $$.
package expand

import "strings"

// Lookup returns the value for a variable name and whether it is set.
type Lookup func(name string) (string, bool)

// MapLookup adapts a map to a Lookup.
func MapLookup(m map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

// Chain returns a Lookup that consults each lookup in order.
func Chain(lookups ...Lookup) Lookup {
	return func(name string) (string, bool) {
		for _, l := range lookups {
			if l == nil {
				continue
			}
			if v, ok := l(name); ok {
				return v, true
			}
		}
		return "", false
	}
}

// Expand substitutes variables in s using lookup. Unknown variables expand
// to the empty string, matching Dockerfile and compose behaviour.
func Expand(s string, lookup Lookup) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var out strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '$' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			out.WriteByte('$')
			i++
			continue
		}
		next := s[i+1]
		switch {
		case next == '$':
			out.WriteByte('$')
			i += 2
		case next == '{':
			end := findClosingBrace(s, i+2)
			if end < 0 {
				out.WriteString(s[i:])
				return out.String()
			}
			out.WriteString(expandBraced(s[i+2:end], lookup))
			i = end + 1
		case isNameStart(next):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			if v, ok := lookup(s[i+1 : j]); ok {
				out.WriteString(v)
			}
			i = j
		default:
			out.WriteByte('$')
			i++
		}
	}
	return out.String()
}

func findClosingBrace(s string, from int) int {
	depth := 1
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func expandBraced(expr string, lookup Lookup) string {
	nameEnd := 0
	for nameEnd < len(expr) && isNameChar(expr[nameEnd]) {
		nameEnd++
	}
	name, rest := expr[:nameEnd], expr[nameEnd:]
	val, set := lookup(name)
	switch {
	case rest == "":
		return val
	case strings.HasPrefix(rest, ":-"):
		if !set || val == "" {
			return Expand(rest[2:], lookup)
		}
		return val
	case strings.HasPrefix(rest, "-"):
		if !set {
			return Expand(rest[1:], lookup)
		}
		return val
	case strings.HasPrefix(rest, ":+"):
		if set && val != "" {
			return Expand(rest[2:], lookup)
		}
		return ""
	case strings.HasPrefix(rest, "+"):
		if set {
			return Expand(rest[1:], lookup)
		}
		return ""
	}
	// ":?" / "?" error forms: behave like a plain lookup.
	return val
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
