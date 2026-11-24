package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dansimau/dockwarden/internal/build"
	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/dockerapi/fake"
	"github.com/dansimau/dockwarden/internal/registry"
	"github.com/dansimau/dockwarden/internal/state"
	"github.com/dansimau/dockwarden/internal/strategy"
)

type fakeResolver struct {
	remote map[string]registry.Remote // familiar ref -> remote
	err    map[string]error
	calls  []string
}

func (f *fakeResolver) Resolve(ctx context.Context, img registry.Image, local []string) (registry.Remote, error) {
	f.calls = append(f.calls, img.Familiar)
	if err := f.err[img.Familiar]; err != nil {
		return registry.Remote{}, err
	}
	r, ok := f.remote[img.Familiar]
	if !ok {
		return registry.Remote{}, errors.New("unknown image " + img.Familiar)
	}
	return r, nil
}

type recStrategy struct {
	targets      []*strategy.Target
	err          error
	handlesBuild bool
	begun, ended int
}

func (s *recStrategy) Name() string       { return "rec" }
func (s *recStrategy) HandlesBuild() bool { return s.handlesBuild }
func (s *recStrategy) HandlesPull() bool  { return false }
func (s *recStrategy) Update(ctx context.Context, t *strategy.Target) error {
	s.targets = append(s.targets, t)
	return s.err
}
func (s *recStrategy) BeginRun(ctx context.Context) error { s.begun++; return nil }
func (s *recStrategy) EndRun(ctx context.Context) error   { s.ended++; return nil }

type recBuilder struct {
	reqs []build.Request
	err  error
}

func (b *recBuilder) Name() string { return "rec" }
func (b *recBuilder) Build(ctx context.Context, r build.Request) error {
	b.reqs = append(b.reqs, r)
	return b.err
}

func setup(t *testing.T) (*fake.Client, *fakeResolver, *recStrategy, *recBuilder, Options) {
	fc := fake.New()
	fc.AddImage(fake.Image{ID: "sha256:nginxold", RepoTags: []string{"nginx:1"}, RepoDigests: []string{"nginx@sha256:old"}})
	fc.AddImage(fake.Image{ID: "sha256:redis", RepoTags: []string{"redis:7"}, RepoDigests: []string{"redis@sha256:same"}})
	fc.AddContainer("c1", "web", "nginx:1", "sha256:nginxold", nil)
	fc.AddContainer("c2", "cache", "redis:7", "sha256:redis", nil)
	fc.PullHook = func(ref string) error {
		fc.AddImage(fake.Image{ID: "sha256:nginxnew", RepoTags: []string{"nginx:1"}, RepoDigests: []string{"nginx@sha256:new"}})
		return nil
	}
	res := &fakeResolver{remote: map[string]registry.Remote{
		"nginx:1": {Digest: "sha256:new", IsIndex: true, Children: []string{"sha256:new-amd64"}},
		"redis:7": {Digest: "sha256:same"},
	}}
	st, _ := state.Load(filepath.Join(t.TempDir(), "state.json"))
	strat := &recStrategy{}
	bld := &recBuilder{}
	opts := Options{Docker: fc, Registry: res, State: st, Strategy: strat, Builder: bld, Logger: slog.New(slog.DiscardHandler), BuildPull: true, SelfID: "0123456789abcdef"}
	return fc, res, strat, bld, opts
}

func TestRunUpdatesImage(t *testing.T) {
	fc, _, strat, _, opts := setup(t)
	opts.Cleanup = true
	r, err := New(opts).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Scanned != 2 || len(r.Updated) != 1 || len(r.Pending) != 0 || len(r.Failed) != 0 {
		t.Fatalf("result %+v", r)
	}
	if r.Updated[0].Container != "web" || r.Updated[0].NewDigest != "sha256:new" || r.Updated[0].OldDigest != "sha256:old" {
		t.Fatalf("item %+v", r.Updated[0])
	}
	if len(strat.targets) != 1 || strat.targets[0].Image != "nginx:1" || strat.targets[0].Kind != strategy.KindImage {
		t.Fatalf("targets %+v", strat.targets)
	}
	calls := strings.Join(fc.Calls, ",")
	if !strings.Contains(calls, "ImagePull nginx:1") || !strings.Contains(calls, "ImageRemove sha256:nginxold") {
		t.Fatalf("calls %s", calls)
	}
	if strat.begun != 1 || strat.ended != 1 {
		t.Fatal("run scope hooks not called")
	}
	if r.ExitCode(3) != 0 {
		t.Fatal("exit code")
	}
}

