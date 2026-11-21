# dockwarden

A small, single-binary replacement for [Watchtower](https://containrrr.dev/watchtower/)
that also understands **docker compose services built from local Dockerfiles**
and can hand restarts off to **Synology Container Manager**.

Run it once a day from cron / DSM Task Scheduler (one-shot mode) or let it
loop with `--interval 24h`.

What it does on each run:

1. **Image updates** – for every running container (opt-in/opt-out via
   labels and flags) it asks the registry what the image tag currently
   points to and compares that digest with the local `RepoDigests`.
   Multi-arch manifest lists are handled (the index digest *and* the
   platform manifest digests are accepted), registry credentials come from
   `~/.docker/config.json` (including `credHelpers`/`credsStore`). Newer
   image ⇒ pull ⇒ recreate.
2. **Rebuilds** – containers that belong to a compose project (detected
   from the `com.docker.compose.*` labels) whose service has a `build:`
   section are handled differently: dockwarden reads the compose file(s),
   finds the Dockerfile, extracts the `FROM` base images (multi-stage, ARGs,
   `scratch` and stage references are understood) and checks whether any
   base image has a newer digest than the one the service was last built
   from (recorded in a small state file). If so it pulls the base, rebuilds
   the service and recreates its containers.
3. **Restart strategy** – how a container is recreated is pluggable:
   * `docker` (default): recreate natively through the Docker API,
     preserving the container's config like Watchtower, or
     `docker compose up -d <service>` for compose containers when the CLI
     is available.
   * `command`: run any command template (e.g. `ssh host 'docker compose
     up -d ...'`) as an escape hatch.
   * `synology`: ask DSM Container Manager to rebuild/start the project or
     recreate the container through its Web API, so the DSM UI stays the
     owner of your projects.

Plus: YAML config with env-var and flag overrides, dry-run and monitor-only
modes, structured (text/JSON) logs, a generic webhook notification, exit
codes suitable for schedulers, and unit tests with a fake Docker client and
`httptest` registries / DSM.

## Quick start

```sh
# one-shot dry run against the local daemon
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/dansimau/dockwarden --dry-run

# daily loop as a compose service
cp docker-compose.example.yml docker-compose.yml   # edit mounts
cp examples/config.yaml config.yaml                # edit settings
docker compose up -d
```

Or build the binary: `make build` (Go 1.26) → `bin/dockwarden`.

Release binaries (linux amd64/arm64/armv7, macOS arm64) are attached to
GitHub releases, and the image is published to
`ghcr.io/dansimau/dockwarden` (`latest`, `main`, semver tags).

## Configuration

`dockwarden --config config.yaml`. All keys, with defaults, are documented in
[`examples/config.yaml`](examples/config.yaml). Precedence: flags > `DOCKWARDEN_*`
environment variables > YAML > defaults.

Common environment overrides:

| Variable | YAML key |
|---|---|
| `DOCKWARDEN_CONFIG` | path to the config file |
| `DOCKWARDEN_DRY_RUN`, `DOCKWARDEN_MONITOR_ONLY`, `DOCKWARDEN_LABEL_ENABLE` | `dry_run`, `monitor_only`, `label_enable` |
| `DOCKWARDEN_INTERVAL` | `interval` (e.g. `24h`) |
| `DOCKWARDEN_CONTAINERS` | `containers` (comma separated) |
| `DOCKWARDEN_STATE_FILE`, `DOCKWARDEN_CLEANUP` | `state_file`, `cleanup` |
| `DOCKWARDEN_LOG_LEVEL`, `DOCKWARDEN_LOG_FORMAT` | `log.level`, `log.format` |
| `DOCKWARDEN_BUILD_MODE`, `DOCKWARDEN_BUILD_COMMAND`, `DOCKWARDEN_BUILD_PULL` | `build.*` |
| `DOCKWARDEN_STRATEGY` | `strategy.type` |
| `DOCKWARDEN_STRATEGY_COMMAND_TEMPLATE`, `..._HANDLES_BUILD`, `..._HANDLES_PULL` | `strategy.command.*` |
| `DOCKWARDEN_SYNOLOGY_URL`, `DOCKWARDEN_SYNOLOGY_USERNAME[_FILE]`, `DOCKWARDEN_SYNOLOGY_PASSWORD[_FILE]`, `DOCKWARDEN_SYNOLOGY_INSECURE_SKIP_VERIFY` | `strategy.synology.*` |
| `DOCKWARDEN_NOTIFY_WEBHOOK_URL`, `..._METHOD`, `..._ON`, `..._TEMPLATE` | `notify.webhook.*` |
| `DOCKWARDEN_EXIT_CODE_ON_UPDATES` | `exit_code_on_updates` |
| `DOCKER_HOST`, `DOCKER_CONFIG` | standard Docker variables are honoured |

Secrets (DSM password) should come from environment variables or
`*_file` keys pointing at Docker secrets — never commit them in the YAML.

### Container selection (labels)

| Label | Effect |
|---|---|
| `com.dockwarden.enable=false` | skip this container (default mode is opt-out) |
| `com.dockwarden.enable=true` | include this container when `label_enable: true` (opt-in mode) |
| `com.dockwarden.monitor-only=true` | report updates for this container but never apply them |
| `com.centurylinklabs.watchtower.enable` / `.monitor-only` | honoured as a fallback for Watchtower migrations |

dockwarden never updates its own container (it recognises itself by
container ID), skips compose one-off containers, images pinned by digest and
locally built images with no compose build config.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success (nothing to do, or every update applied) |
| 1 | fatal: bad config, Docker unreachable |
| 2 | at least one update or registry check failed |
| `exit_code_on_updates` | updates are pending but were not applied (dry-run / monitor-only). Off by default; set e.g. `3` to make a scheduler mail you when updates exist |

## How rebuilds work

For a compose service with `build:`, dockwarden keys state on
`<project>/<service>` and stores the digest of every base image the
service was last built from:

* First run, no state: the digest of the locally present base image is used
  (that is what the last `docker compose build` used); if the base isn't
  present locally, the current remote digest is recorded and the build is
  assumed current.
* Later runs: when the registry digest of any base differs from the recorded
  one, the base is pulled and the service rebuilt; the new digests are saved
  only after a successful rebuild and recreate, so a failed build is retried
  next time. API builds also stamp the image with a
  `com.dockwarden.base-digests` label.

Supported compose features: `build:` as a string or map (`context`,
`dockerfile`, `dockerfile_inline`, `args` as list or map, `target`), several
`-f` files (later files override earlier ones), `${VAR}` interpolation from
the project's `.env` / `environment_file` and the process environment.
`extends`, `include` and remote (git/URL) contexts are not supported.

### Choosing how to build (`build.mode`)

| Mode | How | Trade-offs |
|---|---|---|
| `compose` | runs `docker compose build --pull <service>` with the docker CLI + compose plugin (shipped in the image) | Full BuildKit and compose semantics (the exact same build your `docker compose up --build` does). Requires the project directory to be visible **at the same absolute path as on the host** (or a `compose.path_map`), because the compose labels record host paths and the CLI resolves relative paths from there. |
| `api` | tars the build context (honouring `.dockerignore`) and streams it to the Engine's `ImageBuild` endpoint | No extra tooling, works with just the socket. Uses the **classic builder**: no BuildKit-only Dockerfile features (`RUN --mount`, heredocs, `COPY --link`, `# syntax=` frontends), no SSH/secret mounts, no multi-platform. Fine for simple `FROM base` + `COPY`/`RUN` images. |
| `command` | renders `build.command` and runs it through the shell | Escape hatch, e.g. `ssh nas "cd {{.WorkingDir}} && docker compose build --pull {{.Service}}"` to build on the host when dockwarden can't see the sources. |
| `auto` (default) | `compose` if `docker compose version` works, else `api` | |

The `synology` strategy and a `command` strategy with `handles_build: true`
perform the rebuild themselves (`docker compose up --build` on the NAS /
in your command), so dockwarden only pulls the new base image first.

### Choosing how to restart (`strategy.type`)

**`docker`** – native. For plain containers: stop → rename old → create a new
container with the same `Config`, `HostConfig` and networks → start →
remove the old one; if creation or start fails the old container is renamed
back and restarted. For compose containers, `docker compose up -d --no-deps
--no-build <service>` is used when the CLI is available
(`strategy.docker.compose_cli: auto|always|never`), which keeps compose's
own bookkeeping (config hashes, dependency order) intact; otherwise the
native path is used, which compose also accepts since all labels are
preserved.

**`command`** – `strategy.command.template` is a Go template rendered with
`.Kind` (`image`/`rebuild`), `.ContainerID`, `.ContainerName`, `.Image`,
`.OldImageID`, `.Project`, `.Service`, `.WorkingDir`, `.ConfigFiles`,
`.BaseImages`, `.Labels` and a `shellquote` function. Set `handles_pull` /
`handles_build` when the command does those steps itself. This is also the
SSH fallback: run dockwarden anywhere and let the host do the work:

```yaml
strategy:
  type: command
  command:
    template: ssh nas "cd {{.WorkingDir | shellquote}} && docker compose up -d --build --pull always {{.Service}}"
    handles_build: true
    handles_pull: true
```

**`synology`** – talks to DSM's Web API (`SYNO.API.Auth`,
`SYNO.Docker.Project`, `SYNO.Docker.Container`); see
[docs/synology-api.md](docs/synology-api.md) for the exact calls and the
sources they were verified against. Compose projects are mapped to Container
Manager projects by `project_map`, then by name, then by directory, and
redeployed with the same `build_stream` + `start_stream` calls the UI's Build
and Start buttons make. Plain containers are stopped, deleted with
`preserve_profile=true` and started again, which makes DSM recreate them
from their saved profile on the freshly pulled image.

