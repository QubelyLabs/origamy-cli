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
detached Ed25519 signature (`SHA256SUMS.sig`) with a public key embedded in the
script. A signature mismatch aborts the install.

## Commands

| Command | What it does |
|---|---|
| `origamy deploy --token dpe_…` | Enrols the data plane (generating an mTLS identity locally when the control plane has a CA) and installs it. Kubernetes → Helm chart `oci://ghcr.io/qubelylabs/charts/origamy-data-plane`; Docker → compose bundle in `./origamy-dp-<id>/`. |
| `origamy upgrade` | Moves the install to a newer release. `--enable-ai` / `--disable-ai` toggle the AI engine; `--enable-predictor` / `--disable-predictor` toggle the predictor (Kubernetes). |
| `origamy rollback [--to N]` | Rolls a Kubernetes install back to a previous Helm revision. |
| `origamy status` | Installed version, whether a newer one is published, service health. |
| `origamy uninstall [id]` | Tears the data plane down (Helm release + namespace, or compose project + volumes). |
| `origamy version` | Prints the CLI version. |

Flags of note on `deploy`: `--enable-ai`, `--datastore-auth` (Kubernetes: password-protect
the bundled Redis/NATS/ClickHouse), `--version` (chart version to install instead of the pin).

## What a deploy does with secrets

- The enrollment token (`dpe_`) carries only a one-time handle; the real
  credential is redeemed over HTTPS (`/v1/byod/enroll/resolve` or, with a CA,
  `/v1/byod/register` which also signs a locally generated CSR). The private key
  never leaves the host.
- Kubernetes: the bearer token, mTLS identity and any external ClickHouse
  password go into pre-created Secrets (`origamy-byod-token`,
  `origamy-byod-identity`, `origamy-clickhouse`) — never through `helm --set`,
  which would persist them in release history.
- Docker: datastore passwords (Redis, NATS, ClickHouse, Postgres) and, with AI
  on, the orchestrator KEK + API token are generated on the host into `.env`
  (mode 0600) and reused on re-deploy. Nothing generated here is sent to Origamy.

## Versions

The CLI pins one data-plane release for fresh installs (`helmVersion` in
`cmd/origamy/cmd/deploy.go`): the Helm chart version on Kubernetes and the image
tag (`DP_IMAGE_TAG`) on Docker, since images and chart share a version.
`origamy upgrade` resolves the latest published chart from the registry
(Kubernetes) or moves Docker installs to the CLI's pinned release;
`--channel edge` tracks the mutable `:main` images instead.

## Development

```sh
make build          # bin/origamy
make test           # go test ./...
make build-all      # cross-compile + SHA256SUMS (+ SHA256SUMS.sig when ORIGAMY_RELEASE_KEY is set)
make release        # gh release create from bin/ (tag = git describe)
```

`ORIGAMY_RELEASE_KEY` must point at the Ed25519 private key (unencrypted PKCS#8
PEM) whose public half is embedded in the control plane's `install.sh`; releases
built without it are published unsigned and installers fall back to
checksum-only verification. Signing is done by `tools/sign` in pure Go, so a
release can be cut on macOS (whose system `openssl` is LibreSSL and cannot
handle Ed25519). Generate a key with
`go run ./tools/sign -h` for usage, or `openssl genpkey -algorithm ed25519`
where OpenSSL 3 is available; verify a downloaded release with
`make verify-release PUB=pub.pem DIR=<dir with SHA256SUMS and .sig>`.