func TestRunDryRunAndMonitorOnly(t *testing.T) {
	fc, _, strat, _, opts := setup(t)
	opts.DryRun = true
	r, _ := New(opts).Run(context.Background())
	if len(r.Pending) != 1 || len(strat.targets) != 0 || len(fc.CallsMatching("ImagePull")) != 0 {
		t.Fatalf("dry run should not act: %+v", r)
	}
	if r.ExitCode(3) != 3 || r.ExitCode(0) != 0 {
		t.Fatal("exit code for pending updates")
	}
	// per-container monitor-only label
	fc, _, strat, _, opts = setup(t)
	fc.Containers["c1"].Config.Labels[LabelMonitorOnly] = "true"
	r, _ = New(opts).Run(context.Background())
	if len(r.Pending) != 1 || len(strat.targets) != 0 {
		t.Fatalf("monitor-only label ignored: %+v", r)
	}
}

func TestRunLabelFiltering(t *testing.T) {
	fc, _, strat, _, opts := setup(t)
	opts.LabelEnable = true
	r, _ := New(opts).Run(context.Background())
	if r.Scanned != 0 {
		t.Fatalf("opt-in mode should skip unlabelled containers: %+v", r)
	}
	fc.Containers["c1"].Config.Labels[LabelEnable] = "true"
	r, _ = New(opts).Run(context.Background())
	if r.Scanned != 1 || len(strat.targets) != 1 {
		t.Fatalf("labelled container not updated: %+v", r)
	}
	// opt-out via label in default mode
	fc, _, strat, _, opts = setup(t)
	fc.Containers["c1"].Config.Labels[LabelWatchtowerEnable] = "false"
	r, _ = New(opts).Run(context.Background())
	if r.Scanned != 1 || len(strat.targets) != 0 {
		t.Fatalf("opt-out ignored: %+v", r)
	}
	// explicit container list
	fc, _, _, _, opts = setup(t)
	opts.Containers = []string{"cache"}
	r, _ = New(opts).Run(context.Background())
	if r.Scanned != 1 || len(r.Updated) != 0 {
		t.Fatalf("container filter: %+v", r)
	}
	// self is skipped
	fc, _, _, _, opts = setup(t)
	fc.AddContainer("0123456789abcdef0000", "dockwarden", "nginx:1", "sha256:nginxold", nil)
	r, _ = New(opts).Run(context.Background())
	if r.Scanned != 2 {
		t.Fatalf("self not skipped: %+v", r)
	}
}

func TestRunStrategyFailure(t *testing.T) {
	_, _, strat, _, opts := setup(t)
	strat.err = errors.New("boom")
	r, _ := New(opts).Run(context.Background())
	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0].Error, "boom") || r.ExitCode(0) != 2 {
		t.Fatalf("result %+v", r)
	}
}

func TestRunRegistryError(t *testing.T) {
	_, res, _, _, opts := setup(t)
	res.err = map[string]error{"nginx:1": errors.New("registry down")}
	r, _ := New(opts).Run(context.Background())
	if len(r.Errors) != 1 || r.ExitCode(0) != 2 {
		t.Fatalf("result %+v", r)
	}
}

func TestRunPullFailure(t *testing.T) {
	fc, _, strat, _, opts := setup(t)
	fc.PullHook = func(ref string) error { return errors.New("pull denied") }
	r, _ := New(opts).Run(context.Background())
	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0].Error, "pull denied") || len(strat.targets) != 0 {
		t.Fatalf("result %+v", r)
	}
}

