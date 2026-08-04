<p align="center">
	<img src="assets/logo.svg" alt="docKontroler" width="440">
</p>

<p align="center">
	<strong>A small, fast web UI for the containers on one Docker host.</strong><br>
	Start them, stop them, decide when they come back, and rebuild them into a
	fresh image — in a 15 MB container with no dependencies.
</p>

---

## What it does

- **Every container on one page**, running and stopped, ordered by Compose project.
  As many tiles per line as the screen has room for — seven on a wide monitor, one
  on a phone. Same page, no app to install. What is stopped folds away below.
- **The port each service is on**, and a link straight to it where there is a web
  interface behind it.
- **Start, stop and restart** with one click.
- **Choose when a container starts again** — never, unless you stopped it, or always.
- **Recreate** a container so it picks up a rebuilt image. This is the thing a
  restart cannot do, and the reason this project exists.
- **A Telegram bot** with the same abilities, for when you are not at a browser.
- **Refreshes itself**, so you watch a container come up instead of pressing F5.

<!-- TODO: add a screenshot of the container list, in light and dark mode. -->

## Why not just use Portainer

Portainer is excellent and does far more than this. That is exactly the problem
for the everyday jobs: log in, find the stack, click through to the container,
find the button. docKontroler is one page with no login that does five things,
uses about 12 MB of RAM, and starts instantly.

If you want image management, stack editing, multi-host or user accounts, use
Portainer. This is the thing you keep open in a pinned tab.

## Quick start

On the machine that runs Docker:

```bash
git clone https://github.com/mkrage/dockontroler.git
cd dockontroler
./rebuild.sh
cp docker-compose.example.yml docker-compose.yml
```

