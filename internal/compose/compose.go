// Package compose detects docker compose managed containers from their
// labels and reads the build configuration for a service from the project's
// compose files.
package compose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dansimau/dockwarden/internal/expand"
)

// Labels set by docker compose.
const (
	LabelProject     = "com.docker.compose.project"
	LabelService     = "com.docker.compose.service"
	LabelWorkingDir  = "com.docker.compose.project.working_dir"
	LabelConfigFiles = "com.docker.compose.project.config_files"
	LabelEnvFiles    = "com.docker.compose.project.environment_file"
	LabelOneOff      = "com.docker.compose.oneoff"
	LabelNumber      = "com.docker.compose.container-number"
)

// Info describes the compose project a container belongs to.
type Info struct {
	Project     string
	Service     string
	WorkingDir  string
	ConfigFiles []string
	EnvFiles    []string
	OneOff      bool
}

// FromLabels extracts compose info from container labels.
func FromLabels(labels map[string]string) (Info, bool) {
	project := labels[LabelProject]
	service := labels[LabelService]
	if project == "" || service == "" {
		return Info{}, false
	}
	info := Info{
		Project:    project,
		Service:    service,
		WorkingDir: labels[LabelWorkingDir],
		OneOff:     labels[LabelOneOff] == "True" || labels[LabelOneOff] == "true",
	}
	info.ConfigFiles = splitList(labels[LabelConfigFiles])
	info.EnvFiles = splitList(labels[LabelEnvFiles])
	return info, true
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// PathMapper rewrites host paths to local paths using prefix mappings.
type PathMapper map[string]string

// Map rewrites p using the longest matching prefix.
func (m PathMapper) Map(p string) string {
	best := ""
	for prefix := range m {
		if (p == prefix || strings.HasPrefix(p, strings.TrimSuffix(prefix, "/")+"/")) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return p
	}
	return filepath.Join(m[best], strings.TrimPrefix(p, strings.TrimSuffix(best, "/")))
}

// LocalWorkingDir returns the project directory as visible locally.
func (i Info) LocalWorkingDir(m PathMapper) string {
	if i.WorkingDir == "" {
		return ""
	}
	return m.Map(i.WorkingDir)
}

// LocalConfigFiles returns the compose files as visible locally.
func (i Info) LocalConfigFiles(m PathMapper) []string {
	out := make([]string, 0, len(i.ConfigFiles))
	for _, f := range i.ConfigFiles {
		if !filepath.IsAbs(f) && i.WorkingDir != "" {
			f = filepath.Join(i.WorkingDir, f)
		}
		out = append(out, m.Map(f))
	}
	return out
}

// BuildSpec is the resolved `build:` section of a compose service.
type BuildSpec struct {
	// Context is the absolute local build context directory.
	Context string
	// Dockerfile is the path of the Dockerfile relative to Context.
	Dockerfile string
	// DockerfileInline holds inline Dockerfile content if used instead.
	DockerfileInline string
	Args             map[string]string
	Target           string
	// Image is the `image:` name of the service if set.
	Image string
	// Pull mirrors build.pull.
	Pull bool
}

// DockerfilePath returns the absolute path of the Dockerfile ("" for inline).
func (b BuildSpec) DockerfilePath() string {
	if b.DockerfileInline != "" {
		return ""
	}
	return filepath.Join(b.Context, b.Dockerfile)
}

// ReadDockerfile returns the Dockerfile content.
func (b BuildSpec) ReadDockerfile() ([]byte, error) {
	if b.DockerfileInline != "" {
		return []byte(b.DockerfileInline), nil
	}
	return os.ReadFile(b.DockerfilePath())
}

// ErrNoBuild is returned when the service has no build section.
var ErrNoBuild = errors.New("service has no build section")

// LoadBuildSpec reads the compose files of the project and returns the
// service's build configuration. Returns ErrNoBuild if the service uses an
// image only.
func LoadBuildSpec(info Info, m PathMapper) (*BuildSpec, error) {
	files := info.LocalConfigFiles(m)
	if len(files) == 0 {
		return nil, errors.New("no compose config files recorded in labels")
	}
	env := loadEnv(info, m)
	var merged map[string]any
	var image string
	var baseDir string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read compose file: %w (is the project directory mounted at the same path as on the host?)", err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		interpolate(doc, env)
		services, _ := doc["services"].(map[string]any)
		svc, ok := services[info.Service].(map[string]any)
		if !ok {
			continue
		}
		if img, ok := svc["image"].(string); ok {
			image = img
		}
		if b, ok := svc["build"]; ok {
			bm := toBuildMap(b)
			if merged == nil {
				merged = map[string]any{}
			}
			for k, v := range bm {
				merged[k] = v
			}
			if _, ok := bm["context"]; ok || baseDir == "" {
				baseDir = filepath.Dir(f)
			}
		}
	}
	if merged == nil {
		return nil, ErrNoBuild
	}
	spec := &BuildSpec{Dockerfile: "Dockerfile", Image: image, Args: map[string]string{}}
	ctxDir, _ := merged["context"].(string)
	if ctxDir == "" {
		ctxDir = "."
	}
	if strings.Contains(ctxDir, "://") || strings.HasPrefix(ctxDir, "git@") {
		return nil, fmt.Errorf("remote build context %q is not supported", ctxDir)
	}
	if !filepath.IsAbs(ctxDir) {
		ctxDir = filepath.Join(baseDir, ctxDir)
	} else {
		ctxDir = m.Map(ctxDir)
	}
	spec.Context = filepath.Clean(ctxDir)
	if df, ok := merged["dockerfile"].(string); ok && df != "" {
		spec.Dockerfile = df
	}
	if inline, ok := merged["dockerfile_inline"].(string); ok && inline != "" {
		spec.DockerfileInline = inline
	}
	if t, ok := merged["target"].(string); ok {
		spec.Target = t
	}
	if p, ok := merged["pull"].(bool); ok {
		spec.Pull = p
	}
	switch args := merged["args"].(type) {
	case map[string]any:
		for k, v := range args {
			if v == nil {
				if ev, ok := env(k); ok {
					spec.Args[k] = ev
				}
				continue
			}
			spec.Args[k] = fmt.Sprint(v)
		}
	case []any:
		for _, a := range args {
			s := fmt.Sprint(a)
			k, v, has := strings.Cut(s, "=")
			if has {
				spec.Args[k] = v
			} else if ev, ok := env(k); ok {
				spec.Args[k] = ev
			}
		}
	}
	return spec, nil
}