func TestRunSkipsPinnedAndLocalImages(t *testing.T) {
	fc, res, _, _, opts := setup(t)
	fc.AddImage(fake.Image{ID: "sha256:local", RepoTags: []string{"mybuild:latest"}})
	fc.AddContainer("c3", "local", "mybuild:latest", "sha256:local", nil)
	fc.AddImage(fake.Image{ID: "sha256:pinned", RepoTags: []string{}, RepoDigests: []string{"alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}})
	fc.AddContainer("c4", "pinned", "alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256:pinned", nil)
	r, _ := New(opts).Run(context.Background())
	if r.Scanned != 4 || len(r.Updated) != 1 || len(r.Errors) != 0 {
		t.Fatalf("result %+v", r)
	}
	for _, c := range res.calls {
		if c != "nginx:1" && c != "redis:7" {
			t.Fatalf("unexpected registry call %s", c)
		}
	}
}

// composeProject writes a compose project with a build service and returns its dir.
func composeProject(t *testing.T, dockerfile string) string {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n  app:\n    build: .\n  db:\n    image: redis:7\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600)
	return dir
}

func composeLabels(dir, service string) map[string]string {
	return map[string]string{
		compose.LabelProject: "proj", compose.LabelService: service, compose.LabelWorkingDir: dir,
		compose.LabelConfigFiles: filepath.Join(dir, "docker-compose.yml"),
	}
}

func TestRunRebuild(t *testing.T) {
	fc, res, strat, bld, opts := setup(t)
	dir := composeProject(t, "FROM alpine:3.19 AS build\nFROM alpine:3.19\n")
	fc.AddImage(fake.Image{ID: "sha256:alpine", RepoTags: []string{"alpine:3.19"}, RepoDigests: []string{"alpine@sha256:alpineold"}})
	fc.AddImage(fake.Image{ID: "sha256:app", RepoTags: []string{"proj-app:latest"}})
	fc.AddContainer("a1", "proj-app-1", "proj-app", "sha256:app", composeLabels(dir, "app"))
	fc.AddContainer("a2", "proj-app-2", "proj-app", "sha256:app", composeLabels(dir, "app"))
	fc.Containers["c2"].Config.Labels = composeLabels(dir, "db") // image-only compose service
	res.remote["alpine:3.19"] = registry.Remote{Digest: "sha256:alpinenew"}

	r, err := New(opts).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Updated) != 3 || len(r.Failed) != 0 || len(r.Errors) != 0 {
		t.Fatalf("result %+v", r)
	}
	if len(bld.reqs) != 1 {
		t.Fatalf("expected one build, got %d", len(bld.reqs))
	}
	req := bld.reqs[0]
	if req.Tag != "proj-app" || req.Info.Service != "app" || req.Spec.Context != dir || !req.Pull || req.BaseImages[0] != "alpine:3.19" {
		t.Fatalf("build req %+v", req)
	}
	if !strings.Contains(req.Labels["com.dockwarden.base-digests"], "alpine:3.19@sha256:alpinenew") {
		t.Fatalf("labels %v", req.Labels)
	}
	if len(fc.CallsMatching("ImagePull alpine:3.19")) != 1 {
		t.Fatalf("base not pulled once: %v", fc.Calls)
	}
	rebuilt := 0
	for _, tg := range strat.targets {
		if tg.Kind == strategy.KindRebuild {
			rebuilt++
			if tg.Build == nil || tg.Compose == nil || tg.Compose.Service != "app" {
				t.Fatalf("target %+v", tg)
			}
		}
	}
	if rebuilt != 2 {
		t.Fatalf("expected 2 rebuild targets, got %d", rebuilt)
	}
	s, ok := opts.State.Get("proj/app")
	if !ok || s.BaseDigests["alpine:3.19"] != "sha256:alpinenew" {
		t.Fatalf("state %+v %v", s, ok)
	}
	// Second run: state says we're current -> nothing to do.
	bld.reqs = nil
	r, _ = New(opts).Run(context.Background())
	if len(r.Updated) != 1 || len(bld.reqs) != 0 { // nginx still "updates" because the fake pull hook re-adds old digest container
		t.Fatalf("second run %+v builds=%d", r, len(bld.reqs))
	}
}

func TestRunRebuildFirstObservationRecordsState(t *testing.T) {
	fc, res, _, bld, opts := setup(t)
	dir := composeProject(t, "FROM busybox:stable\n")
	// base image not present locally and no state: record remote digest, no rebuild
	fc.AddImage(fake.Image{ID: "sha256:app", RepoTags: []string{"proj-app:latest"}})
	fc.AddContainer("a1", "proj-app-1", "proj-app", "sha256:app", composeLabels(dir, "app"))
	res.remote["busybox:stable"] = registry.Remote{Digest: "sha256:bb1"}
	r, _ := New(opts).Run(context.Background())
	if len(bld.reqs) != 0 || len(r.Errors) != 0 {
		t.Fatalf("unexpected build/errors: %+v", r)
	}
	s, ok := opts.State.Get("proj/app")
	if !ok || s.BaseDigests["busybox:stable"] != "sha256:bb1" {
		t.Fatalf("state not recorded: %+v", s)
	}
	// Now the base changes upstream -> rebuild.
	res.remote["busybox:stable"] = registry.Remote{Digest: "sha256:bb2"}
	r, _ = New(opts).Run(context.Background())
	if len(bld.reqs) != 1 || len(r.Failed) != 0 {
		t.Fatalf("expected rebuild: %+v", r)
	}
}

func TestRunRebuildStrategyHandlesBuild(t *testing.T) {
	fc, res, strat, bld, opts := setup(t)
	strat.handlesBuild = true
	opts.Builder = nil
	dir := composeProject(t, "FROM busybox:stable\n")
	fc.AddImage(fake.Image{ID: "sha256:bb", RepoTags: []string{"busybox:stable"}, RepoDigests: []string{"busybox@sha256:bb1"}})
	fc.AddImage(fake.Image{ID: "sha256:app", RepoTags: []string{"proj-app:latest"}})
	fc.AddContainer("a1", "proj-app-1", "proj-app", "sha256:app", composeLabels(dir, "app"))
	res.remote["busybox:stable"] = registry.Remote{Digest: "sha256:bb2"}
	r, _ := New(opts).Run(context.Background())
	if len(bld.reqs) != 0 || len(r.Failed) != 0 || len(r.Updated) != 2 {
		t.Fatalf("result %+v", r)
	}
	if len(fc.CallsMatching("ImagePull busybox:stable")) != 1 {
		t.Fatal("base should still be pulled")
	}
}

func TestRunRebuildBuildFailure(t *testing.T) {
	fc, res, _, bld, opts := setup(t)
	bld.err = errors.New("compile error")
	dir := composeProject(t, "FROM busybox:stable\n")
	fc.AddImage(fake.Image{ID: "sha256:bb", RepoTags: []string{"busybox:stable"}, RepoDigests: []string{"busybox@sha256:bb1"}})
	fc.AddImage(fake.Image{ID: "sha256:app", RepoTags: []string{"proj-app:latest"}})
	fc.AddContainer("a1", "proj-app-1", "proj-app", "sha256:app", composeLabels(dir, "app"))
	res.remote["busybox:stable"] = registry.Remote{Digest: "sha256:bb2"}
	r, _ := New(opts).Run(context.Background())
	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0].Error, "compile error") {
		t.Fatalf("result %+v", r)
	}
	if _, ok := opts.State.Get("proj/app"); ok {
		t.Fatal("state must not be updated after a failed build")
	}
}

