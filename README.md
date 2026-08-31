# pinbumper

Re-pin **exact** Docker image tags in Compose files and Portainer CE stacks, or follow a **floating tag** (`latest`, `lts`, `main`, `2`, …) and redeploy when that tag’s registry digest changes. The allowed window (or follow tag) lives in Compose labels.

`apply` is the default command and the only one that writes. `plan` is a dry-run. The container image `CMD` is `apply`, so a Portainer stack can omit `command:` entirely. Unlabeled services are never touched. There is **no auto-rollback**.

## Why not Watchtower or WUD?

| | Watchtower | WUD | pinbumper |
|---|---|---|---|
| What it watches | Running containers | Registries / tags | Compose / Portainer stack files |
| How updates happen | Pull the **same** tag (often `:latest`) and recreate | Notify or update when a newer tag matches include/exclude | Rewrite the **pin** to a newer allowed tag, **or** (`pinbumper.follow`) pull the same floating tag when its **registry digest** changes |
| Major-version surprise | `:latest` (or a floating `:3`) can jump | You filter tags, but the Compose pin is not the source of truth | `^3.1.0` cannot select `4.0.0`. `follow` can jump whenever the floating tag moves |
| Portainer stack `Env` | N/A | Easy to omit on a hand-rolled PUT (that **wipes secrets**) | PUT always sends the existing `Env` array |
| Labels required | Watchtower's own enable/disable labels | WUD include/exclude | **Only** `pinbumper.*` — Watchtower labels are not used |

Watchtower pulls the same tag when the registry digest moves. `pinbumper.follow` is that behavior, driven from the Compose `image:` line (the tag string does **not** change). `pinbumper.range` / `include` is the other mode: Compose stays an exact pin and that pin moves on purpose, inside a window you declared.

## Pin syntax

Labels go on the **service**. The `image:` value must be a literal tag, not `${VAR}`. Digest-only pins (`image@sha256:…` with no tag) are skipped. If a tag+digest is rewritten by range/include, the `@digest` is dropped and only the new tag is written.

For **range** / **include**, prefer an exact pin (`3.1.0`), not `latest`. For **follow**, the image tag *is* the floating tag you want to watch (`latest`, `lts`, `main`, `2`, …).

### 1. Semver (npm / Renovate) — default when `pinbumper.range` is set

```yaml
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
```