```yaml
strategy:
  type: synology
  synology:
    url: https://nas.local:5001
    insecure_skip_verify: true      # self-signed DSM certificate
    password_file: /run/secrets/dsm_password
    project_map:
      my-compose-name: My DSM Project
```

Use a dedicated DSM user in the *administrators* group with 2FA disabled;
`dockwarden synology projects` / `synology containers` list what DSM knows
so you can check the mapping. A daily DSM Task Scheduler recipe is in
[examples/synology-task-scheduler.md](examples/synology-task-scheduler.md).

## Notifications

A generic webhook posts a JSON report (`updated`, `pending`, `failed`,
`errors`, `summary`, …) after each run; `on: changes|errors|always` controls
when. A `template` turns it into whatever your receiver wants, e.g. Slack:

```yaml
notify:
  webhook:
    url: https://hooks.slack.com/services/...
    template: '{"text": {{ .Summary | json }} }'
```

## Running in Docker

The image is based on `docker:cli` so `docker compose` is available for
builds and recreation. Mount:

* `/var/run/docker.sock`
* the state directory (`/var/lib/dockwarden`)
* `~/.docker/config.json` (read-only) for private registries
* every directory containing compose projects with `build:` sections, **at
  the same path as on the host** (or configure `compose.path_map`)

See [`docker-compose.example.yml`](docker-compose.example.yml).

