<p align="center">
	<img src="assets/logo.svg" alt="doCKontroler" width="400">
</p>

<p align="center">
	<strong>A small, fast web UI for the containers on one Docker host.</strong><br>
	Start them, stop them, decide when they come back, and rebuild them into a
	fresh image — in a 15 MB container with no dependencies.
</p>

---

## What it does

- **Every container on one page**, running and stopped, grouped by Compose project.
- **Start, stop and restart** with one click.
- **Choose when a container starts again** — never, unless you stopped it, or always.
- **Recreate** a container so it picks up a rebuilt image. This is the thing a
  restart cannot do, and the reason this project exists.
- **A Telegram bot** with the same abilities, for when you are not at a browser.
- **Refreshes itself**, so you watch a container come up instead of pressing F5.

> **Screenshot** — _to be added: the container list in light and dark mode._

## Why not just use Portainer

Portainer is excellent and does far more than this. That is exactly the problem
for the everyday jobs: log in, find the stack, click through to the container,
find the button. Dockontroler is one page with no login that does five things,
uses about 12 MB of RAM, and starts instantly.

If you want image management, stack editing, multi-host or user accounts, use
Portainer. This is the thing you keep open in a pinned tab.

## About the name

The name is two words sharing letters:

```
do CK ontrol er
└──┬──┘      ┬     doCK … er  →  Docker
   └── C ontrol ──┘            →  Control
```

`doCK` and `er` spell **Docker** around the outside; **Control** sits in the
middle and borrows the same `C`/`K`. The logo colours the two groups and ties
them together with brackets. The favicon is just `CK` — the letters both words
share.

## Quick start

```bash
git clone https://github.com/mkrage/dockontroler.git
cd dockontroler
cp docker-compose.example.yml docker-compose.yml
```