func TestRunDockerDown(t *testing.T) {
	_, _, _, _, opts := setup(t)
	opts.Docker.(*fake.Client).Fail["Ping"] = errors.New("no socket")
	if _, err := New(opts).Run(context.Background()); err == nil {
		t.Fatal("expected fatal error")
	}
}

func TestDigestsFor(t *testing.T) {
	ref, _ := registry.ParseImage("ghcr.io/org/app:1")
	got := digestsFor([]string{"other@sha256:x", "ghcr.io/org/app@sha256:y"}, ref)
	if len(got) != 1 || got[0] != "sha256:y" {
		t.Fatalf("got %v", got)
	}
	ref, _ = registry.ParseImage("nginx")
	got = digestsFor([]string{"docker.io/library/nginx@sha256:z"}, ref)
	if len(got) != 1 || got[0] != "sha256:z" {
		t.Fatalf("got %v", got)
	}
}

func TestLocalOnlyContainerImagesAreSkipped(t *testing.T) {
	fc, res, strat, _, opts := setup(t)
	// Locally built/tagged image with no RepoDigests: never contacts a registry.
	fc.AddImage(fake.Image{ID: "sha256:rcdev", RepoTags: []string{"rcdev:local"}})
	fc.AddContainer("l1", "rcdev", "rcdev:local", "sha256:rcdev", nil)
	// Image with a tag but without digests whose registry lookup would 401.
	fc.AddImage(fake.Image{ID: "sha256:priv", RepoTags: []string{"internal-tool:1.0"}})
	fc.AddContainer("l2", "internal-tool", "internal-tool:1.0", "sha256:priv", nil)
	res.err = map[string]error{"internal-tool:1.0": &registry.HTTPError{Status: 401, Image: "internal-tool:1.0"}}
	r, err := New(opts).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Errors) != 0 || len(r.Failed) != 0 {
		t.Fatalf("local-only images must not produce errors: %+v", r)
	}
	for _, c := range res.calls {
		if c == "rcdev:local" || c == "internal-tool:1.0" {
			t.Fatalf("registry must not be asked about local-only image %s", c)
		}
	}
	if len(strat.targets) != 1 || strat.targets[0].ContainerName != "web" {
		t.Fatalf("only web should update: %+v", strat.targets)
	}
}