npm-semver rules (via [Masterminds/semver](https://github.com/Masterminds/semver), caret/tilde compatible with npm):

| Range | Meaning |
|---|---|
| `^3.1.0` | ≥3.1.0 and &lt;4.0.0 (stay on 3.x) |
| `~3.1.0` | 3.1.x patches only |
| `3.1.0` | exact — never bump |

Candidates are `MAJOR.MINOR.PATCH` or two-part `MAJOR.MINOR` (treated as `MAJOR.MINOR.0`, so official `postgres:15.19` matches `^15.0.0`). `latest`, `beta`, `rc`, and floating majors like `:15` are ignored. Prereleases (`3.2.0-rc.1`) are ignored unless the range itself includes a prerelease.

The newest matching **semver** wins.

### 2. Regex (WUD-style include) — for registries that are not semver

```yaml
services:
  app:
    image: example/calendar:v2026.1
    labels:
      pinbumper.include: "^v2026\\.\\d+$"
      # pinbumper.exclude: ".*-nightly.*"   # optional denylist
```

When **only** include/exclude is set, pinbumper picks the newest tag that matches `include` and does not match `exclude`.

**Sort:** version-aware comparison, like GNU `sort -V`. Digit runs are compared numerically (`v2026.10` &gt; `v2026.8`); non-digit runs are compared as strings. A longer equal prefix wins (`1.0.0` &gt; `1.0`). This is best-effort, not semver.

### 3. Follow (Watchtower-style digest) — when `pinbumper.follow` is set

Keep a floating tag and redeploy when the **registry digest** for that tag changes. The compose `image:` tag string does **not** change.

```yaml
services:
  vaultwarden:
    image: vaultwarden/server:latest
    labels:
      pinbumper.follow: "latest"
```

`plan` prints `FOLLOW latest@sha256:abc… -> sha256:def…` (or `NOOP` if the running RepoDigest already matches). `apply` PUTs the **same** compose (Portainer: existing `Env` array, `PullImage` / `RepullImageAndRedeploy` true) or runs `docker compose` with `--pull always`. No auto-rollback.

The image tag must equal `follow`; a mismatch is skipped and logged (no crash). Follow looks up the digest via Docker Hub and GHCR (same User-Agent and OCI fallback as tag listing). It does **not** semver-sort `latest` / `main`.

If `pinbumper.range` or `pinbumper.include` is also set, **range/include wins**. Follow is ignored — it is only for digest-of-current-tag, not for choosing a new tag.

### Combining labels

If `pinbumper.range` **and** `pinbumper.include` are set, a candidate must satisfy **both** (semver range **and** regex). `exclude` always denylists. `pinbumper.exclude` alone is invalid (it would otherwise allow `latest`).

List-form labels work the same:

```yaml
labels:
  - "pinbumper.range=^3.1.0"
```

Services with **no** `pinbumper.*` labels are skipped — including typical database pins like `postgres:15` and `redis:7`. If you do label them, `^15.0.0` matches official two-part Hub tags (`15.19`).

## Paperless example

Bump the app on 3.x only. Leave Postgres and Redis unlabeled so they stay on the major you chose:

```yaml
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.1.0
    labels:
      pinbumper.range: "^3.1.0"
    # healthcheck: …   # after apply, pinbumper waits if this exists

  postgres:
    image: postgres:15          # unlabeled — never touched

  redis:
    image: redis:7              # unlabeled — never touched
```

```
pinbumper plan  --compose-file docker-compose.yml
pinbumper apply --compose-file docker-compose.yml
```

## Dry-run vs apply

| Command | Effect |
|---|---|
| `pinbumper plan …` | Discover, list tags, print `BUMP` / `NOOP`. **Writes nothing.** Tag-list failures (e.g. Hub 401/403) skip that service and continue; the run still exits 0 if other services planned. |
| `pinbumper apply …` | Same plan, then rewrite pins and deploy. Non-zero exit on deploy/rewrite failure — not on a skipped registry 401/403. |

`apply` is the default (`pinbumper --portainer-url …` and the image `CMD`). `plan` is always an explicit dry-run. If the newest allowed tag is already the current pin, the result is `NOOP`.

## Portainer CE

```
pinbumper plan  --portainer-url http://portainer:9000 --api-key-file ./portainer-api.key
pinbumper apply --portainer-url http://portainer:9000 --api-key-file ./portainer-api.key
```

Use the LAN HTTP URL when you can. Putting Portainer behind Cloudflare (or another proxy that buffers long requests) can hang stack updates.

`--http-timeout` (default 60s) applies to **tag listing** and Portainer GET. Portainer `PUT` (pull + redeploy) uses `--deploy-timeout` (default 30m, `0` disables the client deadline). A 60s abort mid-PUT can leave the stack half-applied; there is no rollback.

`--tls-skip-verify` applies to **Portainer only** (LAN HTTPS). Docker Hub / GHCR keep TLS verification.

A weekly unfiltered run (no `--stack`) **skips** stacks it cannot read or parse (invalid labels, missing stack file, git-backed) and continues with the rest. Git-backed stacks are never PUT. The same skip+log applies to a single service whose registry returns 401/403 (or any other tag-list error): other services in that stack still `BUMP` / `NOOP`. If every labeled service is skipped, the run still exits 0 (same as an empty stack list).

The key is sent as `X-API-Key` and is never logged. **Both** of these are first-class (pick one):

1. **`PORTAINER_API_KEY`** — Portainer stack Environment variable (the UI Env array). Portainer stores `Env` on the stack and shows it in the UI.
2. **`PORTAINER_API_KEY_FILE`** — path to a file that contains the key, bind-mounted into the container.

A file path (`--api-key-file` or `PORTAINER_API_KEY_FILE`) wins over `PORTAINER_API_KEY`.

### Portainer stack (no `command:`, no flags)

Image `CMD` is `apply`. Omit `command:` so a stack run applies.

**API key as stack Env:**

```yaml
services:
  pinbumper:
    image: ghcr.io/gering/pinbumper:0.1.0
    environment:
      PORTAINER_URL: http://192.168.1.16:9000
      PORTAINER_API_KEY: ${PORTAINER_API_KEY}
```

**API key from a file (equal alternative):**

```yaml
services:
  pinbumper:
    image: ghcr.io/gering/pinbumper:0.1.0
    environment:
      PORTAINER_URL: http://192.168.1.16:9000
      PORTAINER_API_KEY_FILE: /run/portainer-key
    volumes:
      - ./portainer-api.key:/run/portainer-key:ro
```

Dry-run the same image with `docker compose run --rm pinbumper plan` (extra args replace `CMD`).

### Env pitfall (do not roll your own PUT)

Portainer’s `PUT /api/stacks/{id}` **replaces** the stack’s environment with the `Env` array in the body. If you omit `Env`, Portainer stores an empty list and **wipes secrets** (`POSTGRES_PASSWORD`, and so on).

pinbumper always reads the stack, then sends that same `Env` array back unchanged, with `PullImage` / `RepullImageAndRedeploy` set to `true`. Git-backed stacks are skipped (a file PUT would detach them from git).

## What apply does

1. **Discover** local `--compose-file` paths and/or every Portainer compose/swarm stack.
2. **List tags** from Docker Hub (Hub catalog API, with a User-Agent; OCI `registry-1.docker.io` fallback on Hub 401/403, and also 429/5xx) or GHCR / other registries (OCI distribution + bearer challenge). For `pinbumper.follow`, look up the **manifest digest** for the current tag the same way (Hub tag API + OCI fallback). `GITHUB_TOKEN` is optional for GHCR rate limits. Tokens are never logged. If the OCI fallback also fails, the skip/log includes the catalog status and the fallback error.
3. **Choose**
   - range/include: the newest tag allowed by the service’s labels. Same as current pin → noop. `latest` / `main` are never semver candidates.
   - follow: compare the registry digest to the running **image** RepoDigest (Portainer image inspect, or local `docker image inspect` — not container inspect). Different → digest-only bump. Same → noop. Image tag ≠ follow → skip and log.
4. **Rewrite** only the image tag in the original YAML (comments and key order stay). Follow does **not** rewrite the image line.
5. **Deploy**
   - Local: `docker compose -f FILE up -d --pull always --no-deps <changed services>` (or `--skip-deploy` to only write the file).
   - Portainer: `PUT` the stack with the compose (updated pin, or the **same** file for follow) + existing `Env`, `PullImage` true.
6. **Health** — if the service has a Compose `healthcheck`, wait until `healthy` (default 10m, `--health-timeout`). If the container exits or goes `unhealthy`, apply fails with a **non-zero exit**. pinbumper does **not** roll the pin or the stack back.

## Install

```bash
go install github.com/gering/pinbumper/cmd/pinbumper@latest
```

Or build a local image:

```bash
docker build -t pinbumper:local .
docker run --rm -v "$PWD:/work:ro" pinbumper:local plan --compose-file /work/docker-compose.yml
```

A weekly Portainer example (env-only, no `command:`) is in [`examples/docker-compose.weekly.yml`](examples/docker-compose.weekly.yml). Schedule with cron or a systemd timer; do not commit API keys.

## CLI

```
pinbumper apply --portainer-url URL --api-key-file PATH
pinbumper apply --compose-file docker-compose.yml
pinbumper plan  --portainer-url URL --api-key-file PATH
pinbumper plan  --compose-file docker-compose.yml
```

`--compose-file` and `--portainer-url` can be used together. `--stack NAME` limits Portainer discovery. `docker run … plan` / `pinbumper plan` still dry-run; extra container args replace `CMD`.

`--skip-deploy` rewrites **local** Compose files only. It does **not** `docker compose up` and does **not** PUT Portainer stacks.

## Development

```bash
go test ./...
go vet ./...
```

CI runs tests, `go vet`, and golangci-lint. MIT license.

## Disclaimer

pinbumper updates pins you asked it to update. Database major upgrades, breaking app releases inside a range you set too wide, and a failed healthcheck after apply are all your problem — the tool will not revert the file or the stack.