Edit the `ports:` line to your server's LAN address — **do not** leave it on
`0.0.0.0`, see [Security](#security). Then:

```bash
docker compose up -d --build
```

Open `http://<your-server>:8080`.

Nothing else is needed: no Go toolchain, no database, no volume. The whole thing
is one static binary with the templates compiled in.

## Configuration

Everything is an environment variable, and everything is optional. Invalid values
stop startup with an explanation rather than falling back to a default.

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | Address to bind the web UI to. |
| `DOCKER_SOCKET` | `/var/run/docker.sock` | Path to the Engine socket. A `unix://` prefix is accepted. |
| `REFRESH_INTERVAL` | `5s` | How often the overview reloads. Minimum `1s`. |
| `STOP_TIMEOUT` | `10s` | Grace period before a container being stopped is killed. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `TELEGRAM_BOT_TOKEN` | – | Unset means the bot does not run at all. |
| `TELEGRAM_ALLOWED_CHAT_IDS` | – | Comma-separated chat ids. **Required** once a token is set. |
| `DOCKONTROLER_SELF_ID` | autodetect | Dockontroler's own container id or name. Only needed if autodetection fails. |

## Restart policies

The three buttons on each row map to Docker's restart policies:

| Button | Docker policy | Behaviour |
| --- | --- | --- |
| **Never** | `no` | Stays down after a reboot or a daemon restart. |
| **Unless stopped** | `unless-stopped` | Comes back on boot, unless you stopped it yourself. |
| **Always** | `always` | Comes back on boot even if you stopped it yourself. |

Changing a policy takes effect immediately, works on stopped containers, and never
starts or restarts anything by itself.

Docker also has `on-failure`, which is deliberately not offered here: it needs a
retry count, and it does not fit a one-click control. A container already using it
keeps it — the row shows the current value and none of the three buttons is
highlighted.

## Recreate: what it does, and what it does not

A container is bound to the **image id** it was created from, not to the tag. So
if you rebuild `myapp:latest`, a restart changes nothing — the container keeps
running the old image. Recreating builds a **new container from the same
configuration** against the current state of the tag. It is what Portainer's
"Deploy" button does to a stack, for one container.

**How it stays safe.** The original is renamed rather than deleted, and only
removed once the replacement is up. If anything fails along the way — the image
is broken, a port is taken, a network is gone — the replacement is discarded, the
original gets its name back and is started again. The worst realistic outcome is
"nothing changed", reported with the daemon's own error message.

**What is carried over.** Everything Docker recorded about the container: command,
environment, ports, labels, capabilities, sysctls, log config, device requests,
and every field this tool has never heard of. Volumes and networks come across
too, including anonymous volumes (the ones with generated names that many database
images create — losing those is the classic way a naive recreate destroys data)
and statically assigned IP addresses.

**Compose containers are safe to recreate.** The Compose labels, including the
config hash, are preserved, so a later `docker compose up -d` sees no drift and
leaves the container alone.

**Where it declines.** The button is disabled, with the reason, when:

- the container references an image **id** rather than a tag — there is nothing to
  re-resolve;
- it shares another container's network namespace (`network_mode: "container:…"`)
  — that reference cannot survive a replacement;
- it is Dockontroler itself.

A digest-pinned image (`postgres@sha256:…`) can be recreated, but the row says so:
you get a new container running the same image.

**No image pulling.** Recreate uses whatever the tag points at locally. It is
built for "I just rebuilt this myself", not for chasing upstream updates — use
Watchtower for that.

## Telegram bot

1. Talk to [@BotFather](https://t.me/BotFather), `/newbot`, copy the token.
2. Set `TELEGRAM_BOT_TOKEN` and restart.
3. Message your new bot. It will refuse you and reply with your chat id.
4. Put that id in `TELEGRAM_ALLOWED_CHAT_IDS` and restart.

Send `/list` and control everything from the buttons. The bot edits its own
message in place, so a control session stays one message instead of a wall of
them. Recreate asks for confirmation first.

The allow-list is mandatory, and startup fails without it. A Telegram bot is
reachable by anyone who knows its name; without an allow-list, that means anyone
could stop your containers.

Only long polling is used, so nothing needs to be reachable from the internet. If
the token already has a webhook registered, polling is refused by Telegram —
Dockontroler logs that and does not delete the webhook, since it may belong to
something else you run.

## Security

Read this part properly. It is the main trade-off of the whole project.

**There is no authentication.** Anyone who can reach the page can start, stop and
replace every container on the host. That is the design — it is what makes it fast
to use — and it is only reasonable on a trusted network.

**The Docker socket is root on the host.** Anything that can talk to
`/var/run/docker.sock` can start a privileged container that mounts `/`. That
applies to Dockontroler and to anyone who reaches its page. Mounting the socket
`:ro` does not change this: a read-only bind mount stops the socket *file* from
being replaced, it does not restrict the API.

Taken together:

- **Bind to a specific LAN address**, not `0.0.0.0`. The example compose file does
  this and the comment says why.
- **Never port-forward this**, and do not put it behind a plain reverse proxy on a
  public hostname. If you need it from outside, use a VPN such as WireGuard or
  Tailscale.
- Consider a **socket proxy** (`tecnativa/docker-socket-proxy`) to expose only the
  endpoints this tool uses. Note the caveat in the compose example: that needs a
  TCP endpoint, which is not supported yet.

**Running as a non-root user.** The image runs as root so it can open the socket,
which is `root:docker` mode `660`. To avoid that, add the host's `docker` group
instead:

```yaml
user: "65532:65532"          # the nonroot uid in distroless
group_add:
  - "998"                    # your docker gid: getent group docker | cut -d: -f3
```

## Development

Neither Go nor anything else needs to be installed — the toolchain runs in a
throwaway container:

```bash
# format, vet and run the full test suite
docker run --rm -v "$PWD":/src -w /src golang:1-alpine \
	sh -c 'gofmt -w . && go vet ./... && go test -race ./...'

# see what the formatter changed
git diff

# build and run
docker compose up -d --build && docker compose logs -f
```

The tests need no Docker daemon. `internal/manager` runs against a small
simulator of the Engine API, so even the recreate logic — including every rollback
path — is exercised in-process.

If you do have Go locally, `go run .` works directly; without a mounted socket it
exits with a clear message about the daemon.

### Layout

```
main.go               wiring, graceful shutdown
internal/config       environment parsing and validation
internal/docker       Engine API client, plain HTTP over the unix socket
internal/manager      the core: list, start, stop, policy, recreate
internal/web          server-rendered UI, embedded templates and assets
internal/telegram     bot, long polling
```

`internal/manager` knows nothing about HTTP or Telegram. Both interfaces are thin
adapters over it, which is what keeps rules like "never stop yourself" from
existing in two places.

### The one file to read carefully

`internal/manager/config_copy.go` turns an inspect result into a create payload.
It is where the recreate traps live, each one commented with what breaks if it is
removed. Its golden-file test is the safety net that catches configuration being
silently dropped:

```bash
# after an intentional change to the payload — then read the diff
go test ./internal/manager -update
```

An unexplained change to that golden file is a bug report, not a formatting nit.

## Not in v1

Deliberately left out, roughly in the order they would make sense to add:

- **Log viewing** — [Dozzle](https://github.com/amir20/dozzle) already does this
  very well, and it is the obvious companion.
- **TCP / socket-proxy support** — would make the hardening advice above actually
  usable.
- **Pulling images** before a recreate, which would make this a Watchtower on a
  button.
- `on-failure` restart policy.
- Authentication, so it could be exposed more widely.
- Multiple hosts, image/volume/network management, metrics.

## License

MIT — see [LICENSE](LICENSE).