## Design notes

* **Digest comparison instead of pulling.** Like Watchtower, a `HEAD` on the
  manifest is enough to detect changes and does not count against Docker
  Hub pull limits; the index is only fetched when the local digest is a
  platform-specific one.
* **Base image tracking via a state file**, not image labels: compose builds
  can't add labels without editing the Dockerfile, and the state file works
  for all build modes. API builds additionally label the image.
* **Compose containers are rebuilt per service and recreated per
  container**, so replicas share one build.
* **Strategies own the restart, the engine owns pull/build** unless the
  strategy says otherwise (`HandlesPull`/`HandlesBuild`), so Synology and
  SSH commands can take over the whole step.
* **Self-contained**: stdlib `flag`, `log/slog` and `text/template`; the
  only real dependencies are the Docker SDK, `distribution/reference` and
  `yaml.v3`.

## Development

```sh
make lint test build      # gofmt + go vet, go test -race, static binary
```

Tests use an in-memory fake of the Docker client
(`internal/dockerapi/fake`) and `httptest` servers for the registry, the
webhook and DSM. CI runs vet/test/build on every push; tags `v*` publish
binaries and a multi-arch image to GHCR.

## Limitations / not verified

* The Synology strategy was written from official docs and working
  community clients but has **not been exercised against a real DSM** by the
  author. The `*_stream` output format and the "Build then Start" semantics
  in particular deserve a first `--dry-run`-less test on a throwaway project.
* No OTP/2FA support for DSM logins.
* Compose `extends`/`include`, remote build contexts and BuildKit-only
  Dockerfile features in `api` build mode are not supported.
* Lifecycle hooks, rolling restarts and registry-specific rate limit
  handling are not implemented.

## License

MIT