func toBuildMap(b any) map[string]any {
	switch v := b.(type) {
	case string:
		return map[string]any{"context": v}
	case map[string]any:
		return v
	}
	return map[string]any{}
}

// loadEnv builds the interpolation environment: process env, then the
// project's env files (.env by default), process env taking precedence as
// in docker compose.
func loadEnv(info Info, m PathMapper) expand.Lookup {
	fileVars := map[string]string{}
	var envFiles []string
	for _, f := range info.EnvFiles {
		if !filepath.IsAbs(f) && info.WorkingDir != "" {
			f = filepath.Join(info.WorkingDir, f)
		}
		envFiles = append(envFiles, m.Map(f))
	}
	if len(envFiles) == 0 && info.WorkingDir != "" {
		envFiles = []string{filepath.Join(info.LocalWorkingDir(m), ".env")}
	}
	for _, f := range envFiles {
		for k, v := range parseEnvFile(f) {
			fileVars[k] = v
		}
	}
	return expand.Chain(os.LookupEnv, expand.MapLookup(fileVars))
}

func parseEnvFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// interpolate expands ${VAR} in all string values of the document in place.
func interpolate(node any, env expand.Lookup) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if s, ok := child.(string); ok {
				v[k] = expand.Expand(s, env)
			} else {
				interpolate(child, env)
			}
		}
	case []any:
		for i, child := range v {
			if s, ok := child.(string); ok {
				v[i] = expand.Expand(s, env)
			} else {
				interpolate(child, env)
			}
		}
	}
}

// DefaultImageName returns the image name compose gives a built service when
// no `image:` is set.
func DefaultImageName(project, service string) string {
	return project + "-" + service
}

// SortedArgs returns build args as sorted KEY=VALUE strings.
func (b BuildSpec) SortedArgs() []string {
	out := make([]string, 0, len(b.Args))
	for k, v := range b.Args {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