func TestLocalOnlyErrorFallback(t *testing.T) {
	fc, res, _, _, opts := setup(t)
	// Image that *was* pulled once (has a digest) but is now unknown to the
	// registry: that is a real problem and must still be reported.
	fc.AddImage(fake.Image{ID: "sha256:gone", RepoTags: []string{"gone:1"}, RepoDigests: []string{"gone@sha256:g"}})
	fc.AddContainer("g1", "gone", "gone:1", "sha256:gone", nil)
	res.err = map[string]error{"gone:1": &registry.HTTPError{Status: 404, Image: "gone:1"}}
	r, _ := New(opts).Run(context.Background())
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "gone") {
		t.Fatalf("expected error for previously pulled image: %+v", r)
	}
}

func TestIsLocalOnlyError(t *testing.T) {
	e401 := &registry.HTTPError{Status: 401}
	cases := []struct {
		err     error
		ref     string
		digests []string
		want    bool
	}{
		{e401, "rcdev:local", nil, true},
		{&registry.HTTPError{Status: 404}, "mytool", nil, true},
		{e401, "ghcr.io/org/app:1", nil, false},              // qualified: a real auth problem
		{e401, "localhost:5000/x:1", nil, false},             // local registry host
		{e401, "rcdev:local", []string{"r@sha256:x"}, false}, // was pulled before
		{&registry.HTTPError{Status: 500}, "rcdev:local", nil, false},
		{errors.New("timeout"), "rcdev:local", nil, false},
		{nil, "rcdev:local", nil, false},
	}
	for _, c := range cases {
		if got := isLocalOnlyError(c.err, c.ref, c.digests); got != c.want {
			t.Errorf("isLocalOnlyError(%v, %q, %v) = %v, want %v", c.err, c.ref, c.digests, got, c.want)
		}
	}
}

func TestUntaggedImageIDContainersAreSkipped(t *testing.T) {
	fc, res, strat, _, opts := setup(t)
	const id = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// dawarich-style: created from a raw image ID; image has digests but no tags.
	fc.AddImage(fake.Image{ID: id, RepoDigests: []string{"freikin/dawarich@sha256:d"}})
	fc.AddContainer("d1", "dawarich_app", id, id, nil)
	// raw ID with a tag available: the tag is checked instead.
	const redisID = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	fc.AddImage(fake.Image{ID: redisID, RepoTags: []string{"redis:7"}, RepoDigests: []string{"redis@sha256:same"}})
	fc.AddContainer("d2", "by-id-with-tag", redisID, redisID, nil)
	r, err := New(opts).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", r.Errors)
	}
	for _, c := range res.calls {
		if strings.Contains(c, "sha256") {
			t.Fatalf("registry asked about raw image id: %s", c)
		}
	}
	if len(strat.targets) != 1 || strat.targets[0].ContainerName != "web" {
		t.Fatalf("targets %+v", strat.targets)
	}
}

