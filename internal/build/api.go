package build

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	buildtypes "github.com/docker/docker/api/types/build"

	"github.com/dansimau/dockwarden/internal/dockerapi"
)

// API builds through the Docker Engine ImageBuild endpoint using the classic
// builder. BuildKit-only Dockerfile features (RUN --mount, heredocs,
// COPY --link, ...) are not available.
type API struct {
	Client dockerapi.Client
	Logger *slog.Logger
}

func (b *API) Name() string { return "api" }

// Build implements Builder.
func (b *API) Build(ctx context.Context, req Request) error {
	if req.Spec == nil {
		return errors.New("api builder requires a build spec")
	}
	log := logger(b.Logger)
	log.Info("building via docker engine api", "project", req.Info.Project, "service", req.Info.Service, "context", req.Spec.Context, "tag", req.Tag)

	dockerfile := req.Spec.Dockerfile
	var inline []byte
	if req.Spec.DockerfileInline != "" {
		inline = []byte(req.Spec.DockerfileInline)
		dockerfile = ".dockwarden.Dockerfile"
	} else if abs := filepath.Clean(filepath.Join(req.Spec.Context, dockerfile)); !strings.HasPrefix(abs, req.Spec.Context+string(filepath.Separator)) {
		// Dockerfile outside the context: inject it.
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("read dockerfile: %w", err)
		}
		inline = data
		dockerfile = ".dockwarden.Dockerfile"
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeContext(pw, req.Spec.Context, dockerfile, inline))
	}()

	args := map[string]*string{}
	for k, v := range req.Spec.Args {
		v := v
		args[k] = &v
	}
	opts := buildtypes.ImageBuildOptions{
		Tags:        []string{req.Tag},
		Dockerfile:  dockerfile,
		BuildArgs:   args,
		Target:      req.Spec.Target,
		PullParent:  req.Pull,
		NoCache:     req.NoCache,
		Remove:      true,
		ForceRemove: true,
		Labels:      req.Labels,
	}
	resp, err := b.Client.ImageBuild(ctx, pr, opts)
	if err != nil {
		pr.Close()
		return fmt.Errorf("image build: %w", err)
	}
	defer resp.Body.Close()
	err = dockerapi.DrainJSONStream(resp.Body, func(m dockerapi.JSONMessage) {
		if s := strings.TrimSpace(m.Stream); s != "" {
			log.Debug(s)
		}
	})
	if err != nil {
		return fmt.Errorf("image build failed: %w", err)
	}
	return nil
}

// writeContext tars the build context honouring .dockerignore.
func writeContext(w io.Writer, root, dockerfile string, inline []byte) error {
	ignore, err := loadDockerignore(root, dockerfile)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	defer tw.Close()
	if inline != nil {
		if err := tw.WriteHeader(&tar.Header{Name: dockerfile, Mode: 0o644, Size: int64(len(inline))}); err != nil {
			return err
		}
		if _, err := tw.Write(inline); err != nil {
			return err
		}
	}
	return filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if ignore.Match(rel, fi.IsDir()) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// ignoreRules is a minimal .dockerignore matcher: supports `*`, `?`, `**`,
// directory patterns and `!` negation; patterns match the path or any of its
// parent directories.
type ignoreRules struct {
	rules []ignoreRule
}

type ignoreRule struct {
	pattern string
	negate  bool
}

func loadDockerignore(root, dockerfile string) (*ignoreRules, error) {
	r := &ignoreRules{}
	data, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			rule.negate = true
			line = line[1:]
		}
		line = strings.Trim(filepath.ToSlash(filepath.Clean(line)), "/")
		if line == "." || line == "" {
			continue
		}
		rule.pattern = line
		r.rules = append(r.rules, rule)
	}
	// Docker always includes the Dockerfile and .dockerignore.
	r.rules = append(r.rules, ignoreRule{pattern: dockerfile, negate: true}, ignoreRule{pattern: ".dockerignore", negate: true})
	return r, nil
}

// Match reports whether rel (slash separated) is excluded.
func (r *ignoreRules) Match(rel string, isDir bool) bool {
	if r == nil {
		return false
	}
	excluded := false
	for _, rule := range r.rules {
		if matchPattern(rule.pattern, rel) {
			excluded = !rule.negate
		}
	}
	return excluded
}

func matchPattern(pattern, rel string) bool {
	// Match the full path or any parent prefix.
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		if globMatch(pattern, strings.Join(parts[:i], "/")) {
			return true
		}
	}
	return false
}

// globMatch supports ** across path segments; other wildcards don't cross /.
func globMatch(pattern, path string) bool {
	if !strings.Contains(pattern, "**") {
		ok, _ := filepath.Match(pattern, path)
		return ok
	}
	pp := strings.Split(pattern, "/")
	sp := strings.Split(path, "/")
	return globSegments(pp, sp)
}

func globSegments(pp, sp []string) bool {
	for len(pp) > 0 {
		if pp[0] == "**" {
			if len(pp) == 1 {
				return true
			}
			for i := 0; i <= len(sp); i++ {
				if globSegments(pp[1:], sp[i:]) {
					return true
				}
			}
			return false
		}
		if len(sp) == 0 {
			return false
		}
		if ok, _ := filepath.Match(pp[0], sp[0]); !ok {
			return false
		}
		pp, sp = pp[1:], sp[1:]
	}
	return len(sp) == 0
}
