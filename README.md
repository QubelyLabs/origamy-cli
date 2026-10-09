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
| `origamy upgrade` | Moves the install to a newer release. `--enable-ai` / `--disable-ai` toggle the AI engine; `--enable-predictor` / `--disable-predictor` toggle the predictor (Kubernetes). |
| `origamy rollback [--to N]` | Rolls a Kubernetes install back to a previous Helm revision. |
| `origamy status` | Installed version, whether a newer one is published (Kubernetes), service health. |
| `origamy uninstall [id]` | Tears the data plane down (Helm release + namespace, or compose project + volumes). |
| `origamy version` | Prints the CLI version. |

Flags of note on `deploy`: `--target auto|kubernetes|docker` (auto prefers
Kubernetes whenever kubectl reaches a cluster, Docker Desktop's included — the
install prints the kubectl context it is about to use), `--enable-ai`,
`--datastore-auth` (Kubernetes: password-protect the bundled
Redis/NATS/ClickHouse), `--version` (data-plane release to install instead of
the pin: the chart version on Kubernetes, the image tag on Docker).

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
  plane that looks Connected and drops every event at insert.

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

## Versions

The CLI pins one data-plane release for fresh installs (`helmVersion` in
`cmd/origamy/cmd/deploy.go`): the Helm chart version on Kubernetes and the image
tag (`DP_IMAGE_TAG`) on Docker, since images and chart share a version.
`origamy upgrade` resolves the latest published chart from the registry
(Kubernetes) or moves Docker installs to the CLI's pinned release; on
Kubernetes it re-applies the release's own values from an export (never
`--reuse-values`, which breaks when the target chart has defaults the old
release never set). `--channel edge` tracks moving images instead — `:main`
on Kubernetes, `:staging` on Docker (the only moving tag every service is
rebuilt under) — and is not for production.

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
  (`--yes` skips it), then drops and recreates the table itself; on Docker it
  first re-fetches the current schema from the control plane.

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
pick it up.
