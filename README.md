# slayground

**Slay your idle Docker Compose stacks.** Slayground is a tiny HTTP proxy you
drop into a Compose stack. When nobody has used the app for a while, it stops
every other container in the stack. The next visitor gets a brief "waking
things up" page while the containers start again — then traffic flows as if
nothing happened.

Perfect for the pile of self-hosted apps you use twice a month but leave
running 24/7.

- **Zero-config by default** — auto-discovers its own Compose project via the
  Docker socket; the only required setting is the upstream to forward to.
- **Real proxy** — headers pass through untouched, WebSockets and SSE work,
  the original `Host` is preserved (happy behind Cloudflare, Tor, or any
  fronting proxy).
- **Health-aware wake-up** — containers with healthchecks must report healthy
  before traffic is forwarded.
- **Monitor-friendly** — uptime bots (by user agent) and health endpoints (by
  path) can be ignored so they neither keep the stack awake nor wake it.
- **Featherweight** — a single static Go binary in a `FROM scratch` image, one
  small YAML dependency, no Docker SDK.

## Quick start

Add one service to your `docker-compose.yml`:

```yaml
services:
  slayground:
    image: ghcr.io/jclement/slayground:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      SLAYGROUND_UPSTREAM: http://app:8080
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock

  app:
    image: your/app
```

That's it. Slayground inspects its own container to find the Compose project,
and after 30 idle minutes stops everything in the project except itself.
Any real request wakes it all back up.

A runnable example lives in [examples/docker-compose.yml](examples/docker-compose.yml).

## How it works

```
        ┌───────────────── idle timeout ─────────────────┐
        ▼                                                 │
  [suspended] ──request──▶ [starting] ──healthy──▶ [up] ──┘
   503 / wait page          wait page               proxy
```

- While **up**, requests are reverse-proxied to the configured upstream(s).
  Every non-ignored request resets the idle clock.
- When the idle timeout passes, slayground **stops** all other containers in
  its Compose project (`docker stop`, honoring the grace period).
- The next non-ignored request triggers **start**: browsers see an
  auto-refreshing wait page (`503` + `Retry-After` for API clients) while the
  containers start and their healthchecks go green, then traffic forwards
  again.
- Containers labeled `slayground.exclude: "true"` (say, a database) and
  Compose one-off containers (`docker compose run ...`) are never touched.
- On startup slayground resumes the whole stack once, so it's always in a
  known-good state.

If the Docker socket isn't mounted, slayground logs a warning and runs as a
plain always-on proxy.

## Configuration

Everything is set with environment variables, or optionally a YAML file
(point `SLAYGROUND_CONFIG` at it). Environment variables win over the file.

| Environment variable | Default | Description |
|---|---|---|
| `SLAYGROUND_UPSTREAM` | *(required)* | Default upstream base URL, e.g. `http://app:8080` |
| `SLAYGROUND_LISTEN` | `:80` | Listen address |
| `SLAYGROUND_IDLE_TIMEOUT` | `30m` | Idle time before the stack is stopped |
| `SLAYGROUND_STARTUP_TIMEOUT` | `5m` | Max time to wait for containers to become healthy on wake |
| `SLAYGROUND_STOP_TIMEOUT` | `30s` | Per-container grace period when stopping |
| `SLAYGROUND_IGNORE_USER_AGENTS` | | Comma-separated, case-insensitive user-agent substrings (e.g. `UptimeRobot,Pingdom,kube-probe`) |
| `SLAYGROUND_IGNORE_PATHS` | | Comma-separated path prefixes (e.g. `/health,/ping`) |
| `SLAYGROUND_ROUTES` | | Path-based routing: `/api=http://api:3000,/ws=http://ws:9000` |
| `SLAYGROUND_COMPOSE_PROJECT` | *(auto)* | Override Compose project auto-discovery |
| `SLAYGROUND_DOCKER_SOCKET` | `/var/run/docker.sock` | Docker daemon socket path |
| `SLAYGROUND_CONFIG` | | Path to a YAML config file |

Ignored requests (matching a user agent or path) are still proxied while the
stack is up — they just don't count as activity, and while the stack is asleep
they get a `503` instead of waking it.

See [examples/config.yaml](examples/config.yaml) for the full YAML equivalent.

### URL-based routing

Routes use longest-prefix matching on path-segment boundaries (`/api` matches
`/api` and `/api/v1`, not `/apiary`). Anything that matches no route goes to
`SLAYGROUND_UPSTREAM`:

```yaml
environment:
  SLAYGROUND_UPSTREAM: http://web:3000
  SLAYGROUND_ROUTES: /api=http://api:8080
```

### Keeping containers running

```yaml
db:
  image: postgres:17
  labels:
    slayground.exclude: "true"
```

## Images

Multi-arch (amd64/arm64) images are published to GitHub Container Registry:

| Tag | Meaning |
|---|---|
| `ghcr.io/jclement/slayground:latest` | Every CI build of `main` |
| `ghcr.io/jclement/slayground:1.2.0`, `:1.2` | Stable tagged releases |

The running version is baked in at build time (`slayground -version`).

## Development

Tooling is managed with [mise](https://mise.jdx.dev):

```sh
mise install        # Go toolchain
mise run test       # go test -race ./...
mise run build      # binary in bin/ with version stamped
mise run docker     # local image build
mise release        # tag & push the next release (minor bump; or: patch/major)
```

`mise release` computes the next version from existing `v*` tags, pushes the
tag, and CI publishes the versioned image.

## License

[MIT](LICENSE)
