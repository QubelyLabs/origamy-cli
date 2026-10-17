# origamy-cli

`origamy` deploys and manages a self-hosted (BYOD) Origamy data plane on a
Kubernetes cluster or a plain Docker host.

## Install

The dashboard's **Connections → Connect data plane** page prints a one-liner:

```sh
sh -c "$(curl -fsSL https://v1.origamy.io/install.sh)" deploy --token dpe_xxx
```

`install.sh` downloads the latest GitHub release for your OS/arch, verifies the
binary against the release's `SHA256SUMS`, and verifies `SHA256SUMS` against a
detached Ed25519 signature (`SHA256SUMS.sig`) with the public keys embedded in
the script. It fails closed: a missing `SHA256SUMS` or `SHA256SUMS.sig`, a
signature that does not verify, or a checksum mismatch aborts the install.

One caveat: on a host whose `openssl` cannot do Ed25519 (macOS ships LibreSSL)
the signature cannot be checked, so the installer prints a red warning and
falls back to checksum verification, which catches a corrupt download but not
a forged release. Install OpenSSL 3 (`brew install openssl@3`, found
automatically) to get full verification, or set `ORIGAMY_REQUIRE_SIGNATURE=1`
to refuse instead.

The latest release is resolved from the `github.com/…/releases/latest`
redirect, not the rate-limited GitHub API. An installed CLI is only replaced by
a newer release; a newer or locally built one (`origamy dev`) is kept.
Environment knobs, set before `sh -c`:

| Variable | Effect |
|---|---|
| `ORIGAMY_VERSION=v0.1.19` | Install that release instead of the latest, replacing whatever is installed. |
| `ORIGAMY_FORCE_UPDATE=1` | Replace the installed CLI even when it is newer or not a release build. |
| `ORIGAMY_REQUIRE_SIGNATURE=1` | Refuse to install when the signature cannot be checked. |
| `ORIGAMY_OPENSSL=/path/to/openssl` | Use this OpenSSL 3 for the signature check. |

## Commands

