// Package engine implements a dockwarden run: scan containers, compare
// digests with the registry, pull/rebuild and hand updated containers to
// the configured strategy.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"

	"github.com/dansimau/dockwarden/internal/build"
	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/dockerapi"
	"github.com/dansimau/dockwarden/internal/dockerfile"
	"github.com/dansimau/dockwarden/internal/notify"
	"github.com/dansimau/dockwarden/internal/registry"
	"github.com/dansimau/dockwarden/internal/state"
	"github.com/dansimau/dockwarden/internal/strategy"
)

// Labels understood by dockwarden.
const (
	LabelEnable      = "com.dockwarden.enable"
	LabelMonitorOnly = "com.dockwarden.monitor-only"
	// Watchtower labels are honoured as a fallback to ease migration.
	LabelWatchtowerEnable      = "com.centurylinklabs.watchtower.enable"
	LabelWatchtowerMonitorOnly = "com.centurylinklabs.watchtower.monitor-only"
)

// Resolver resolves what a tag points to in the registry.
type Resolver interface {
	Resolve(ctx context.Context, img registry.Image, localDigests []string) (registry.Remote, error)
}

// RunScoped is optionally implemented by strategies that keep per-run state
// (e.g. a DSM session).
type RunScoped interface {
	BeginRun(ctx context.Context) error
	EndRun(ctx context.Context) error
}

// Options configure an Engine.
type Options struct {
	Docker      dockerapi.Client
	Registry    Resolver
	Credentials registry.CredentialStore
	State       *state.File
	Strategy    strategy.Strategy
	Builder     build.Builder
	Notifier    *notify.Webhook
	Logger      *slog.Logger

	DryRun         bool
	MonitorOnly    bool
	LabelEnable    bool
	IncludeStopped bool
	Containers     []string
	Cleanup        bool
	BuildPull      bool
	BuildNoCache   bool
	PathMap        compose.PathMapper
	// SelfID is the hostname (= container ID prefix when running in Docker),
	// used to avoid updating ourselves.
	SelfID string
}

// Engine runs update cycles.
type Engine struct {
	o   Options
	log *slog.Logger
}

// New creates an engine.
func New(o Options) *Engine {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Credentials == nil {
		o.Credentials = registry.NoCredentials{}
	}
	return &Engine{o: o, log: o.Logger}
}

// Item is the outcome for one container.
type Item struct {
	Container  string
	Kind       strategy.Kind
	Image      string
	Project    string
	Service    string
	OldDigest  string
	NewDigest  string
	BaseImages []string
	Error      string
}

// Result summarises a run.
type Result struct {
	StartedAt time.Time
	Duration  time.Duration
	Scanned   int
	Updated   []Item
	Pending   []Item
	Failed    []Item
	Errors    []string
}

// ExitCode maps the result to a process exit code: 0 success, 2 if any
// update failed, updatesCode (if non-zero) when updates are pending.
func (r *Result) ExitCode(updatesCode int) int {
	if len(r.Failed) > 0 || len(r.Errors) > 0 {
		return 2
	}
	if updatesCode != 0 && len(r.Pending) > 0 {
		return updatesCode
	}
	return 0
}

// Summary is a one-line human summary.
func (r *Result) Summary() string {
	return fmt.Sprintf("scanned %d, updated %d, pending %d, failed %d", r.Scanned, len(r.Updated), len(r.Pending), len(r.Failed))
}

// candidate is a container with its classification.
type candidate struct {
	inspect     container.InspectResponse
	name        string
	imageRef    string // tag reference to run
	compose     *compose.Info
	buildSpec   *compose.BuildSpec
	monitorOnly bool
}

// plan is the set of work discovered by a scan.
type plan struct {
	images   []*imageUpdate
	rebuilds []*rebuild
}

type imageUpdate struct {
	cand      *candidate
	img       registry.Image
	oldDigest string
	remote    registry.Remote
}

