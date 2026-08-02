# Contributing to sqlpod

Thanks for your interest! sqlpod is a small tool with a deliberately small
surface — see [docs/open-source-plan.md](docs/open-source-plan.md) for the
design decisions (and non-goals) before proposing larger changes.

## Prerequisites

- Go 1.26+
- Docker (for image builds; any BuildKit or classic builder works)
- `kubectl` + a cluster only if you want to run the end-to-end flow

## Checks

CI runs exactly these — run them locally before opening a PR:

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
go build ./...
docker build .
```

## Testing against real databases

The unit tests cover the pure logic. For engine behavior, run a throwaway
database and point the binary at it:

```bash
docker run -d --name pg -e POSTGRES_PASSWORD=pw -p 5432:5432 postgres:17-alpine
docker run -d --name my -e MYSQL_ROOT_PASSWORD=pw -e MYSQL_DATABASE=test -p 3306:3306 mysql:9

go build -o sqlpod .
SQLPOD_CONN="postgres://postgres:pw@localhost:5432/postgres?sslmode=disable" ./sqlpod "SELECT 1"
SQLPOD_CONN="mysql://root:pw@localhost:3306/test" ./sqlpod "SELECT 1"
```

Things worth re-verifying when touching read-only enforcement (see the matrix
in the README): a read-mode `INSERT` must be rejected on Postgres/MySQL, and
the MySQL DDL-autocommit escape is expected behavior, not a bug.

For the full kubectl-exec path, deploy into any local cluster (k3s/kind) with
`manage.sh` and drive it through `query.sh` — the flow in the README works
as-is with a locally built image (patch `imagePullPolicy` to `IfNotPresent`
if the image only exists in your local daemon).

That path is also scripted: `./scripts/integration-test.sh` builds the image,
pushes it to a throwaway local registry, deploys into a throwaway namespace
alongside an in-cluster Postgres, and asserts on `query.sh` behavior (read,
write, tsv, truncation, named connections, read-only enforcement). It needs
`docker`, `envsubst`, `jq`, and a kubectl context pointing at a docker-runtime
cluster (e.g. colima with `--kubernetes`, or minikube with `--driver=docker`);
set `KEEP=1` to keep the namespace around after a run for debugging.

## Pull requests

- CI must be green; behavior changes need tests.
- Keep the zero-endpoint model intact: the Go binary must stay free of any
  Kubernetes or transport dependency — platform coupling belongs in the
  driver scripts and manifests.
- Security-relevant invariants that must not regress: write mode never falls
  back to a read connection, connection strings never appear in argv or error
  output, and `query.sh` must never gain lifecycle/credential commands.

## License

MIT — by contributing you agree your contributions are licensed under the
project's [LICENSE](LICENSE).
