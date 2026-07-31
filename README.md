# sqlpod

[![CI](https://github.com/nathanfox/sqlpod/actions/workflows/ci.yml/badge.svg)](https://github.com/nathanfox/sqlpod/actions/workflows/ci.yml)

A tiny tool for running **ad-hoc SQL queries** against a database the cluster can reach but
your laptop can't. It deploys a small Go binary into your developer namespace; you drive it over
`kubectl exec` and get results back as JSON. Designed to be driven by an AI agent (queries in,
JSON out), so it has a built-in row cap and is read-only by default.

Currently supports **SQL Server**; PostgreSQL and MySQL support is planned — see
[docs/open-source-plan.md](docs/open-source-plan.md).

## Why not just use psql/sqlcmd in a bastion pod?

You can — but sqlpod packages what that DIY setup leaves ad-hoc:

- **No network endpoint.** No HTTP server, no tokens, no port-forward. Access control is
  exactly your kubeconfig's RBAC to the namespace.
- **Read-only by default, enforced in the engine** (always-rolled-back transaction), not just
  by convention.
- **Structured output** with row caps and truncation flags, built for programmatic/AI-agent
  consumption.
- **Hardened by default**: distroless image, non-root, read-only rootfs, no shell.

## How it works

- The pod stays idle (warm) so repeated queries have no startup latency.
- Queries run via `kubectl exec ... -- /sqlpod "<SQL>"`. Access control is just your kubeconfig's
  RBAC to the namespace — there's no HTTP endpoint, no tokens, no port-forward.
- The connection string lives in a k8s secret and is injected as an env var; it is never passed on
  the command line and never logged.

### Read-only by default

Read mode is enforced at two layers:

1. **Login:** put a read-only (e.g. `db_datareader`) login in the secret's `conn-string` key. A
   write-capable login goes in `conn-string-write` (optional).
2. **Transaction:** read-mode statements run inside a transaction that is **always rolled back**, so
   even a misconfigured login can't persist changes.

Writes require `--write`, which selects `conn-string-write` and commits. If that key isn't set,
`--write` fails cleanly instead of writing through the read connection.

## First-time setup

```bash
export NAMESPACE=dev-alice                    # your developer namespace
export REGISTRY=ghcr.io/youruser              # where you push the image
# optional, only for private registries:
# export IMAGE_PULL_SECRET=my-registry-secret

./manage.sh setup-namespace                   # create the namespace
./manage.sh set-conn "sqlserver://user:pass@host:1433?database=mydb"
# optional, only if you need writes:
./manage.sh set-conn-write "sqlserver://writeuser:pass@host:1433?database=mydb"

./manage.sh build
./manage.sh push
./manage.sh deploy
./manage.sh status                            # confirm the pod is Running
```

Prefer not to build? A prebuilt multi-arch image (amd64/arm64) is published to
`ghcr.io/nathanfox/sqlpod` on every release — skip `build`/`push` and deploy it directly:

```bash
REGISTRY=ghcr.io/nathanfox IMAGE_TAG=v0.1.0 ./manage.sh deploy
```

Connection-string format is the go-mssqldb URL form:
`sqlserver://username:password@host:port?database=dbname&encrypt=true`.

### Reusing an existing secret

To point at an existing secret instead of creating `sqlpod-conn-secret`:

```bash
SQLPOD_SECRET_NAME=my-existing-secret SQLPOD_SECRET_KEY=conn-string ./manage.sh deploy
```

## Daily use

Queries go through `query.sh` — deliberately separate from `manage.sh` so it can
be handed to an AI agent without also granting deploy/delete/credential access:

```bash
./query.sh query "SELECT TOP 10 * FROM dbo.Orders"
./query.sh query --max-rows 5000 "SELECT * FROM dbo.BigTable WHERE region = 'NE'"
./query.sh query-file report.sql

# writes (requires set-conn-write):
./query.sh query --write "UPDATE dbo.Orders SET status = 'shipped' WHERE id = 42"
```

### Using with an AI agent

The two-script split lets you scope agent permissions to your comfort level:

- **Unattended or auto-approved use** (standing allowlists, background agents,
  CI): allow only `./query.sh` — e.g. `Bash(./query.sh *)` in Claude Code. Its
  worst case is running SQL, which is already governed by the read-only
  connection and your namespace RBAC; it has no deploy/delete/credential
  commands to reach.
- **Interactive use** (you approve each command as it runs): there's no harm in
  letting the agent drive `manage.sh` too — deploys and secret changes are
  visible and gated per call.

### One pod per developer (and shared read-only pods)

sqlpod is designed to be deployed **per developer**: each developer runs their
own pod in their own namespace, with their own database login in their own
secret. That gives database-side attribution (queries arrive as *your* login),
per-user write control, and a small blast radius — and the pod is cheap enough
(50m CPU / 64Mi requests) that there's no reason to share.

The boundary to understand: **one pod = one set of credentials.** RBAC controls
who can `kubectl exec` into a pod, but not which flags they pass — anyone who
can exec can use `--write` if that pod has a write key. So:

- A team *may* share a single **read-only** pod (write key never set) in a
  shared namespace — a cheap way to hand query access to a whole team or a
  fleet of agents.
- **Never set a write key on a shared pod.** Writes go through a personal pod.

### Output

Read mode:

```json
{"columns":["id","name"],"durationMs":12,"maxRows":1000,"mode":"read",
 "rowCount":2,"rows":[[1,"a"],[2,"b"]],"truncated":false}
```

`truncated: true` means the result hit `--max-rows` (default 1000) — add a filter or raise the cap.

Write mode: `{"durationMs":8,"mode":"write","rowsAffected":1}`.

Errors go to stderr as `{"error":"..."}` with a non-zero exit; the connection string is scrubbed.

## Notes / limitations (v1)

- SQL Server only for now (PostgreSQL and MySQL planned).
- Raw SQL only — no bound parameters yet.
- Write mode reports `rowsAffected` only; it does not return rows (no SELECT-after-write).
- The image is distroless (no shell). Use `query`/`query-file`, not an interactive shell. If you
  ever need to poke around inside, swap the base image to `gcr.io/distroless/static:debug`.

## Layout

| File | Purpose |
|------|---------|
| `main.go` | the CLI that runs in the pod (query execution, JSON output, read-only enforcement) |
| `Dockerfile` | multi-stage build → distroless static image |
| `k8s/deployment-template.yaml` | the sleeper Deployment (envsubst for secret name/key) |
| `query.sh` | agent-facing driver: run queries via kubectl exec |
| `manage.sh` | operator lifecycle: build / push / deploy / secrets |
| `scripts/lib.sh` | logging + kubectl helpers shared by both scripts |
| `docs/open-source-plan.md` | roadmap and design decisions |

## License

MIT — see [LICENSE](LICENSE).