| Command | What it does |
|---|---|
| `origamy deploy --token dpe_…` | Asks every question and runs every check first, then enrols the data plane (generating an mTLS identity locally when the control plane has a CA) and installs it. Kubernetes → Helm chart `oci://ghcr.io/qubelylabs/charts/origamy-data-plane`; Docker → compose bundle in `./origamy-dp-<id>/` (re-run inside that directory to re-deploy in place). |
| `origamy upgrade` | Moves the install to a newer release. `--enable-ai` / `--disable-ai` toggle the AI engine; `--enable-predictor` / `--disable-predictor` toggle the predictor (Kubernetes). On Docker it also refreshes the compose bundle (see [Docker bundle refresh](#docker-bundle-refresh)). |
| `origamy rollback [--to N]` | Rolls a Kubernetes install back to a previous Helm revision (not across the storage reset: see below). |
| `origamy status` | Installed version, whether a newer one is published (Kubernetes), service health. |
| `origamy uninstall [id]` | Tears the data plane down (Helm release + namespace, or compose project + volumes). |
| `origamy version` | Prints the CLI version. |

Flags of note on `deploy`: `--target auto|kubernetes|docker` (auto prefers
Kubernetes whenever kubectl reaches a cluster, Docker Desktop's included — the
install prints the kubectl context it is about to use), `--enable-ai`,
`--datastore-auth` (Kubernetes: password-protect the bundled
Redis/NATS/ClickHouse), `--version` (data-plane release to install instead of
the pin: the chart version on Kubernetes, the image tag on Docker),
`--require-signed-config` (see [Config signing](#config-signing)).

## Deploy the core first, add the rest later

A fresh install only needs the core plane (ingestion gateway, transformer,
identity resolver, segment evaluator, bulker, config-sync, portal-agent and the
bundled NATS/Redis/ClickHouse). Everything else is optional and can be switched
on later without touching a running plane:

| Piece | At install | Later |
|---|---|---|
| AI engine (orchestrator; needs the AI package + an LLM credential in the dashboard) | `--enable-ai` (default off) | `origamy upgrade --enable-ai` / `--disable-ai` |
| Predictor (conversion scoring; Kubernetes) | off | `origamy upgrade --enable-predictor` / `--disable-predictor` |
| Journeys, broadcasts, human tasks (workflow-engine + Postgres) | Kubernetes: on; Docker: the `full` profile, asked at install | Docker: add `full` to `COMPOSE_PROFILES` in `.env` and `docker compose --env-file .env up -d` |

The dependency arrow only points one way (orchestrator → workflow engine, never
back), the portal-agent only offers AI/proposal queries when
`ORCHESTRATOR_ENGINE_URL` is set, and the orchestrator's KEK is kept in `.env`
across a disable/enable cycle, so adding or removing these pieces does not
disturb the core.

## Preflight checks

- **CPU architecture.** The data-plane images are published for `linux/amd64`
  only. `deploy` refuses a cluster with no amd64 node, warns about a mixed one,
  and warns on a non-amd64 Docker host (Docker Desktop runs the images under
  emulation; a Linux arm64 host needs QEMU binfmt first).
- **ClickHouse schema (Docker).** The control plane serves one
  `clickhouse-init.sql` — the schema of its current build — while the install
  pins the images to a release. The two diverged in the 2026-08 storage reset
  (payload columns became native JSON): releases up to 0.1.17 cannot write the
  current schema, so `deploy` refuses that pairing instead of bringing up a
  plane that looks Connected and drops every event at insert, also in place
  over a plane whose events table that release does write (`deploy
  --version 0.1.17` over the String table with a control plane serving the
  JSON schema), since the served schema is what the deploy installs. For a
  plane that already runs, what its ClickHouse holds decides (see the storage
  reset below).

  **Which checks run depends on the move.** The heavy ones run only on a move
  that can touch the events table: it crosses the storage reset (the release
  the plane runs and the target write different tables, up or down), a reset
  is recorded in `.env`, either release is not a numbered one (`main`: the
  projects of CLI v0.1.15 to v0.1.18 run it), or ClickHouse, peeked at, holds
  another table than the target writes. Any other move (0.1.19 → 0.1.20, a
  rollback within a generation, an in-place `deploy` of the release the plane
  runs) behaves like CLI v0.1.20: no new refusal for want of an answer and no
  new failure after the commit point. For it, ClickHouse is only asked if it
  already answers (never started), nothing is pulled to compare images, and
  what the checks after `up -d` find (a ClickHouse that does not answer, a
  data-plane container on another release of the same generation) is a
  warning. Two things fail it all the same, because they are the very
  mismatch the checks exist for: an events table of another generation than
  the target writes, or none (recorded as a reset for the next run to
  finish), and a data-plane image of the other generation (an override
  pinning 0.1.17 under 0.1.20: refused before anything is changed, found
  after `up -d` otherwise, from the reference the container was made from).

  **On a move that can touch the table**, `upgrade` and in-place `deploy`
  start ClickHouse when it does not answer, and refuse when it still cannot be
  asked; `upgrade` compares, right after the pull and before anything is
  changed, what every data-plane service the configuration starts would run
  with the release; the containers of data-plane services whose profile is
  off are removed before `up -d`; and after `up -d` both check that
  `events.events` exists and is the generation the release writes, and that
  EVERY container of the compose project (found by its
  `com.docker.compose.project` label, under the name the CLI pins) that runs
  a data-plane image runs the release's, ClickHouse being 25.3 or newer
  under the JSON table (as its image tag says, or the server itself when the
  tag names no version). When either is not so, the run fails instead of
  reporting success, and says what can cause it. The details:

  - A data-plane image is told by the reference its container was created
    from (an image of `origamy-data-plane/`, whichever service it belongs
    to: a `bulker-worker-2` an override file adds counts), by its service's
    name (the bundle's data-plane services), or by its ID, when that is the
    ID of a data-plane image on this host (a mirror's retag of one).
  - A container is the release's when it runs the same image, by ID, as the
    release's own reference
    (`ghcr.io/qubelylabs/origamy-data-plane/<service>:<release>`) names,
    whatever tag its reference carries (a tag can name another image than it
    did when the container was made), or the image has an
    `org.opencontainers.image.version` label naming the release. Only
    running (or restarting, or paused) containers count; an exited one writes
    nothing.
  - An image pinned by digest (`<repo>:<tag>@sha256:…`) is judged by the
    digest, which must be the ID or a manifest digest of the release's image:
    compose runs the digest and ignores the tag next to it, so changing the
    tag text alone does not pass.
  - A service the configuration scales to 0 (`scale: 0`,
    `deploy.replicas: 0`) runs nowhere on purpose: it needs no container, is
    reported as scaled down, and its image is not judged.
  - **Known limitation: registry mirrors without access to ghcr.io.** The
    release's image is looked at on the host and pulled when the host does
    not have it, so a mirror's image can be compared with it. A host that
    cannot reach ghcr.io cannot make the comparison, and a move that can
    touch the events table is refused before anything is changed. To go on,
    put the release's image on the host under its ghcr.io reference: `docker
    pull` it where ghcr.io is reachable and carry it over (`docker save` |
    `docker load`), or, once you have checked that the mirror's image is the
    release's (the digest `docker image inspect` shows is the one ghcr.io has
    for that release), tag it: `docker tag <mirror image>
    ghcr.io/qubelylabs/origamy-data-plane/<service>:<release>`. The CLI then
    compares the images it has. An ordinary move is not refused.
  - **Known limitation: other platforms and unknown names.** Images are
    compared by ID, so a service the configuration pins to another platform
    than the host's (`platform:`) does not match the image the host pulls for
    the release, and is refused until its release image is on the host under
    that platform. A mirror's image under a service name the CLI does not
    know is recognised as a data-plane image only when the host has an image
    of that ID under a `origamy-data-plane/` reference.
  - What a container runs is what `docker inspect` says it was created from
    (Compose before 2.15 lists no image).

  A `--version` that is not a numbered release (`main`, `production`,
  `sha-…`; the edge channel's `:staging` writes the JSON table) is refused by
  `upgrade` and `deploy` before they start: which events table those images
  write is not known (they carry no version label), so no run could check it.
- **The release and the project `docker compose` runs (Docker).** Every
  `docker compose` command the CLI runs in a project gets `DP_IMAGE_TAG=<the
  release it is for>` in its environment: compose takes a `DP_IMAGE_TAG`
  exported in the shell over `.env`, so one left there by `set -a; .
  ./.env` cannot start another release (the CLI warns that a plain `docker
  compose up` from that shell would). Each one also pins the project's name
  (`-p`): `COMPOSE_PROJECT_NAME` in `.env`, else the top-level `name:` of
  `docker-compose.yml`, else the directory's name, as `origamy deploy` named
  it. It leaves `COMPOSE_PROJECT_NAME`, `COMPOSE_FILE`, `COMPOSE_PROFILES`
  (and the other `COMPOSE_*` settings of which project and files it is)
  exported in the shell out, so compose reads them from `.env`: a name
  exported for another project can never point `uninstall`'s `down -v` at
  it, nor a shell's profiles undo `--enable-ai`. A plane whose containers
  all run under another name (a `COMPOSE_PROJECT_NAME` exported when it was
  deployed) is refused until that name is in `.env`. So is one with no
  container at all (it was taken down with `docker compose down`) and no
  volume under the pinned name while the data volumes of a plane like it
  (`clickhouse-data`, `dp-redis-data`, `nats-data`, `dp-postgres-data`) exist
  under another, unless that project runs from another directory that still
  exists: the refusal comes before anything is created, because `up -d` under
  the pinned name would make a new, empty plane (a ClickHouse without the
  history) beside them, and the upgrade would report it done. It names the
  volumes and the project, and says to put `COMPOSE_PROJECT_NAME=<that name>`
  in `.env` (or, when the variable in the shell names another project, to
  unset it, and start the plane under its own name first if it has no data to
  keep). `uninstall` removes only the plane's own containers and volumes.
  `down -v` removes every container and volume labelled with the project's
  name, so `uninstall` is refused while a container of that name was started
  from another directory that still exists; one whose directory is gone is
  taken for this plane's (a project that was moved) and removed with the
  rest, with every volume carrying the name, those the compose file does not
  declare too. When the plane's data volumes exist under another name than the
  pinned one, `uninstall` says which before it asks, does not call that data
  removed (it lists the volumes that stay, and the `docker volume rm` to
  run), and removes the directory, and the `.env` with the passwords the data
  was created with, only once you confirm.
  `.env` is read as compose reads it where the CLI decides what compose will
  do with a value (`COMPOSE_PROFILES`, `COMPOSE_PROJECT_NAME`,
  `COMPOSE_FILE`, `DP_IMAGE_TAG`, the control plane's URL): the last of
  several `KEY=` lines winning, quotes and `export` handled and an inline
  `# comment` dropped (a `${VAR}` in a value is not expanded); the
  generated secrets are carried forward as they are written. Setting a key
  leaves one line, the others kept as they were.
  `.env` is written by renaming a file written next to it over it, so a run
  that stops half-way never leaves it cut short, and the new file keeps the
  owner and group of the old one (`sudo origamy upgrade` does not leave it to
  root). Two cases cannot be done that way and are **not atomic**: a
  hard-linked `.env` (renaming would break its other links), and one whose
  owner the user cannot give the new file. Those are written over in place:
  the new text first, padded with blank lines to the old length, flushed,
  and only then cut to size and flushed again. A run killed, or a machine
  lost, after that first write leaves the new `.env` followed by blank
  lines, never a key of the old file behind the new text; one lost during
  the write itself can leave a mix of the two, which this cannot prevent.
- **Profiles (Docker).** `origamy deploy` and `upgrade` manage three
  profiles: `full` (journeys, broadcasts, tasks), `agentic` (the AI engine)
  and `ingress` (HTTPS). Every other profile in `COMPOSE_PROFILES` (the
  bundle's log stack, `logs` and `loki`, or the operator's own) stays as it
  is, in `.env` and running: an in-place `deploy` writes the answers for the
  three and keeps the rest in the order they had. compose keeps the services
  of a profile that is off in the project, so `up -d --remove-orphans` leaves
  their containers running on the release they were started with. When a run
  turns a profile off (`upgrade --disable-ai`, or "no" to journeys or the AI
  engine on an in-place `deploy`), the services only that profile ran are
  stopped and removed (`docker compose rm --stop --force`, with the old
  profiles on) before `up -d`; one another profile still runs (Postgres,
  with `full` or `agentic`) is kept. On a move that can touch the events
  table, the containers of data-plane services whose profiles are all off
  are removed whoever turned the profile off (a hand edit of
  `COMPOSE_PROFILES`, an earlier run whose removal failed, a fresh deploy
  over an old plane's containers), so the plane that runs is the one the
  summary describes; a removal that fails is warned about with the command,
  and the check after `up -d` fails the run on the container. On any other
  move they are left alone (a warning names one on another release).
- **In-place deploy (Docker).** Re-running `deploy` in a project downloads
  the bundle into a directory inside it, runs every check (and the
  questions) and redeems the token first, and only then swaps the files in,
  each one it replaces backed up as `<file>.bak-<release>`. A refused deploy,
  or one whose enrollment fails, leaves the project byte for byte as it was.
  A project whose `docker-compose.yml` (or `.env`) no longer parses, an edit
  gone wrong, has its ClickHouse asked through the container compose made
  (found by its `com.docker.compose.project`/`service` labels; a stopped one
  is started as it was) instead of the run stopping there: `deploy` and
  `upgrade --replace-bundle` replace a compose file that does not parse with
  the release's validated one.
- **One run at a time (Docker).** `upgrade`, `deploy` and `uninstall` hold
  the operating system's advisory lock on `.origamy.lock` in the project
  directory (`flock`; `LockFileEx` on Windows), which the file's pid, host,
  start time and command name for the message another run is refused with.
  The lock goes with the process that holds it, however that ends, so a run
  that was killed leaves no stale lock (at most the file, which the next run
  simply locks again, and removes when it is done, refused or not; what such
  a file says is not checked). Where the file system has no advisory locks
  (`flock` answers `ENOLCK`, `EOPNOTSUPP` or `ENOSYS`: NFS without a lock
  manager, some SMB and FUSE mounts), the lock file itself is the lock,
  created with `O_EXCL` and removed by its run. A file such a run left
  behind (killed) is taken over when it names a process on this host that no
  longer runs, or has said nothing for 5 seconds; taking it over is
  serialized by a second `O_EXCL` file, `.origamy.lock.takeover`, so two
  runs that find it stale at once cannot both get the lock. One that names a
  running process, or a process on another host (which cannot be checked),
  refuses the run, and the message says to remove the file once that run is
  gone. Ctrl-C, SIGTERM and SIGHUP end an `upgrade` or a
  `deploy` with exit code 130 once the step under way (a write of `.env`,
  the swap of the bundle files, the reset's write of `clickhouse-init.sql`)
  is through, removing the bundle download (also one still downloading) and
  the lock file. A signal the CLI was started with ignored stays ignored:
  `nohup origamy upgrade` over SSH outlives the session.

## What a deploy does with secrets

- The enrollment token (`dpe_`) carries only a one-time handle; the real
  credential is redeemed over HTTPS (`/v1/byod/enroll/resolve` or, with a CA,
  `/v1/byod/register` which also signs a locally generated CSR). The private key
  never leaves the host.
- Kubernetes: the bearer token, mTLS identity and any external ClickHouse
  password go into pre-created Secrets (`origamy-byod-token`,
  `origamy-byod-identity`, `origamy-clickhouse`), applied as manifests over
  kubectl's stdin — never through `helm --set` (which would persist them in
  release history) and never as `--from-literal` arguments (which sit in `ps`
  and audit logs while kubectl runs). The ClickHouse password is typed with
  echo off.
- External ClickHouse (Kubernetes, release 0.1.19 or newer): `deploy` asks
  for the host, TLS, native port, user and password; older releases only get
  the bundled ClickHouse, because their charts cannot authenticate to another
  server or create its schema. The chart embeds the password in the services'
  connection URL, so it may only contain letters, digits and
  `` -._~!$&'()*+,;=:@ ``. `upgrade` refuses to move such a plane to a chart
  older than 0.1.19.
- Docker: datastore passwords (Redis, NATS, ClickHouse, Postgres) and, with AI
  on, the orchestrator KEK + API token are generated on the host into `.env`
  (mode 0600) and reused on re-deploy. Nothing generated here is sent to Origamy.

## Config signing

The control plane signs the config it sends; a data plane holding the
control plane's ed25519 **public** key rejects config whose signature does not
verify. Enrollment hands the key out (`csk` from `/v1/byod/enroll/resolve`,
`config_signing_key` from `/v1/byod/register`); a control plane that does not
sign, or predates handing the key out, sends none and nothing below applies.

- **Release gate.** Data-plane releases before 0.1.19 (`minSigningVersion` in
  `cmd/origamy/cmd/signing.go`) check the signature over the wrong field and
  reject *all* config when given the key. The CLI therefore only hands the key
  to the services on 0.1.19 or newer (numbered releases only; the edge
  `:staging` images never get it automatically). On an older release it is
  only stored: Docker keeps it as `ORIGAMY_CONFIG_SIGNING_KEY` in `.env` with
  `CONFIG_SIGNING_KEY` blank; Kubernetes keeps it in the
  `origamy-config-signing` Secret, which older charts never read.
- **Upgrade.** `origamy upgrade` to 0.1.19 or newer applies the stored key
  (Docker: `CONFIG_SIGNING_KEY`; Kubernetes:
  `controlPlane.existingSecret=origamy-config-signing`). On Docker, moving back
  below 0.1.19 blanks `CONFIG_SIGNING_KEY` again; on Kubernetes, `upgrade`
  refuses to move a plane that verifies to an older chart (`origamy rollback`
  restores a whole earlier revision instead).
- **No stored key.** There is no control-plane endpoint the plane's
  credentials can fetch the key from, so a plane deployed without one gets it
  by re-deploying with a fresh token, or by hand with the key from the
  dashboard's install snippet: `CONFIG_SIGNING_KEY=` in `.env` (Docker), or
  `origamy upgrade --set controlPlane.publicKey=<key>` (Kubernetes).
- **Strict mode** (`CONFIG_SIGNING_REQUIRED` / `controlPlane.signingRequired`)
  makes the services refuse to start without a key instead of running
  unverified. The CLI never turns it on by itself: pass
  `--require-signed-config` to `deploy` or `upgrade` once the plane runs
  cleanly with the key. It is refused below 0.1.19 and when no key is known.
  Turn it off again by editing `.env` (Docker) or with
  `origamy upgrade --set controlPlane.signingRequired=false` (Kubernetes).

The key is public (it can check config, never sign it), so it is safe in
`.env` and in Helm values; only the control plane holds the private half.

## Versions

The CLI pins one data-plane release for fresh installs (`helmVersion` in
`cmd/origamy/cmd/deploy.go`): the Helm chart version on Kubernetes and the image
tag (`DP_IMAGE_TAG`) on Docker, since images and chart share a version.
`origamy upgrade` resolves the latest published chart from the registry
(Kubernetes) or moves Docker installs to the CLI's pinned release; on
Kubernetes it re-applies the release's own values from an export (never
`--reuse-values`, which breaks when the target chart has defaults the old
release never set). `--channel edge` tracks moving images instead — `:staging`
on Kubernetes and on Docker (the only moving tag every service is rebuilt
under) — and is not for production.

Two release boundaries the CLI knows about:

- **Charts before 0.1.18** ship a config-sync whose health check treats the
  first telemetry push as liveness, so the chart's default probe kills it
  before it can report healthy. `deploy` and `upgrade` give those charts a
  slower liveness probe; the overrides are dropped again on the first
  upgrade to 0.1.18 or later.
- **0.1.17 → 0.1.18 (storage reset)**: ClickHouse moves to 25.3 and the
  events table is recreated with native JSON columns, which discards the
  event history collected so far (profiles, traits, segments and journeys
  are kept). `origamy upgrade` explains this, asks for a typed `yes`
  (`--yes` skips it), then recreates the table itself. On Docker
  the running ClickHouse decides, never the tags or `clickhouse-init.sql`
  (projects deployed by CLI v0.1.15–v0.1.18 run `DP_IMAGE_TAG=main`, and an
  earlier upgrade or deploy can have moved the files on): for a release that
  writes JSON, String payload columns cross the reset (asked first) and a
  missing events table is created with the new schema (nothing is dropped,
  so nothing is asked). A table that already has the JSON columns is never
  dropped, and moving it to a release before 0.1.18 is refused, as is an
  events table of any other type. On a move that can touch the table (see
  the preflight checks above) a ClickHouse that does not answer
  (stopped, hung, no container) is started with `up -d clickhouse` on the
  release and compose files the project runs now and asked again; when it
  still cannot be asked, the upgrade is refused rather than guessing. On any
  other move ClickHouse is only peeked at, and one table of another
  generation than the target writes, or none, turns it into this one. The
  reset asks ClickHouse again once it is up on the new bundle and moves the
  table aside only if it is the String one, and only when
  `clickhouse-init.sql` is the new schema (a served schema of neither
  generation is refused before anything is touched). Moving aside is
  `RENAME TABLE events.events TO events.events_pre_reset_<unix time>`, never
  a `DROP` of `events.events`: the init then creates the new table, and only
  once ClickHouse reports it with the JSON columns (the bundle's init service
  exits 0 even when a statement fails) is the renamed table dropped, and only
  while it has the String columns. One that turns out to have the JSON
  columns (the table changed between the check and the move) is kept, with
  the `EXCHANGE TABLES` that puts it back; one that cannot be dropped (over
  ClickHouse's `max_table_size_to_drop`, say) is kept, with the `DROP` to run.
  A reset that fails after the move keeps the old data in the renamed table
  and says so; the next run creates the new table without moving anything
  again and then drops what the failed run moved aside (the operator agreed
  then; it never drops a renamed JSON table). `RENAME TABLE` was checked on
  the bundle's `ReplacingMergeTree` events table on ClickHouse 24.8 and on
  25.3 (the 24.8 volume under 25.3, as the reset finds it).
  The reset needs ClickHouse 25.3, so it only runs when the compose
  configuration the project would run (override files and `COMPOSE_FILE`
  included, as `docker compose config` renders it) has `clickhouse-server`
  25.3 or newer with a version tag; otherwise the upgrade is refused before
  anything is touched or asked. Without the target's bundle (`--keep-bundle`,
  another served release) the control plane's current schema is written to
  `clickhouse-init.sql`, which is backed up first and never written through a
  symlink. The reset is recorded in `.env` as `ORIGAMY_RESET_PENDING=<release>`
  before anything is moved, and `DP_IMAGE_TAG` moves only once it is
  through: a reset that fails half-way is finished by the next
  `origamy upgrade` (or in-place `origamy deploy`) with a release that writes
  the JSON schema, which asks again when it moves the table aside and
  never reports the data as untouched. When ClickHouse already holds the JSON
  table, the record is cleared without asking anything. Either
  way `clickhouse` (and `caddy` with the ingress profile) are restarted, since
  the run that began the reset may have replaced the files they mount.
  Every upgrade, reset or not, ends with the check after `up -d`: a missing
  or String table under a release that writes JSON fails the run and is
  recorded as `ORIGAMY_RESET_PENDING`, so the next run finishes it. (A
  ClickHouse that does not answer fails a move that can touch the table; any
  other move only warns.)
  The commit point of a Docker upgrade is the pull and, across the reset,
  the reset itself: until both went through, `.env` is left byte for byte as
  it was but for that record, and `DP_IMAGE_TAG` and the `--enable-ai` /
  `--disable-ai` toggle (`COMPOSE_PROFILES`, the AI engine's secrets) reach
  `docker compose` through its environment only; `.env` takes them once the
  run is through. The bundle files the reset itself needs (the compose file
  with ClickHouse 25.3, the new schema) are swapped in after the pull and
  the record, each one replaced backed up as `<file>.bak-<release>`, and a
  reset that fails after that keeps them with the record: putting the 24.8
  compose file back over a volume ClickHouse 25.3 has opened is not safe to
  start, and the next run finishes the reset with them.
  **Kubernetes.** (Chart 0.1.18 was never published: 0.1.19 is the first chart
  on the new table, and the one messages name.) The safety machinery below
  runs only on a move that can touch the events table: one that takes the data-plane images across the
  reset (the installed release writes String and the target JSON, or the
  other way round, as helm's own record of the installed and the target
  images says; an unnumbered image tag counts as "can't tell"), or one made
  while a reset is recorded as unfinished. Every other upgrade, rollback and
  deploy (0.1.19 → 0.1.20, 0.1.20 → 0.1.19, 0.1.16 → 0.1.17, `--enable-ai`,
  a `--set`) behaves as before the reset existed: it needs
  no more permissions than helm and reading the release's objects, never
  asks ClickHouse anything, and ends with a look at the images the
  Deployments, StatefulSets and pods run (services on both sides of the
  reset, or ClickHouse older than 25.3 under the JSON images, fail it).
  Where the user may not list those objects, or read ConfigMaps, the run says
  so and goes on. Such a move can only be told from helm's state: a plane that
  an old CLI left on JSON images over a String table (the chart on the target
  version, no record) says "Already on …" as before; `origamy upgrade
  --version <chart>` from the previous chart, or a record left by this CLI,
  is what checks it.

  A move that can touch the table decides from what ClickHouse holds, never
  the chart versions. ClickHouse is asked in its pod (found by the chart's
  labels; **create on `pods/exec` in the release namespace**, a missing
  permission is named and not waited for, a pod that is not ready is waited
  for 30 seconds and its state named) or, for an external ClickHouse, through
  a short-lived Job made from the chart's own schema Job (create and delete
  on `jobs`, get on `pods/log`; a release with `clickhouse.initSchema=false`
  has no such Job, so such a move is refused and the table has to be checked
  by hand). The run holds a lease while it works (ConfigMap
  `origamy-reset-lock`: created if absent, so one run wins, and taken over
  once it is 45 minutes old), and is refused before anything changes when it
  cannot take it. Refused too, before anything changes: a `--version` or a
  rollback below 0.1.19 over the JSON table, a rollback or a deploy onto the
  String table (only `origamy upgrade` resets it), images whose tag is not a
  numbered release or that are pinned by digest alone (`<service>.image.digest`
  wins over any tag: clear it with `--set <service>.image.digest=` so the
  tag decides), services given tags of both sides of the reset, a ClickHouse
  image older than 25.3 under the JSON images (an image whose repository is
  not `clickhouse-server` is accepted: the reset asks the server its
  version), a rollback that switches between the bundled and an external
  ClickHouse. An external ClickHouse is whichever one `clickhouse.host` (or a
  DSN) names when it is not the bundled Service, whatever `clickhouse.enabled`
  says; it is never reset, and when the chart's own ClickHouse also runs
  beside it the move that would need a reset says so.

  Across the reset the upgrade **records the reset first**, in the release
  namespace (ConfigMap `origamy-reset-pending`: the target, whether the
  operator agreed to discard the String table, and the name it moves to), so
  a user who cannot write it moves nothing, and a `helm upgrade` that fails
  after it applied the chart is finished by the next run. Once the chart is on
  the target and the ClickHouse StatefulSet's rollout is through on 25.3 or
  newer, it moves a String table aside with the same `RENAME TABLE` (if the
  client sees an error it asks what ClickHouse holds before saying whether the
  table moved), applies the chart's schema from its ConfigMap in the
  ClickHouse pod, checks for the JSON table and only then drops the renamed
  one, and only that one. A missing table is created and nothing is dropped;
  that is recorded as a create, which is never taken for agreement to drop
  anything: if a String table turns up before the next run, the next run asks
  as for any reset. The JSON table is never touched. While the record is there
  the next `origamy upgrade` finishes the reset, even on the same chart
  version, and `origamy rollback` and an in-place deploy are refused; it is
  removed once the check after the upgrade passed, which for such a move asks
  ClickHouse again: it holds the events table that every data-plane image of
  the release writes, with ClickHouse 25.3 or newer under the JSON table.
  A fresh `origamy deploy` asks ClickHouse once, without holding the install
  up: a missing table or one the images cannot write fails the deploy (after
  the endpoint and summary are printed, with the failure marked, and a missing
  table recorded so that `origamy upgrade --version <chart>` creates it), one
  that could not be asked (no `pods/exec`, ClickHouse not up yet) is reported
  as not verified.

### Docker bundle refresh

A Docker plane also runs the bundle its control plane serves at `/byod/*`
(`docker-compose.yml`, `clickhouse-init.sql`, `clickhouse-users.xml`,
`Caddyfile`). `origamy upgrade` refreshes it only when the control plane's
`/byod/VERSION` names exactly the target release; otherwise (an older control
plane, another `--version`, `--channel edge`) it says so and only moves
`DP_IMAGE_TAG`.

- Only unedited files are replaced, each backed up as
  `<file>.bak-<release>`, named after the release the control plane handed
  out with exactly that file (`main` for bundles from before it pinned one);
  an edited file after the release it came with, as the manifest records it
  for that file (an edit kept through a refresh keeps its own release), else
  the manifest's release, else the installed tag. Unedited
  means the sha256 recorded in the project's `.origamy-bundle.json` (written
  by `deploy` and `upgrade`) or any version the control plane ever served
  (`bundle_hashes.go`, generated by `scripts/gen-bundle-hashes.sh`, which
  also maps each version to its releases). An edited file is kept and the new one
  written as `<file>.new` (removed again once the file itself is the served
  version); `--replace-bundle` replaces it anyway (backed up, and named in the
  output), `--keep-bundle` skips the refresh.
- The served `clickhouse-init.sql` must be one the target release writes.
  When the bundle for a release that writes the JSON schema carries the
  pre-reset (String) one and the refresh takes it (writes it, or finds it
  already identical in the project), the upgrade is refused before anything
  is touched; a JSON schema in the bundle of a release before 0.1.18 is not
  taken either (the refresh fails validation and the files are left as they
  are, or, when the project already holds that file, the upgrade is
  refused). A `clickhouse-init.sql` you edited and the refresh keeps is not
  refused on this ground: what ClickHouse holds decides, and a pre-reset
  file kept over a plane that already holds the JSON table is warned about.
  Across the storage reset the schema the reset applies (served or fetched)
  must be the JSON one, or nothing is touched.
- The new compose file must pass `docker compose config -q` against your
  `.env` first, and the images are pulled with it (new services' too) before
  any file is replaced, so a failed pull leaves the project as it was. A
  replaced `clickhouse-users.xml` or `Caddyfile` restarts `clickhouse` /
  `caddy`. Across the storage reset the new schema is always taken, and
  ClickHouse is brought up on the new compose file's image before the events
  table is recreated.
- The control plane URL is `CONTROL_PLANE_HTTP_URL` in `.env`; for a project
  deployed by CLI v0.1.18 or older it is taken from `CONFIG_URL` and then
  recorded as `CONTROL_PLANE_HTTP_URL`. Such an `.env` also gains
  `TELEMETRY_URL` when the compose file reads it, and its bare `CONFIG_URL`
  becomes `<control plane>/api/v1/config`: config-sync requests `CONFIG_URL`
  as it is, so with the bare URL those planes never received their config.
  No other value is changed.

## Development

```sh
make build          # bin/origamy
make test           # go test ./...
make build-all      # cross-compile + SHA256SUMS (+ SHA256SUMS.sig when ORIGAMY_RELEASE_KEY is set)
make release        # gh release create from bin/ (tag = git describe)
```

`ORIGAMY_RELEASE_KEY` must point at the Ed25519 private key (unencrypted PKCS#8
PEM) whose public half is embedded in the control plane's `install.sh`.
`install.sh` refuses a release without `SHA256SUMS.sig`, so `make release`
refuses to publish without one, and uploads it in the same `gh release create`
call as the binaries (gh keeps the release a draft until every asset is up, so
it is never `latest` unsigned). `make build-all` without the key still works
for local builds. Signing is done by `tools/sign` in pure Go, so a
release can be cut on macOS (whose system `openssl` is LibreSSL and cannot
handle Ed25519). Generate a key with
`go run ./tools/sign -h` for usage, or `openssl genpkey -algorithm ed25519`
where OpenSSL 3 is available; verify a downloaded release with
`make verify-release PUB=pub.pem DIR=<dir with SHA256SUMS and .sig>`.

## Cutting a release

`install.sh` always installs the **latest GitHub release**, so a release is what
customers get the moment it is published. The version string comes from
`git describe`, so cut it from a clean checkout of the tagged commit — a dirty
tree or an untagged commit would publish `v0.1.19-3-gabc123-dirty` as the
release name and bake it into `origamy version`. The installer treats such a
version as a non-release build: hosts that install it keep it and never
auto-update past it without `ORIGAMY_FORCE_UPDATE=1`.

```sh
git switch main && git pull --ff-only          # CI green on this commit
git tag -a v0.1.19 -m "origamy-cli v0.1.19"
git push origin v0.1.19
ORIGAMY_RELEASE_KEY=/path/to/release-key.pem make release   # build-all + sign + gh release create
```

Then verify what was actually published, with the public key the installer
trusts (first `RELEASE_PUBKEYS` entry in the control plane's `install.sh`):

```sh
mkdir -p /tmp/origamy-verify && gh release download v0.1.19 --repo QubelyLabs/origamy-cli --dir /tmp/origamy-verify
make verify-release PUB=pub.pem DIR=/tmp/origamy-verify
sh -c "$(curl -fsSL https://v1.origamy.io/install.sh)"      # no token: installs, prints the version, drops into a shell
```

After the first release signed with a rotated key, remove the retired key from
`RELEASE_PUBKEYS` in the control plane's `install.sh`.

The data-plane release a fresh install gets (`helmVersion` in
`cmd/origamy/cmd/deploy.go`) is a separate pin: bump it when the data plane
publishes a new chart + image set, and cut a CLI release so fresh installs
pick it up. With the bump, once the control plane serves that release's
bundle, regenerate the known bundle versions so Docker projects deployed by
older CLIs keep refreshing: `scripts/gen-bundle-hashes.sh <control-plane
checkout>` (`ORIGAMY_CONTROL_PLANE=<checkout> go test ./cmd/...` fails while
the table is stale).
