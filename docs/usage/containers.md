---
title: Run in a container
description: Build and run stock Docbank with environment settings, mounted secrets, and a persistent vault.
---

# Run in a container

The repository Dockerfile runs stock Docbank as UID/GID `1000:1000` on Debian
with glibc and the compiled web application. CI builds and starts this image on
Linux amd64, and the repository does not publish an image.

The image listens on `0.0.0.0:8485`. Supply an explicit API key or startup
fails. The ordinary binary still defaults to loopback with an ephemeral key.

## Choose the network boundary

Binding to a non-loopback address means you are opting in to sending the API
key and vault contents across that network. Docbank serves plain HTTP and does
not authenticate remote servers or encrypt traffic. Use a private container
network you trust, an encrypted tunnel, or an HTTPS proxy, and do not expose
this listener directly to an untrusted network. An API key grants full
authority over one vault, and Docbank remains a single-owner service.

The Host allowlist rejects unconfigured names, including on wildcard binds,
though it does not authenticate peers. List the name and port clients use, such
as `docbank:8485`, in `DOCBANK_ALLOWED_HOSTS`.

Server-path ingest and preflight, backup restore, and explicit backup
repository paths still require a loopback peer, regardless of its key, Host, or
forwarding headers. Remote clients send document bytes to
`POST /api/v1/uploads` and back up only to the configured `[backup] repo`. They
cannot use the API to read or write other container-local paths. When you need
local imports or restores, run them with `docker exec`. A proxy on loopback
makes its upstream requests local, so its own access policy must protect these
routes.

## Build and start

From the repository root, build the image, create the network, volumes, and
secret file, and start the container:

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
  docbank:local
```

You do not need a config file, shell entrypoint, or forwarding sidecar, and
this example publishes no host port. If a local client needs one, add a
loopback-only port mapping such as `-p 127.0.0.1:8485:8485`.

If you bind-mount `/data`, make sure UID/GID `1000:1000` can write to it.
Docbank makes the vault owner-private. Keep configuration and secret mounts
outside the build context, and keep secret values out of image layers and
command flags. Mounted secrets must be regular files owned by UID `1000` with
mode `0400` or `0600`. Docbank refuses symlinks and files accessible to other
users. Restart the daemon after you rotate a key.

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

To access the vault, supply the same key through an owner-controlled client
secret. This example lets curl read the header from standard input:

```bash
{ printf 'Authorization: Bearer '; sudo cat secrets/api-key; } | \
  docker run --rm -i --network docbank-private curlimages/curl:latest \
    --fail --header @- http://docbank:8485/api/v1/info
```

The URL sends `Host: docbank:8485`. Without the key, the info request returns
`401`. An unconfigured Host returns `403`, even with a valid key. `/health`
checks liveness and does not prove that every background job is healthy.

## Keep ownership and backups coherent

`DOCBANK_LOCK_DIR` names the persistent lock registry outside the vault, and
the image sets it to `/var/lib/docbank-locks` by default. Every process that
can own or restore overlapping vault trees must share that registry and the
same filesystem view. Persist the registry even for a single container so
recreating it preserves the lock identities. When separate containers
participate, mount the same persistent registry rather than running independent
registries against shared vault trees. Stop all participating processes before
changing its location, and never delete its identities.

The Dockerfile supplies the ordinary binary. To run a local command inside the
running container, use `docker exec docbank docbank vault info`. It discovers
and authenticates to the daemon through the private runtime record. Background
start and foreground run follow the same ownership rules. Follow the documented
[backup and restore workflow](backup.md), and save configuration separately.
For startup settings and secret-file failure behavior, see
[Configuration](../configuration.md#environment-variables).

## Run MCP in the same container

The daemon can host authenticated, read-only MCP tools at `/mcp` on its API
port. Create a second random secret owned and readable by UID `1000`, mount it
at `/run/secrets/mcp-bearer` when you create the container, and add these
environment settings to the `docker run` command:

```text
-e DOCBANK_MCP_HTTP_ENABLED=true
-e DOCBANK_MCP_HTTP_TOKEN_FILE=/run/secrets/mcp-bearer
-e DOCBANK_MCP_HTTP_ALLOWED_HOSTS=docbank:8485
```

A client on `docbank-private` connects to `http://docbank:8485/mcp` with that
bearer and sends `Host: docbank:8485`. The hosted catalog is read-only, with no
daemon flags to enable MCP writes, and you do not need a second process, port,
or proxy. Because the image binds a network address, hosting stays off until
you set `DOCBANK_MCP_HTTP_ENABLED`. The MCP bearer authorizes only `/mcp`, and
`/mcp` rejects the API key.

An invalid selected credential, or a bearer equal to the effective daemon API
key, stops daemon startup. When you stop the daemon, in-flight MCP reads get up
to five seconds to finish before the API closes. The container's restart policy
supervises the single daemon. Restart the daemon after you change MCP settings
or rotate the bearer.

This endpoint uses plain HTTP. Use the trusted private network above, an
encrypted tunnel, or an HTTPS proxy. For exact request headers, see
[Model Context Protocol](mcp.md). For mounted secrets and named bindings, see
the [configuration reference](../configuration.md#mcp-http-credential).
