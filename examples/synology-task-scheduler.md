# Running dockwarden daily on a Synology NAS

Two options: a Task Scheduler job that runs the container once a day (the
"cron" model, recommended), or a long-running container with `--interval`.

## Option A: DSM Task Scheduler (one-shot run per day)

1. Put the config on the NAS, e.g. `/volume1/docker/dockwarden/config.yaml`
   (see `examples/config.yaml`). Keep DSM credentials out of it:
   create `/volume1/docker/dockwarden/secrets/dsm_password` with the
   password only (`chmod 600`).
2. Pull the image once: `docker pull ghcr.io/dansimau/dockwarden:latest`.
3. **Control Panel → Task Scheduler → Create → Scheduled Task → User-defined
   script**:
   * General: task name `dockwarden`, user `root` (needed for the Docker
     socket), enabled.
   * Schedule: daily, e.g. 04:00.
   * Task Settings → Run command:

```sh
#!/bin/sh
set -eu
PROJECTS=/volume1/docker              # where your compose projects live
CONF=/volume1/docker/dockwarden

docker run --rm --name dockwarden \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$CONF/config.yaml:/etc/dockwarden/config.yaml:ro" \
  -v "$CONF/state:/var/lib/dockwarden" \
  -v "$CONF/secrets:/run/secrets:ro" \
  -v "$PROJECTS:$PROJECTS" \
  -e DOCKWARDEN_SYNOLOGY_USERNAME=dockwarden \
  -e DOCKWARDEN_SYNOLOGY_PASSWORD_FILE=/run/secrets/dsm_password \
  ghcr.io/dansimau/dockwarden:latest --config /etc/dockwarden/config.yaml
```

   * Optionally tick *Send run details by email* / *only when the script
     terminates abnormally*. dockwarden exits non-zero (2) when an update
     failed, so DSM mails you only on problems. Set
     `exit_code_on_updates: 3` together with `monitor_only: true` if you
     want a mail whenever updates are available instead of applying them.
4. Run the task once manually and check the output in Task Scheduler →
   Action → View Result.

Notes:

* The project directory is mounted at the **same path** (`/volume1/docker`
  on both sides) so compose labels resolve and `docker compose build` works.
* With `strategy.type: synology` dockwarden asks Container Manager to
  rebuild/start projects and recreate containers, so DSM's UI stays in sync.
  Use a dedicated DSM user in the *administrators* group without 2FA.
  Compose projects are matched to DSM projects automatically; verify the
  derived mapping first with
  `docker run --rm ... ghcr.io/dansimau/dockwarden synology projects`
  (same mounts and env vars as above, `synology projects` instead of
  `--config ...`) and add `project_map` entries only for mismatches.
* With `strategy.type: docker` dockwarden recreates containers itself via the
  socket (Watchtower style). Container Manager will show the new
  containers, but projects created in its UI are best updated via the
  synology strategy.

## Option B: long-running container

Use `docker-compose.example.yml` with `--interval 24h`. Add it as a
Container Manager project or run it with `docker compose up -d`.