`rebuild.sh` builds `dockontroler:latest`, stamps the version into the binary, and
removes any container from a previous build. It uses `docker buildx` when it is
installed and the legacy builder otherwise; the legacy builder can only produce an
image for the host's own architecture, so cross-building for a Raspberry Pi from an
amd64 machine needs buildx. Edit the `ports:` line to your server's
LAN address — **do not** leave it on `0.0.0.0`, see [Security](#security). Then:

```bash
docker compose up -d
```

Open `http://<your-server>:3625`.

### The compose file

Two lines need your attention: the address in `ports:` and, if you want the bot, the
Telegram pair. Everything else works as it stands.

```yaml
services:
  dockontroler:
    image: dockontroler:latest      # built and tagged by ./rebuild.sh
    container_name: dockontroler
    restart: unless-stopped

    ports:
      # CHANGE THIS to your server's LAN address. The plain "3625:3625" form binds
      # to every interface, including ones you did not think about.
      - "192.168.1.10:3625:3625"

    volumes:
      # The reason it works, and the reason it is dangerous: this socket is root on
      # the host. The :ro stops the file being replaced, it does not restrict the API.
      - /var/run/docker.sock:/var/run/docker.sock:ro

    # Nothing is ever written to disk.
    read_only: true
    security_opt:
      - no-new-privileges:true

    environment:
      # All optional, defaults are in Configuration below. Keep one of them
      # active: a key with only comments under it is null, and compose rejects
      # that ("environment must be a mapping").
      LOG_LEVEL: "info"

      # REFRESH_INTERVAL: "5s"
      # STOP_TIMEOUT: "10s"

      # Telegram. Without a token the bot does not run at all; with one, the
      # allow-list is mandatory and startup fails without it.
      # TELEGRAM_BOT_TOKEN: "123456789:AAExampleTokenFromBotFather"
      # TELEGRAM_ALLOWED_CHAT_IDS: "123456789"

      # Only needed if the log says the own container could not be identified.
      # DOCKONTROLER_SELF_ID: "dockontroler"
```

[`docker-compose.example.yml`](docker-compose.example.yml) is this same file with the
reasoning spelled out line by line, plus the socket-proxy hardening option. Read that
one before you change the socket mount or the port binding.

### Deploying from Portainer

A Portainer stack cannot build an image: the stack editor has no build context, so
`build: .` fails there. That is why the compose file points at a tag you build
yourself.

1. On the server, clone the repo and run `./rebuild.sh`.
2. **Stacks → Add stack**, paste the compose file above, fix the `ports:` line.
3. Deploy with **Pull latest image** switched **off** — the tag exists only on this
   host, and a pull would go looking for it on Docker Hub.

After a code change, run `./rebuild.sh` again and hit **Update the stack**. The
script removes the old container, so the redeploy comes up on the new image.

Nothing else is needed: no Go toolchain, no database, no volume. The whole thing
is one static binary with the templates compiled in.

## The overview

Each container is a card: name and state on the first line, then the image, its
ports, the three actions and the autostart setting. How many cards sit beside each
other is left to the browser — one per line on a phone, four on a laptop, seven on a
2560-pixel monitor. There is no separate mobile page and no client detection; it is
the same HTML either way.

**The state is the card's left edge** — green running, amber restarting, red dead,
grey stopped. A colour is scannable across thirty cards in a way a dot beside thirty
names is not, and nothing depends on seeing it: the status text says the same thing
in Docker's own words.

**What is not running has its own folded section** below the grid. On a host where
six of thirty-three containers are stopped on purpose, they are what you go looking
for, not what you watch. A container *restarting* stays in the grid: a crash loop is
the one thing on the page that wants attention, and it is offered **Stop** rather
than Start, because Docker cannot start what is already trying.

The cards form **one grid, not a section per Compose project**. A project is not a
layout unit: on a typical host two thirds of them hold a single container, so a
section each means a row each, which is how a wide screen ends up showing one card
per line and scrolling for pages. The containers of a project stay next to each
other because that is how they are sorted, and a card names its project unless the
project and the service are the same word — a one-service stack, where the name
already says it.

**Ports** are the host ports, with the container port after an arrow when the two
differ (`8080 → 80`). A tcp port on a running container is a link, and it is built
from the address you are reading the page at: the daemon only knows the port is
bound to `0.0.0.0`, and nothing here can tell which of the host's addresses reaches
it — your browser just demonstrated one. Two cases deviate on purpose:

- A port bound to one specific address links to *that* address, because it is the
  only one that answers.
- `network_mode: host` publishes nothing, so the ports come from what the image
  exposes. Those are host ports as they stand.

The scheme is a guess — `https` for container port 443, 8443 and 9443, `http`
otherwise. Docker knows which ports are published, never what speaks behind them.
A stopped container still shows its configured mapping, without a link: there is
nothing listening yet.

## Configuration

Everything is an environment variable, and everything is optional. Invalid values
stop startup with an explanation rather than falling back to a default.

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `:3625` | Address to bind the web UI to. |
| `DOCKER_SOCKET` | `/var/run/docker.sock` | Path to the Engine socket. A `unix://` prefix is accepted. |
| `REFRESH_INTERVAL` | `5s` | How often the overview reloads. Minimum `1s`. |
| `STOP_TIMEOUT` | `10s` | Grace period before a container being stopped is killed. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `TELEGRAM_BOT_TOKEN` | – | Unset means the bot does not run at all. |
| `TELEGRAM_ALLOWED_CHAT_IDS` | – | Comma-separated chat ids. **Required** once a token is set. |
| `DOCKONTROLER_SELF_ID` | autodetect | docKontroler's own container id or name. Only needed if autodetection fails. |

## HTTP endpoints

Two of them are meant to be used from outside the page:

| Endpoint | Returns |
| --- | --- |
| `GET /api/containers` | The whole overview as JSON: groups, state, restart policy, whether a recreate is possible. Nothing in the UI needs it — it is there so you can script against docKontroler. |
| `GET /healthz` | `ok` with status 200 while the process is up. It deliberately does not touch the Docker socket, so it still answers when the daemon is unreachable: it tells you the container is alive, not that Docker is. |

`GET /partials/containers` serves the HTML fragment the auto-refresh swaps in. That
one is an internal detail of the page, not an interface to build on.

Both are unauthenticated, like everything else here. The state-changing routes are
POST-only and refuse cross-origin browser requests — see [Security](#security).

**There is no container healthcheck**, and it is not an omission that can be fixed in
compose: the runtime image is distroless, so it has no shell, no `curl` and no `wget`
for a `healthcheck:` to run. Point an external monitor at `/healthz` instead. A
Docker-level healthcheck would need the binary to grow a self-check flag it could
call itself.

## Restart policies

The **Autostart** control on each card maps to Docker's restart policies:

| Setting | Docker policy | Behaviour |
| --- | --- | --- |
| **never** | `no` | Stays down after a reboot or a daemon restart. |
| **unless stopped** | `unless-stopped` | Comes back on boot, unless you stopped it yourself. |
| **always** | `always` | Comes back on boot even if you stopped it yourself. |

It is a dropdown rather than three buttons because it is one setting with one value,
and three buttons on thirty cards turn a page into a wall of controls. Choosing a
value applies it; without JavaScript the form shows its own submit button. A policy
Docker reports but this tool does not offer — `on-failure` with its retry count —
is displayed as the current value and cannot be selected.

Changing a policy takes effect immediately, works on stopped containers, and never
starts or restarts anything by itself.

**Where the setting lives.** Nothing is written to a file, and no compose file is
touched: the policy is part of the container's own configuration in the Docker
daemon, changed through the Engine API. It therefore survives a daemon restart, a
reboot, and a recreate through this tool, which carries the whole host configuration
over.

What it does not survive is the container being rebuilt **from its compose file**,
because that constructs a new container and a `restart:` line in the yaml wins again.
Worth knowing precisely, since the difference decides whether your setting is still
there tomorrow:

- **Leaves it alone:** a reboot, a daemon restart, `docker start`/`stop`/`restart`, a
  recreate through this tool, and — perhaps unexpectedly — redeploying an *unchanged*
  Compose stack. Compose compares the `com.docker.compose.config-hash` label against
  the yaml, and changing a policy through the Engine API does not touch that label, so
  it sees no drift and keeps the container.
- **Overwrites it:** editing the yaml and running `docker compose up -d`, a
  `--force-recreate`, a `down` followed by an `up`, or a redeploy with "re-pull image"
  enabled. Also `docker compose down` on its own, since the container is gone
  afterwards.

So the buttons are the way to change a policy now, and the yaml is the value you get
after the next rebuild. If you want the two to agree permanently, put your choice in
the yaml as well — and the row tells you which file that is.

**The row names the file.** Compose records the files it was invoked with on every
container it creates, so a Compose-managed row shows the one to edit, with the full
path in its tooltip. Where several files were used, the last is shown, because later
files override earlier ones and that is where a `restart:` line actually wins.

Read that path as Compose wrote it, not as a location on your host. A stack deployed
through Portainer reports something like `/data/compose/7/docker-compose.yml`, which
is inside the Portainer container — the file behind Portainer's own stack editor. Edit
it there rather than hunting for it on the host.

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
- it removes itself when it stops (`--rm`, `AutoRemove`) — the daemon deletes such a
  container, along with its anonymous volumes, the moment the recreate stops it, so
  there would be nothing left to restore if a later step failed;
- it is docKontroler itself.

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
docKontroler logs that and does not delete the webhook, since it may belong to
something else you run.

## Security

Read this part properly. It is the main trade-off of the whole project.

**There is no authentication.** Anyone who can reach the page can start, stop and
replace every container on the host. That is the design — it is what makes it fast
to use — and it is only reasonable on a trusted network.

**Ordinary cross-site posts are refused, though.** Being reachable is enough to
authorise an action, which would otherwise make the browser of anyone on your
network a way in: a form on any page they open could post to docKontroler, and
binding to the LAN would not help. So actions are refused when the browser reports
them as coming from another origin — `Origin` on a plain-HTTP address, plus
`Sec-Fetch-Site` where the browser sends it. This is not a login and does not behave
like one: the page's own buttons send values that match, and `curl` or a script sends
no such headers at all, so both keep working unchanged.

It does not defeat **DNS rebinding**, where an attacker points a hostname of their
own at your server's address so that their page counts as same-origin. Stopping that
means rejecting requests whose `Host` is not the name this instance answers to,
which is a filter for your reverse proxy — the points below matter more than this
one anyway.

**The Docker socket is root on the host.** Anything that can talk to
`/var/run/docker.sock` can start a privileged container that mounts `/`. That
applies to docKontroler and to anyone who reaches its page. Mounting the socket
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
#
# golang:1 rather than golang:1-alpine: -race needs cgo, and the Alpine image has no
# C compiler, so it fails there with "-race requires cgo".
docker run --rm -v "$PWD":/src -w /src golang:1 \
	sh -c 'gofmt -w . && go vet ./... && go test -race ./...'

# see what the formatter changed
git diff

# build and run
./rebuild.sh && docker compose up -d && docker compose logs -f
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
