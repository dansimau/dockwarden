// Package dockerfile extracts base images from a Dockerfile.
//
// It understands multi-stage builds (FROM ... AS name, and later stages that
// reference earlier stage names), global ARGs that are substituted into FROM
// lines, the `# escape=` parser directive, line continuations and `scratch`.
package dockerfile

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/dansimau/dockwarden/internal/expand"
)

// Stage describes one FROM line in a Dockerfile.
type Stage struct {
	Index    int
	Name     string // stage alias (AS name) or ""
	Ref      string // image reference after ARG substitution, "" for scratch/stage refs
	Platform string // --platform value if given
	Scratch  bool
	// FromStage is set when the FROM references a previous stage by name or index.
	FromStage bool
}

// BaseImages returns the external image references used by the Dockerfile
// (deduplicated, in order of first appearance). Stage references and scratch
// are excluded. buildArgs override ARG defaults.
func BaseImages(r io.Reader, buildArgs map[string]string) ([]string, error) {
	stages, err := Parse(r, buildArgs)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range stages {
		if s.Scratch || s.FromStage || s.Ref == "" {
			continue
		}
		if !seen[s.Ref] {
			seen[s.Ref] = true
			out = append(out, s.Ref)
		}
	}
	return out, nil
}

var directiveRe = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)

// Parse returns all stages of the Dockerfile.
func Parse(r io.Reader, buildArgs map[string]string) ([]Stage, error) {
	lines, err := logicalLines(r)
	if err != nil {
		return nil, err
	}
	globalArgs := map[string]string{} // ARG defaults declared before the first FROM
	argLookup := func(name string) (string, bool) {
		if v, ok := buildArgs[name]; ok {
			return v, true
		}
		if v, ok := globalArgs[name]; ok {
			return v, true
		}
		return "", false
	}
	var stages []Stage
	stageNames := map[string]int{}
	seenFrom := false
	for _, line := range lines {
		fields := splitFields(line)
		if len(fields) == 0 {
			continue
		}
		instr := strings.ToUpper(fields[0])
		switch instr {
		case "ARG":
			if seenFrom {
				continue // ARGs after FROM don't affect FROM lines
			}
			for _, a := range fields[1:] {
				name, val, hasVal := strings.Cut(a, "=")
				if hasVal {
					globalArgs[name] = unquote(expand.Expand(val, argLookup))
				} else if _, ok := globalArgs[name]; !ok {
					// declared without default; leave unset unless provided via buildArgs
				}
			}
		case "FROM":
			seenFrom = true
			st := Stage{Index: len(stages)}
			args := fields[1:]
			// flags
			for len(args) > 0 && strings.HasPrefix(args[0], "--") {
				flag := args[0]
				args = args[1:]
				if v, ok := strings.CutPrefix(flag, "--platform="); ok {
					st.Platform = expand.Expand(v, argLookup)
				}
			}
			if len(args) == 0 {
				return nil, fmt.Errorf("FROM without image at stage %d", st.Index)
			}
			ref := expand.Expand(args[0], argLookup)
			if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
				st.Name = args[2]
				stageNames[strings.ToLower(st.Name)] = st.Index
			}
			switch {
			case ref == "":
				// ARG without value: unresolvable; skip
			case strings.EqualFold(ref, "scratch"):
				st.Scratch = true
			default:
				if _, ok := stageNames[strings.ToLower(ref)]; ok {
					st.FromStage = true
				} else if isStageIndex(ref, len(stages)) {
					st.FromStage = true
				} else {
					st.Ref = ref
				}
			}
			stages = append(stages, st)
		}
	}
	return stages, nil
}

func isStageIndex(s string, n int) bool {
	if s == "" {
		return false
	}
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		v = v*10 + int(c-'0')
	}
	return v < n
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// splitFields splits on whitespace while keeping quoted segments intact.
func splitFields(line string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := byte(0)
	flush := func() {
		if cur.Len() > 0 {
			fields = append(fields, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote != 0:
			cur.WriteByte(c)
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
			cur.WriteByte(c)
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return fields
}

// logicalLines joins continuation lines, strips comments and handles the
// escape directive.
func logicalLines(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	escape := byte('\\')
	var out []string
	var cur strings.Builder
	continuing := false
	first := true
	for sc.Scan() {
		raw := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(raw)
		if first {
			if m := directiveRe.FindStringSubmatch(trimmed); m != nil && strings.EqualFold(m[1], "escape") {
				if m[2] == "`" {
					escape = '`'
				}
				continue
			}
			if trimmed != "" {
				first = false
			}
		}
		if strings.HasPrefix(trimmed, "#") {
			continue // comments (also inside continuations) are ignored
		}
		if trimmed == "" {
			if continuing {
				continue
			}
			continue
		}
		if strings.HasSuffix(trimmed, string(escape)) {
			cur.WriteString(strings.TrimSuffix(trimmed, string(escape)))
			cur.WriteByte(' ')
			continuing = true
			continue
		}
		cur.WriteString(trimmed)
		out = append(out, cur.String())
		cur.Reset()
		continuing = false
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, sc.Err()
}
