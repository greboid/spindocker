# spindle-docker-engine

A Docker engine for [Spindle](https://tangled.sh) CI that uses standard container images instead of Nix/Nixery.

## Quick start

```bash
git clone https://tangled.org/tangled.org/core.git
cd core
git apply /path/to/add-docker-engine.patch
go build -o spindle ./cmd/spindle/
```

## Configuration

Set the `engine: docker` field in your pipeline YAML and optionally specify an `image`:

```yaml
# .tangled/workflows/test.yaml
when:
  - event: ["push", "pull_request"]
    branch: ["master"]

engine: docker
image: golang:1.23-bookworm

steps:
  - name: run all tests
    command: go test -v ./...
```

If `image` is omitted, it falls back to `SPINDLE_DOCKER_PIPELINES_DEFAULT_IMAGE` (default: `debian:bookworm-slim`).

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SPINDLE_DOCKER_PIPELINES_DEFAULT_IMAGE` | `debian:bookworm-slim` | Image used when pipeline omits `image` |
| `SPINDLE_DOCKER_PIPELINES_WORKFLOW_TIMEOUT` | `5m` | Max duration per workflow |

All standard `SPINDLE_SERVER_*` variables from upstream spindle still apply.

## How it works

The docker engine implements the same `models.Engine` interface as the nixery engine. For each workflow:

1. Pulls the specified container image (or the configured default)
2. Creates a container with the image, a tmpfs `/tmp`, and dropped capabilities
3. Clones the repo into `/tangled/workspace`
4. Runs each step via `sh -c <command>` inside the container
5. Streams stdout/stderr back as structured log lines
6. Tears down the container and network on completion

## Differences from nixery engine

- Uses user-specified container images instead of Nixery
- No `dependencies` block — pick an image with your tools pre-installed
- No Nix setup step or nix profile installs
- Uses `sh -c` instead of `bash -c` for broader base image compatibility
- Both engines can coexist — set `engine: docker` or `engine: nixery` per workflow