func TestRebuildIgnoresLocalOnlyBase(t *testing.T) {
	fc, res, _, bld, opts := setup(t)
	dir := composeProject(t, "FROM rcdev:local AS tools\nFROM busybox:stable\nCOPY --from=tools /x /x\n")
	fc.AddImage(fake.Image{ID: "sha256:rcdev", RepoTags: []string{"rcdev:local"}}) // local-only base
	fc.AddImage(fake.Image{ID: "sha256:bb", RepoTags: []string{"busybox:stable"}, RepoDigests: []string{"busybox@sha256:bb1"}})
	fc.AddImage(fake.Image{ID: "sha256:app", RepoTags: []string{"proj-app:latest"}})
	fc.AddContainer("a1", "proj-app-1", "proj-app", "sha256:app", composeLabels(dir, "app"))
	res.remote["busybox:stable"] = registry.Remote{Digest: "sha256:bb2"}
	res.err = map[string]error{"rcdev:local": &registry.HTTPError{Status: 401, Image: "rcdev:local"}}
	r, err := New(opts).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Errors) != 0 || len(r.Failed) != 0 {
		t.Fatalf("local-only base must not fail the check: %+v", r)
	}
	for _, c := range res.calls {
		if c == "rcdev:local" {
			t.Fatal("registry must not be asked about local-only base")
		}
	}
	if len(bld.reqs) != 1 || bld.reqs[0].BaseImages[0] != "busybox:stable" {
		t.Fatalf("expected rebuild for busybox only: %+v", bld.reqs)
	}
	s, _ := opts.State.Get("proj/app")
	if _, ok := s.BaseDigests["rcdev:local"]; ok {
		t.Fatal("local-only base must not be recorded in state")
	}
}

func TestRebuildBaseUnknownToRegistryButLocal(t *testing.T) {
	fc, res, _, bld, opts := setup(t)
	// Base exists locally (pulled from nowhere: no digests) but is referenced
	// under a name the registry rejects with 404: treated as local-only.
	dir := composeProject(t, "FROM mybase\n")
	fc.AddImage(fake.Image{ID: "sha256:mybase", RepoTags: []string{"mybase:latest"}})
	fc.AddImage(fake.Image{ID: "sha256:app", RepoTags: []string{"proj-app:latest"}})
	fc.AddContainer("a1", "proj-app-1", "proj-app", "sha256:app", composeLabels(dir, "app"))
	res.err = map[string]error{"mybase:latest": &registry.HTTPError{Status: 404, Image: "mybase:latest"}}
	r, _ := New(opts).Run(context.Background())
	if len(r.Errors) != 0 || len(bld.reqs) != 0 {
		t.Fatalf("result %+v builds=%d", r, len(bld.reqs))
	}
	// Base missing locally AND unknown to the registry is still an error.
	fc2, res2, _, _, opts2 := setup(t)
	dir2 := composeProject(t, "FROM typo-image:1\n")
	fc2.AddImage(fake.Image{ID: "sha256:app", RepoTags: []string{"proj-app:latest"}})
	fc2.AddContainer("a1", "proj-app-1", "proj-app", "sha256:app", composeLabels(dir2, "app"))
	res2.err = map[string]error{"typo-image:1": &registry.HTTPError{Status: 401, Image: "typo-image:1"}}
	r2, _ := New(opts2).Run(context.Background())
	if len(r2.Errors) != 1 {
		t.Fatalf("expected error for missing base: %+v", r2)
	}
}

func TestIsLocalOnlyErrorPaths(t *testing.T) {
	e := &registry.HTTPError{Status: 401}
	if !isLocalOnlyError(e, "myorg/tool:1", nil) {
		t.Fatal("docker hub style user/repo without host is unqualified")
	}
	if isLocalOnlyError(e, "registry.lan:5000/tool:1", nil) {
		t.Fatal("host with port is qualified")
	}
}
