---
title: Run in a container
description: Build and run stock Docbank with environment settings, mounted secrets, and a persistent vault.
---

# Run in a container

The repository Dockerfile runs stock Docbank as UID/GID `1000:1000` on Debian
with glibc and the compiled web application. CI builds and starts it on Linux
amd64. The repository does not publish an image.

The image selects `0.0.0.0:8485`. Supply an explicit API key or startup fails.
The ordinary binary still defaults to loopback with an ephemeral key.

## Choose the network boundary

A non-loopback bind is an operator opt-in to sending the API key and vault
contents across the selected network. Docbank serves plain HTTP and does not
authenticate remote servers or encrypt traffic. Use a private container network
you trust, an encrypted tunnel, or an HTTPS proxy. Do not expose this listener
directly to an untrusted network. A key grants full authority over one vault;
this is still a single-owner service.

The Host allowlist rejects unconfigured names, including on wildcard binds.
It does not authenticate peers. List the name and port clients use, such as
`docbank:8485`, in `DOCBANK_ALLOWED_HOSTS`.

Server-path ingest and preflight, backup restore, and explicit backup
repository paths still require a loopback peer, regardless of its key, Host, or
forwarding headers. Remote clients send document bytes to
`POST /api/v1/uploads` and back up only to the configured `[backup] repo`; they
cannot use the API to read or write other container-local paths. Run local
imports and restores with `docker exec` when needed. A proxy on loopback
makes its upstream requests local, so its own access policy must protect these
routes.

## Build and start

From the repository root:

```bash
docker build --build-arg VERSION=dev --build-arg COMMIT="$(git rev-parse --short HEAD)" \
  -t docbank:local .
docker network create docbank-private
docker volume create docbank-data
docker volume create docbank-locks
mkdir -p secrets
openssl rand -hex 32 > secrets/api-key
chmod 0400 secrets/api-key
# The container user must be able to read the mounted key.
sudo chown 1000:1000 secrets/api-key

docker run -d --name docbank --network docbank-private \
  --mount source=docbank-data,target=/data \
  --mount source=docbank-locks,target=/var/lib/docbank-locks \
  --mount type=bind,src="$(pwd)/secrets/api-key",dst=/run/secrets/api-key,readonly \
  -e DOCBANK_API_KEY_FILE=/run/secrets/api-key \
  -e DOCBANK_ALLOWED_HOSTS=docbank:8485 \
  -e DOCBANK_TELEMETRY_ENABLED=off \
  docbank:local
```

No config file, shell entrypoint, or forwarding sidecar is required. No host
port is published here. Add a loopback-only port mapping such as
`-p 127.0.0.1:8485:8485` if a local client needs one.

A bind-mounted `/data` must be writable by UID/GID `1000:1000`. Docbank makes
the vault owner-private. Keep configuration and secret mounts outside the
build context, and keep secret values out of image layers and command flags.
Mounted secrets must be regular files owned by UID `1000`, with mode `0400`
or `0600`; symlinks and files accessible to other users are refused. Restart
the daemon after rotating a key.

Some orchestrators mount secrets in a form these checks refuse. Kubernetes
Secret volumes expose each key as a root-owned symlink, and Docker Swarm
secrets default to `root:root` with mode `0444`. In those environments, pass
the key through `DOCBANK_API_KEY` or `DOCBANK_MCP_HTTP_TOKEN` from the
orchestrator's secret reference instead of the `_FILE` form, or set Swarm's
secret `uid` and `mode` to match the rules above.

## Check from a second container

A container on the same network can check liveness without a credential:

```bash
docker run --rm --network docbank-private curlimages/curl:latest \
  --fail http://docbank:8485/health
```

For vault access, provide the same key through an owner-controlled client
secret. This example lets curl read the header from its standard input:

```bash
{ printf 'Authorization: Bearer '; sudo cat secrets/api-key; } | \
  docker run --rm -i --network docbank-private curlimages/curl:latest \
    --fail --header @- http://docbank:8485/api/v1/info
```

The URL sends `Host: docbank:8485`. Without the key, the info request returns
`401`; an unconfigured Host returns `403`, even with a valid key. `/health`
checks liveness and does not prove that every background job is healthy.

## Keep ownership and backups coherent

`DOCBANK_LOCK_DIR` names the persistent lock registry outside the vault. Its
image default is `/var/lib/docbank-locks`. All processes that can own or restore
overlapping vault trees must share that registry and the same filesystem view.
Persist the registry even for one container so recreating it preserves the
lock identities. Mount the same persistent registry when separate containers
participate; do not run independent registries against shared vault trees. Stop all participating
processes before changing its location, and never delete its identities.

The Dockerfile supplies the ordinary binary. To run a local command in the
running container, use `docker exec docbank docbank info`; it discovers and
authenticates to the daemon through the private runtime record. Background
start and foreground run use the same ownership rules. Use the documented
[backup and restore workflow](backup.md), and save configuration separately.
See [Configuration](../configuration.md#environment-variables) for startup
settings and secret-file failure behavior.

## Run MCP in the same container

MCP runs as a separate client of the vault's daemon and uses a separate bearer.
Create a second random secret, owned and readable by UID `1000`, and mount it
at `/run/secrets/mcp-bearer` when creating the container. Add these startup
environment settings to the `docker run` command:

```text
-e DOCBANK_MCP_HTTP_TOKEN_FILE=/run/secrets/mcp-bearer
-e DOCBANK_MCP_HTTP_ALLOWED_HOSTS=docbank:7341
```

Then start the second stock process:

```bash
docker exec -d docbank docbank mcp --transport http --listen 0.0.0.0:7341
```

A client on `docbank-private` connects to `http://docbank:7341/mcp` with that
bearer, sending `Host: docbank:7341`. The read catalog is the default; choose
explicit `--allow-*` flags only when needed. For automatic process restart,
configure your service manager to supervise both commands in the same container.
No proxy or host rewriting is needed. Starting a second container against the
vault is not this recipe: MCP discovery requires its local daemon's process
identity and authenticated connection proof, as well as the shared lock registry.
See [Model Context Protocol](mcp.md) for exact request headers and credential
separation, including refusal when the daemon and MCP keys are equal.