type rebuild struct {
	key         string
	info        compose.Info
	spec        *compose.BuildSpec
	containers  []*candidate
	tag         string
	changed     []string          // base refs that changed
	newDigests  map[string]string // base ref -> digest after the rebuild
	oldDigests  map[string]string
	monitorOnly bool
}

// Run executes one update cycle. The returned error is fatal (e.g. Docker
// unreachable); per-container problems are reported in Result.
func (e *Engine) Run(ctx context.Context) (*Result, error) {
	res := &Result{StartedAt: time.Now()}
	defer func() { res.Duration = time.Since(res.StartedAt) }()

	if _, err := e.o.Docker.Ping(ctx); err != nil {
		return res, fmt.Errorf("docker: %w", err)
	}
	if rs, ok := e.o.Strategy.(RunScoped); ok {
		if err := rs.BeginRun(ctx); err != nil {
			return res, err
		}
		defer func() {
			if err := rs.EndRun(ctx); err != nil {
				e.log.Warn("strategy cleanup failed", "error", err)
			}
		}()
	}

	cands, err := e.scan(ctx)
	if err != nil {
		return res, err
	}
	res.Scanned = len(cands)
	p := e.check(ctx, cands, res)

	if len(p.images) == 0 && len(p.rebuilds) == 0 {
		e.log.Info("all containers up to date", "scanned", res.Scanned)
	}
	e.execute(ctx, p, res)
	e.notify(ctx, res)
	e.log.Info("run complete", "summary", res.Summary(), "duration", res.Duration.Round(time.Millisecond))
	return res, nil
}

// scan lists and filters containers.
func (e *Engine) scan(ctx context.Context) ([]*candidate, error) {
	list, err := e.o.Docker.ContainerList(ctx, container.ListOptions{All: e.o.IncludeStopped})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	sort.Slice(list, func(i, j int) bool { return firstName(list[i]) < firstName(list[j]) })
	only := map[string]bool{}
	for _, n := range e.o.Containers {
		only[strings.TrimPrefix(n, "/")] = true
	}
	var out []*candidate
	for _, c := range list {
		name := firstName(c)
		log := e.log.With("container", name)
		if e.o.SelfID != "" && len(e.o.SelfID) >= 12 && strings.HasPrefix(c.ID, e.o.SelfID) {
			log.Debug("skipping self")
			continue
		}
		if len(only) > 0 && !only[name] {
			continue
		}
		if !e.enabled(c.Labels) {
			log.Debug("skipping: not enabled by labels")
			continue
		}
		if c.Labels[compose.LabelOneOff] == "True" {
			log.Debug("skipping compose one-off container")
			continue
		}
		insp, err := e.o.Docker.ContainerInspect(ctx, c.ID)
		if err != nil {
			log.Warn("inspect failed", "error", err)
			continue
		}
		cand := &candidate{inspect: insp, name: name, monitorOnly: e.o.MonitorOnly || isTrue(c.Labels[LabelMonitorOnly]) || isTrue(c.Labels[LabelWatchtowerMonitorOnly])}
		if info, ok := compose.FromLabels(c.Labels); ok {
			cand.compose = &info
			spec, err := compose.LoadBuildSpec(info, e.o.PathMap)
			switch {
			case err == nil:
				cand.buildSpec = spec
			case errors.Is(err, compose.ErrNoBuild):
			default:
				log.Warn("cannot read compose build config; treating as image-only service", "error", err)
			}
		}
		out = append(out, cand)
	}
	return out, nil
}

func (e *Engine) enabled(labels map[string]string) bool {
	if v, ok := labels[LabelEnable]; ok {
		return isTrue(v)
	}
	if v, ok := labels[LabelWatchtowerEnable]; ok {
		return isTrue(v)
	}
	return !e.o.LabelEnable
}

func isTrue(s string) bool {
	return strings.EqualFold(s, "true") || s == "1" || strings.EqualFold(s, "yes")
}

func firstName(c container.Summary) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

