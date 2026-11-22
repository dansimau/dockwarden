package compose

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dansimau/dockwarden/internal/expand"
)

var projectNameChars = regexp.MustCompile(`[a-z0-9_-]`)

// NormalizeProjectName applies docker compose's project name normalisation:
// lowercase, drop every character outside [a-z0-9_-], and strip leading
// underscores/dashes (a project name must start with a letter or digit).
func NormalizeProjectName(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(projectNameChars.FindAllString(s, -1), "")
	return strings.TrimLeft(s, "_-")
}

// DeriveProjectName determines the compose project name for a project the
// same way docker compose does when no -p flag is given:
//
//  1. the top-level `name:` key of the compose file,
//  2. COMPOSE_PROJECT_NAME from the project's .env file (envFile; may be ""
//     or missing) or the process environment,
//  3. the basename of the project directory.
//
// The result is normalised. content may be nil when only the directory is
// known.
func DeriveProjectName(content []byte, projectDir, envFile string) string {
	fileVars := map[string]string{}
	if envFile != "" {
		fileVars = parseEnvFile(envFile)
	}
	// Process environment wins over the env file, but an empty value counts
	// as unset so a blank COMPOSE_PROJECT_NAME does not shadow the file.
	env := expand.Chain(func(name string) (string, bool) {
		v, ok := os.LookupEnv(name)
		return v, ok && v != ""
	}, expand.MapLookup(fileVars))

	if len(content) > 0 {
		var doc struct {
			Name string `yaml:"name"`
		}
		if yaml.Unmarshal(content, &doc) == nil && doc.Name != "" {
			if n := NormalizeProjectName(expand.Expand(doc.Name, env)); n != "" {
				return n
			}
		}
	}
	if v, ok := env("COMPOSE_PROJECT_NAME"); ok && v != "" {
		if n := NormalizeProjectName(v); n != "" {
			return n
		}
	}
	return NormalizeProjectName(filepath.Base(filepath.Clean(projectDir)))
}
