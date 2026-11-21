# Synology DSM Container Manager Web API notes

What dockwarden's `synology` strategy calls, and where each fact comes from.
Nothing here was verified against a live DSM by the author; it is compiled
from the official DSM API guide and from community clients that work against
DSM 7.2 Container Manager. If you can confirm or correct any of it on a real
NAS, please open an issue.

**[O]** official Synology source, **[C]** community source / working code.

## Request basics

* All calls go to `POST /webapi/entry.cgi` with `api`, `method`, `version`
  form fields. The response envelope is `{"success":true,"data":...}` or
  `{"success":false,"error":{"code":N}}`. [O1]
* Every `SYNO.Docker.*` API has `requestFormat: "JSON"`, which means each
  parameter value must be JSON-encoded: strings are sent **with quotes**
  (`id="9fb91ca5-…"`, `name="web"`), booleans as `true`/`false`, numbers
  bare. [O1 p.4, p.14] This is what the DSM UI sends [C1] and what working
  clients do [C2][C3][C4]. dockwarden JSON-encodes every value.

## SYNO.API.Info

`api=SYNO.API.Info&version=1&method=query&query=SYNO.API.Auth,SYNO.Docker.Project,SYNO.Docker.Container`
returns `{path, minVersion, maxVersion, requestFormat}` per API; no login
needed. [O1 pp.9–14] dockwarden uses it to pick the `SYNO.API.Auth` version
and to warn when `SYNO.Docker.Project` is missing (Container Manager not
installed / older DSM).

## SYNO.API.Auth

| method | version | params | returns |
|---|---|---|---|
| `login` | 6–7 (6 "recommended") | `account`, `passwd`, `format=sid`, `enable_syno_token=yes`, optional `session`, `otp_code` | `data.sid`, `data.synotoken` |
| `logout` | same | `_sid` | — |

Source: [O1 pp.15–17]. Afterwards every request carries `_sid=<sid>`
(form parameter) and the SynoToken. The guide documents `SynoToken=<token>`
as a request parameter [O1 p.16]; the DSM UI and most clients send the
`X-SYNO-TOKEN` header [C2][C5]. dockwarden sends both, plus the `id` cookie.

Error codes [O1 p.18]: 400 wrong account/password, 401 disabled account,
402 denied permission, 403 2FA code required, 404 bad 2FA code, 406 2FA
enforced, 407 blocked IP, 409/410 password expired. dockwarden does not
support OTP: use a dedicated account without 2FA.

Common codes [O1 pp.8–9]: 101 missing api/method/version, 102 API does not
exist, 103 method does not exist, 104 unsupported version, **105 no
permission** (use an *administrators* account — Container Manager has no
per-user application privilege, see [C7]), 106 session timeout, 119 invalid
SID, 150 source IP mismatch.

## SYNO.Docker.Project (Container Manager, DSM 7.2+)

All known clients use `version=1`. Project IDs are UUIDs.

| method | params | notes / source |
|---|---|---|
| `list` | — | `data` is an object keyed by project id with `id, name, path, share_path, status (RUNNING/STOPPED/ERROR), services, containerIds, is_package, …` [C2][C3][C4] |
| `get` | `id` | project plus `containers` and `content` (compose YAML) [C2][C3] |
| `build_stream` | `id` | What the UI's **Build** button calls; per the Container Manager help, Build = `docker-compose up --build --no-start` [O2][C1][C4][C6] |
| `start_stream` | `id` | **Start** = `docker-compose start` [O2][C1][C6] |
| `stop_stream` | `id` | **Stop** = `docker-compose stop` [O2][C4] |
| `restart_stream` | `id` | `docker-compose restart` [C6] |
| `clean_stream` | `id` | **Clean** = `docker-compose down` — removes containers, networks, volumes and images; dockwarden never calls it [O2][C4] |
| `build`, `start`, `stop`, `clean` | `id` | non-streaming variants used by [C2]; one report says `start` "sometimes fails" where `start_stream` works [C1] |
| `create` / `update` / `delete` | `name, share_path, content, …` / `id, content` / `id, preserve_content` | [C2][C3]; not used by dockwarden |

The `*_stream` responses are plain text lines (compose output) that end
when the action completes; dockwarden logs them at debug level and treats a
JSON `success:false` line or an HTTP error as failure.

dockwarden's project redeploy = `build_stream` followed by `start_stream`,
which mirrors clicking Build then Start in the UI. Because dockwarden has
already pulled the newer base image through the Docker socket, the compose
build picks it up.

Project matching: an explicit `strategy.synology.project_map` entry, then a
case-insensitive match of the compose project name against the DSM project
`name`, then a match of the compose `working_dir` against the DSM project
`path` (e.g. `/volume1/docker/myapp`).

## SYNO.Docker.Container

`version=1`, containers addressed by `name`.

| method | params | source |
|---|---|---|
| `list` | `limit=-1`, `offset=0`, `type="all"` | [C3][C4] |
| `start` / `stop` / `restart` | `name` | [C2][C3][C5] |
| `delete` | `name`, `force` (bool), `preserve_profile` (bool) | [C2][C8] |
| `create` | `profile` (JSON, as exported by `SYNO.Docker.Container.Profile`) | [C2] |

Recreating a plain (non-project) container on a new image, as done by
[C8] (a working DSM 7 updater): `stop` → `delete` with
`preserve_profile=true, force=false` → `start`. DSM keeps the container's
profile and recreates it from the (now updated) local image on `start`.
dockwarden does exactly this after it has pulled the image.

## Could not verify

* `SYNO.Docker.Project` version/`requestFormat` from a live
  `SYNO.API.Info` dump — all public dumps predate Container Manager.
* Whether `build_stream` leaves previously running containers stopped
  (hence the unconditional `start_stream`).
* Exact stream line format of `*_stream` methods.
* That Container Manager requires the *administrators* group — no official
  statement exists; it is the community consensus and error 105 is what you
  get otherwise.

## Sources

* [O1] DSM Login Web API Guide (2023) — https://global.download.synology.com/download/Document/Software/DeveloperGuide/Os/DSM/All/enu/DSM_Login_Web_API_Guide_enu.pdf
* [O2] Container Manager help, *Project* — https://kb.synology.com/en-global/DSM/help/ContainerManager/docker_project?version=7
* [C1] Browser-captured project calls (DSM 7.2) — https://blog.differentpla.net/blog/2025/07/12/synowebapi-container-manager-projects/
* [C2] N4S4/synology-api `docker_api.py` — https://github.com/N4S4/synology-api
* [C3] LukeWinikates/synology-go (Go client incl. project streams) — https://github.com/LukeWinikates/synology-go
* [C4] rjadams82/synology-docker-project-automate (`synowebapi` project rebuild script) — https://github.com/rjadams82/synology-docker-project-automate
* [C5] acme.sh `deploy/synology_dsm.sh` (login/SynoToken handling) — https://github.com/acmesh-official/acme.sh/blob/master/deploy/synology_dsm.sh
* [C6] hifishhe/Synology-Docker-MCP — https://github.com/hifishhe/Synology-Docker-MCP
* [C7] synoforum: permissions for a user to run Docker — https://www.synoforum.com/threads/permissions-for-user-to-run-docker.3536/
* [C8] Xboarder56/SynoDockerContainerUpgrade (`update_containers.py`) — https://github.com/Xboarder56/SynoDockerContainerUpgrade
* [C9] dzhuneyt: cron + `synowebapi` `start_stream` — https://dzhuneyt.com/post/synology-cron-docker-compose