var hexID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{12,64}$`)

// check compares every candidate with the registry and builds the plan.
func (e *Engine) check(ctx context.Context, cands []*candidate, res *Result) *plan {
	p := &plan{}
	rebuilds := map[string]*rebuild{}
	for _, c := range cands {
		log := e.log.With("container", c.name)
		img, err := e.o.Docker.ImageInspect(ctx, c.inspect.Image)
		if err != nil {
			log.Warn("image inspect failed", "image", c.inspect.Image, "error", err)
			continue
		}
		c.imageRef = c.inspect.Config.Image
		if hexID.MatchString(c.imageRef) {
			// Container was created from a raw image ID. Use its tag if it has
			// one; otherwise there is no tag to check against a registry.
			if len(img.RepoTags) == 0 {
				log.Debug("skipping: container runs an untagged image ID", "image", short(c.imageRef))
				continue
			}
			c.imageRef = img.RepoTags[0]
		}

		if c.buildSpec != nil {
			key := state.Key(c.compose.Project, c.compose.Service)
			rb, ok := rebuilds[key]
			if !ok {
				rb = &rebuild{key: key, info: *c.compose, spec: c.buildSpec, tag: c.imageRef, monitorOnly: c.monitorOnly}
				if err := e.checkBases(ctx, rb, log); err != nil {
					log.Warn("base image check failed", "error", err)
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", c.name, err))
					continue
				}
				rebuilds[key] = rb
				if len(rb.changed) > 0 {
					p.rebuilds = append(p.rebuilds, rb)
				}
			}
			rb.containers = append(rb.containers, c)
			rb.monitorOnly = rb.monitorOnly || c.monitorOnly
			continue
		}

		if len(img.RepoDigests) == 0 {
			log.Debug("skipping: local-only image (no repo digest) and no compose build config", "image", c.imageRef)
			continue
		}
		ref, err := registry.ParseImage(c.imageRef)
		if err != nil {
			log.Warn("skipping: cannot parse image reference", "image", c.imageRef, "error", err)
			continue
		}
		if ref.Digest != "" {
			log.Debug("skipping: image pinned by digest")
			continue
		}
		local := digestsFor(img.RepoDigests, ref)
		remote, err := e.o.Registry.Resolve(ctx, ref, local)
		if isLocalOnlyError(err, c.imageRef, img.RepoDigests) {
			log.Debug("skipping: image not in registry; treating as local-only", "image", c.imageRef, "error", err)
			continue
		}
		if err != nil {
			log.Warn("registry check failed", "image", ref.Familiar, "error", err)
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", c.name, err))
			continue
		}
		if remote.Matches(local) {
			log.Debug("up to date", "image", ref.Familiar, "digest", short(remote.Digest))
			continue
		}
		log.Info("update available", "image", ref.Familiar, "local", short(local[0]), "remote", short(remote.Digest))
		p.images = append(p.images, &imageUpdate{cand: c, img: ref, oldDigest: local[0], remote: remote})
	}
	return p
}

// checkBases determines whether any base image of a built service changed.
func (e *Engine) checkBases(ctx context.Context, rb *rebuild, log *slog.Logger) error {
	df, err := rb.spec.ReadDockerfile()
	if err != nil {
		return fmt.Errorf("read Dockerfile: %w", err)
	}
	bases, err := dockerfile.BaseImages(strings.NewReader(string(df)), rb.spec.Args)
	if err != nil {
		return fmt.Errorf("parse Dockerfile: %w", err)
	}
	recorded, _ := e.o.State.Get(rb.key)
	rb.newDigests = map[string]string{}
	rb.oldDigests = map[string]string{}
	firstObservation := false
	for _, base := range bases {
		ref, err := registry.ParseImage(base)
		if err != nil {
			log.Warn("cannot parse base image reference", "base", base, "error", err)
			continue
		}
		if ref.Digest != "" {
			continue // pinned
		}
		if hexID.MatchString(base) {
			log.Debug("ignoring base image given as a raw image ID", "base", short(base))
			continue
		}
		localImg, localErr := e.o.Docker.ImageInspect(ctx, base)
		localExists := localErr == nil
		if localExists && len(localImg.RepoDigests) == 0 {
			// Built or tagged locally (e.g. FROM mybase:local): nothing to
			// compare against a registry, so it never triggers a rebuild.
			log.Debug("ignoring local-only base image", "base", base)
			continue
		}
		var known []string
		if d := recorded.BaseDigests[base]; d != "" {
			known = []string{d}
		} else if localExists {
			known = digestsFor(localImg.RepoDigests, ref)
		}
		remote, err := e.o.Registry.Resolve(ctx, ref, known)
		if err != nil && localExists && isLocalOnlyError(err, base, localImg.RepoDigests) {
			log.Debug("ignoring base image not found in registry; treating as local-only", "base", base, "error", err)
			continue
		}
		if err != nil {
			return fmt.Errorf("resolve base %s: %w", ref.Familiar, err)
		}
		rb.newDigests[base] = remote.Digest
		switch {
		case len(known) == 0:
			// Nothing to compare against: assume the current build is
			// current and remember the digest for next time.
			log.Info("recording base image digest for first time", "base", ref.Familiar, "digest", short(remote.Digest))
			firstObservation = true
		case remote.Matches(known):
			rb.oldDigests[base] = known[0]
			rb.newDigests[base] = known[0]
			log.Debug("base image up to date", "base", ref.Familiar)
		default:
			rb.oldDigests[base] = known[0]
			rb.changed = append(rb.changed, base)
			log.Info("base image changed; rebuild needed", "base", ref.Familiar, "local", short(known[0]), "remote", short(remote.Digest))
		}
	}
	if firstObservation && len(rb.changed) == 0 {
		if err := e.o.State.Set(rb.key, state.ServiceState{BaseDigests: rb.newDigests}); err != nil {
			log.Warn("failed to save state", "error", err)
		}
	}
	return nil
}

// digestsFor extracts the digests from RepoDigests belonging to ref's
// repository (falling back to all digests).
func digestsFor(repoDigests []string, ref registry.Image) []string {
	var same, all []string
	for _, rd := range repoDigests {
		repo, d, ok := strings.Cut(rd, "@")
		if !ok {
			continue
		}
		all = append(all, d)
		if normRepo(repo) == normRepo(ref.RepoName()) {
			same = append(same, d)
		}
	}
	if len(same) > 0 {
		return same
	}
	return all
}

// isLocalOnlyError reports whether a registry error for ref most likely
// means the image only exists locally: the registry answered 401/404 (Docker
// Hub returns 401 for unknown repositories), the reference is a bare
// unqualified name (no registry host) and the local image carries no repo
// digest, i.e. it was never pulled from anywhere.
func isLocalOnlyError(err error, ref string, repoDigests []string) bool {
	var he *registry.HTTPError
	if !errors.As(err, &he) || (he.Status != 401 && he.Status != 404) {
		return false
	}
	if len(repoDigests) > 0 {
		return false
	}
	name := ref
	if i := strings.LastIndex(name, "@"); i >= 0 {
		name = name[:i]
	}
	first, _, hasPath := strings.Cut(name, "/")
	if !hasPath {
		return true // bare name such as rcdev:local
	}
	// A registry host contains a dot or a port, or is "localhost".
	return !strings.ContainsAny(first, ".:") && first != "localhost"
}

func normRepo(r string) string {
	if img, err := registry.ParseImage(r); err == nil {
		return img.Registry + "/" + img.Repository
	}
	return r
}

func short(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// execute applies the plan.
func (e *Engine) execute(ctx context.Context, p *plan, res *Result) {
	apply := !e.o.DryRun && !e.o.MonitorOnly
	for _, u := range p.images {
		item := Item{Container: u.cand.name, Kind: strategy.KindImage, Image: u.img.Familiar, OldDigest: u.oldDigest, NewDigest: u.remote.Digest}
		if u.cand.compose != nil {
			item.Project, item.Service = u.cand.compose.Project, u.cand.compose.Service
		}
		if !apply || u.cand.monitorOnly {
			e.log.Info("update available (not applied)", "container", u.cand.name, "image", u.img.Familiar, "dry_run", e.o.DryRun, "monitor_only", u.cand.monitorOnly)
			res.Pending = append(res.Pending, item)
			continue
		}
		if err := e.updateImage(ctx, u); err != nil {
			e.log.Error("update failed", "container", u.cand.name, "error", err)
			item.Error = err.Error()
			res.Failed = append(res.Failed, item)
			continue
		}
		e.log.Info("container updated", "container", u.cand.name, "image", u.img.Familiar)
		res.Updated = append(res.Updated, item)
	}
	for _, rb := range p.rebuilds {
		items := make([]Item, 0, len(rb.containers))
		for _, c := range rb.containers {
			items = append(items, Item{Container: c.name, Kind: strategy.KindRebuild, Image: rb.tag, Project: rb.info.Project, Service: rb.info.Service, BaseImages: rb.changed})
		}
		if !apply || rb.monitorOnly {
			e.log.Info("rebuild available (not applied)", "project", rb.info.Project, "service", rb.info.Service, "bases", rb.changed, "dry_run", e.o.DryRun, "monitor_only", rb.monitorOnly)
			res.Pending = append(res.Pending, items...)
			continue
		}
		failed, err := e.applyRebuild(ctx, rb)
		for i, c := range rb.containers {
			if msg, ok := failed[c.name]; ok {
				items[i].Error = msg
				res.Failed = append(res.Failed, items[i])
			} else if err != nil {
				items[i].Error = err.Error()
				res.Failed = append(res.Failed, items[i])
			} else {
				res.Updated = append(res.Updated, items[i])
			}
		}
	}
}

func (e *Engine) updateImage(ctx context.Context, u *imageUpdate) error {
	if !e.o.Strategy.HandlesPull() {
		if err := e.pull(ctx, u.img); err != nil {
			return err
		}
	}
	t := &strategy.Target{
		Kind:          strategy.KindImage,
		ContainerID:   u.cand.inspect.ID,
		ContainerName: u.cand.name,
		Image:         u.cand.imageRef,
		OldImageID:    u.cand.inspect.Image,
		Compose:       u.cand.compose,
		Inspect:       u.cand.inspect,
	}
	if err := e.o.Strategy.Update(ctx, t); err != nil {
		return fmt.Errorf("%s strategy: %w", e.o.Strategy.Name(), err)
	}
	e.cleanup(ctx, u.cand.inspect.Image)
	return nil
}

// applyRebuild pulls changed bases, rebuilds once and recreates each
// container. Returns per-container failures plus a shared error for the
// build phase.
func (e *Engine) applyRebuild(ctx context.Context, rb *rebuild) (map[string]string, error) {
	log := e.log.With("project", rb.info.Project, "service", rb.info.Service)
	if !e.o.Strategy.HandlesPull() {
		for _, base := range rb.changed {
			ref, err := registry.ParseImage(base)
			if err != nil {
				return nil, err
			}
			if err := e.pull(ctx, ref); err != nil {
				return nil, err
			}
		}
	}
	if !e.o.Strategy.HandlesBuild() {
		if e.o.Builder == nil {
			return nil, errors.New("no builder configured for rebuilding compose services")
		}
		req := build.Request{
			Info:       rb.info,
			Spec:       rb.spec,
			Tag:        rb.tag,
			Pull:       e.o.BuildPull,
			NoCache:    e.o.BuildNoCache,
			BaseImages: rb.changed,
			Labels:     map[string]string{"com.dockwarden.base-digests": formatDigests(rb.newDigests)},
		}
		if err := e.o.Builder.Build(ctx, req); err != nil {
			log.Error("rebuild failed", "error", err)
			return nil, fmt.Errorf("%s build: %w", e.o.Builder.Name(), err)
		}
		log.Info("service rebuilt", "tag", rb.tag, "bases", rb.changed)
	}
	failed := map[string]string{}
	for _, c := range rb.containers {
		t := &strategy.Target{
			Kind:          strategy.KindRebuild,
			ContainerID:   c.inspect.ID,
			ContainerName: c.name,
			Image:         c.imageRef,
			OldImageID:    c.inspect.Image,
			Compose:       c.compose,
			Build:         rb.spec,
			BaseImages:    rb.changed,
			Inspect:       c.inspect,
		}
		if err := e.o.Strategy.Update(ctx, t); err != nil {
			log.Error("recreate failed", "container", c.name, "error", err)
			failed[c.name] = fmt.Sprintf("%s strategy: %v", e.o.Strategy.Name(), err)
			continue
		}
		log.Info("container recreated after rebuild", "container", c.name)
		e.cleanup(ctx, c.inspect.Image)
	}
	if len(failed) < len(rb.containers) {
		if err := e.o.State.Set(rb.key, state.ServiceState{BaseDigests: rb.newDigests}); err != nil {
			log.Warn("failed to save state", "error", err)
		}
	}
	return failed, nil
}

func formatDigests(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"@"+m[k])
	}
	return strings.Join(parts, ",")
}

func (e *Engine) pull(ctx context.Context, img registry.Image) error {
	ref := img.Familiar
	e.log.Info("pulling image", "image", ref)
	creds, err := e.o.Credentials.Get(img.Registry)
	if err != nil {
		return fmt.Errorf("credentials for %s: %w", img.Registry, err)
	}
	server := img.Registry
	if server == "registry-1.docker.io" {
		server = "https://index.docker.io/v1/"
	}
	rc, err := e.o.Docker.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: registry.EncodeDockerAuth(creds, server)})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	if err := dockerapi.DrainJSONStream(rc, func(m dockerapi.JSONMessage) {
		if m.Status != "" {
			e.log.Debug(m.Status, "image", ref)
		}
	}); err != nil {
		io.Copy(io.Discard, rc) //nolint:errcheck
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

func (e *Engine) cleanup(ctx context.Context, oldImageID string) {
	if !e.o.Cleanup || oldImageID == "" {
		return
	}
	if _, err := e.o.Docker.ImageRemove(ctx, oldImageID, image.RemoveOptions{}); err != nil {
		e.log.Debug("old image not removed", "image", short(oldImageID), "error", err)
		return
	}
	e.log.Info("removed old image", "image", short(oldImageID))
}

func (e *Engine) notify(ctx context.Context, res *Result) {
	if e.o.Notifier == nil {
		return
	}
	host, _ := os.Hostname()
	rep := &notify.Report{
		Host:        host,
		StartedAt:   res.StartedAt,
		Duration:    res.Duration.Round(time.Millisecond).String(),
		DryRun:      e.o.DryRun,
		MonitorOnly: e.o.MonitorOnly,
		Title:       "dockwarden: " + res.Summary(),
		Summary:     res.Summary(),
		Scanned:     res.Scanned,
		Errors:      res.Errors,
	}
	rep.Updated = toNotifyItems(res.Updated)
	rep.Pending = toNotifyItems(res.Pending)
	rep.Failed = toNotifyItems(res.Failed)
	if err := e.o.Notifier.Send(ctx, rep); err != nil {
		e.log.Warn("notification failed", "error", err)
	}
}

func toNotifyItems(items []Item) []notify.Item {
	out := make([]notify.Item, 0, len(items))
	for _, it := range items {
		out = append(out, notify.Item{Container: it.Container, Kind: string(it.Kind), Image: it.Image, Project: it.Project, Service: it.Service, OldDigest: it.OldDigest, NewDigest: it.NewDigest, Bases: it.BaseImages, Error: it.Error})
	}
	return out
}
