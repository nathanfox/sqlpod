# sqlpod

[![CI](https://github.com/nathanfox/sqlpod/actions/workflows/ci.yml/badge.svg)](https://github.com/nathanfox/sqlpod/actions/workflows/ci.yml)

A tiny tool for running **ad-hoc SQL queries** against a database the cluster can reach but
your laptop can't. It deploys a small Go binary into your developer namespace; you drive it over
`kubectl exec` and get results back as JSON. Designed to be driven by an AI agent (queries in,
JSON out), so it has a built-in row cap and is read-only by default.

Supports **SQL Server**, **PostgreSQL**, and **MySQL** — the engine is inferred from the
connection string's scheme, no extra configuration.

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

1. **Login:** put a read-only (e.g. `db_datareader` / `pg_read_all_data` / `SELECT`-only) login in
   the secret's `conn-string` key. A write-capable login goes in `conn-string-write` (optional).
2. **Transaction:** read-mode statements run inside a transaction that is **always rolled back** —
   and on PostgreSQL and MySQL the transaction is additionally opened `READ ONLY`, so the server
   itself rejects writes inside it.

Per-engine reality:

| Engine | In-engine enforcement | Caveat |
|---|---|---|
| PostgreSQL | `READ ONLY` transaction + rollback | strongest: the server rejects writes |
| MySQL | `READ ONLY` transaction + rollback | **DDL auto-commits and escapes the transaction** — see below |
| SQL Server | rollback only (T-SQL has no read-only transactions) | the read-only login is the real guardrail |

> **MySQL caveat:** DDL statements (`CREATE`/`ALTER`/`DROP` …) auto-commit in MySQL and are **not**
> stopped by the read-only transaction or the rollback. In all engines the read-only **login** is
> the primary defense — on MySQL it is the *only* thing standing between read mode and DDL. Don't
> put a DDL-capable login in `conn-string`.

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
REGISTRY=ghcr.io/nathanfox ./manage.sh deploy   # or pin with IMAGE_TAG=v0.5.0
```

The database engine is inferred from the connection string's scheme:

| Engine | Connection string |
|---|---|
| SQL Server | `sqlserver://user:pass@host:1433?database=mydb&encrypt=true` |
| PostgreSQL | `postgres://user:pass@host:5432/mydb?sslmode=require` |
| MySQL | `mysql://user:pass@host:3306/mydb` |

A string with no `scheme://` prefix is treated as SQL Server (go-mssqldb also accepts ADO-style
`server=...;user id=...` strings). MySQL URLs are translated internally to the go-sql-driver
format with `parseTime=true`, so date/time columns come back as RFC 3339 strings.

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

### Named connections (one pod, several databases)

A single pod can serve several databases, each with its own read and optional write
credentials, selected per query:

```bash
./manage.sh set-conn --name orders "postgres://reader:pass@pg:5432/orders"
./manage.sh set-conn-write --name orders "postgres://writer:pass@pg:5432/orders"
./manage.sh set-conn --name warehouse "mysql://reader:pass@my:3306/warehouse"
SQLPOD_CONNECTIONS=orders,warehouse ./manage.sh deploy

./query.sh query --conn orders "SELECT * FROM customers LIMIT 5"
./query.sh query --conn warehouse "SELECT COUNT(*) FROM inventory"
./query.sh query "SELECT 1"                    # the default connection still works
```

Names are letters/digits/dashes. Each maps to secret keys and env vars mechanically:

| | Secret key | Env var |
|---|---|---|
| read | `conn-string-<name>` | `SQLPOD_CONN_<NAME>` |
| write | `conn-string-<name>-write` | `SQLPOD_CONN_<NAME>_WRITE` |

Write isolation is **per connection**: `--conn orders --write` looks up exactly
`SQLPOD_CONN_ORDERS_WRITE` and fails cleanly if it isn't set — it never falls back to the
connection's read credentials or to another connection's write key. A shared read-only pod can
therefore expose several databases to a team or agent fleet; the never-set-a-write-key-on-a-shared-pod
rule applies per connection, same as before.

### Output

Read mode:

```json
{"columns":["id","name"],"durationMs":12,"maxRows":1000,"mode":"read",
 "rowCount":2,"rows":[[1,"a"],[2,"b"]],"truncated":false}
```

`truncated: true` means the result hit `--max-rows` (default 1000) — add a filter or raise the cap.
`moreResultSets: true` (SQL Server batches / stored procedures) means only the first result set
was returned; run the statements separately to see the rest.

Value encoding: dates/times are RFC 3339 strings; binary values that aren't valid UTF-8
(`VARBINARY`, `ROWVERSION`, MySQL `BIT`, ...) are base64 strings; SQL Server `UNIQUEIDENTIFIER`
renders as a canonical GUID string; non-finite floats become `"NaN"` / `"Infinity"` / `"-Infinity"`.

`--format tsv` prints a header line then tab-joined rows (`NULL` for nulls; tabs, newlines, and
backslashes inside values are escaped as `\t`, `\n`, `\\`); truncation is reported on stderr.

Flags must come **before** the SQL argument — a trailing flag is rejected rather than silently
joined into the SQL text.

Write mode: `{"durationMs":8,"mode":"write","rowsAffected":1}`.

Errors go to stderr as `{"error":"..."}` with a non-zero exit; the connection string is scrubbed.

## Notes / limitations (v1)

- Raw SQL only — no bound parameters yet.
- On MySQL, some exact-numeric types (e.g. `DECIMAL`) may serialize as JSON strings rather than
  numbers — that's the driver's lossless representation.
- Write mode reports `rowsAffected` only; it does not return rows (no SELECT-after-write).
- The image is distroless (no shell). Use `query`/`query-file`, not an interactive shell. If you
  ever need to poke around inside, swap the base image to `gcr.io/distroless/static:debug`.

## Layout

| File | Purpose |
|------|---------|
| `main.go` | the CLI that runs in the pod (query execution, JSON output, read-only enforcement) |
| `drivers.go` | DSN-scheme → driver detection and per-engine DSN normalization |
| `Dockerfile` | multi-stage build → distroless static image |
| `k8s/deployment-template.yaml` | the sleeper Deployment (envsubst for secret name/key) |
| `query.sh` | agent-facing driver: run queries via kubectl exec |
| `manage.sh` | operator lifecycle: build / push / deploy / secrets |
| `scripts/lib.sh` | logging + kubectl helpers shared by both scripts |
| `examples/` | per-engine secret manifests and a named-connections sample |
| `docs/open-source-plan.md` | roadmap and design decisions |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) — checks, local database testing, and the
invariants PRs must not break.

## License

MIT — see [LICENSE](LICENSE).
