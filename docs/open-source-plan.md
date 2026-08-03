# sqlpod — design decisions & roadmap

Status: living document. Created 2026-07-30. Tracks the design decisions behind
sqlpod and the path to a public v0.1.0.

## 1. Goal & scope

sqlpod runs ad-hoc SQL queries from inside a Kubernetes cluster. It has two
parts:

- a **small Go CLI** that runs as a warm distroless pod in the cluster and
  executes the queries (read-only by default),
- **`query.sh`, the agent-facing driver script** — the surface an AI agent (or
  human) uses to run queries; it wraps `kubectl exec` and nothing else, and
- **`manage.sh`, the operator script** — build, deploy, and secret management,
  kept separate so granting an agent query access never grants lifecycle or
  credential access.

Together they solve three problems at once:

- **Reach.** The database is often reachable only from inside the cluster — not
  from the operator's laptop. The pod runs where the connectivity already is, so
  no VPN, port-forward, or firewall exception is needed.
- **A command-line interface built for AI agents.** The agent issues plain shell
  calls — `./query.sh query "<SQL>"`, `query-file`, or stdin — and results come
  back as structured JSON with explicit row caps and truncation flags; errors are
  JSON on stderr with a non-zero exit. There is no REPL to script around and no
  output parsing beyond JSON — one shell call in, one JSON document out. An
  agent's permission allowlist needs exactly one entry (`query.sh`), whose worst
  case is running SQL.
- **Connection indirection.** The caller never holds a database connection or
  credentials. The connection string lives in a k8s secret and is injected into
  the pod as an env var; the client side only ever talks to the cluster via
  `kubectl exec`. The only client-side credential is the kubeconfig, and access
  control is namespace RBAC — giving an agent query access never means handing
  it a DSN.

The intended deployment model is **one pod per developer, in that developer's
namespace**, each with its own database login and optional write key. A shared
namespace may additionally run a read-only pod (write key never set) for
team-wide or agent-fleet query access — see the deployment-model decision in §2.

The path to v0.1.0:

1. A LICENSE, README, and this plan committed from the first commit.
2. A CI pipeline that builds/tests on PR and publishes a multi-arch Docker image others can deploy.
3. A credible multi-database story (SQL Server first; PostgreSQL and MySQL next).
4. Named connections so one pod can serve several databases safely.

## 2. Decisions

| Decision | Choice | Rationale / rejected alternatives |
|---|---|---|
| Name | **sqlpod** | Obvious "runner"-style names are heavily used already (snowplow/sql-runner is a well-known Go tool; several similarly named projects and a PyPI package exist). `sqlpod` had no GitHub collisions at time of search and captures the k8s-pod angle. `podsql` was the runner-up. |
| License | **MIT** | Small tool, maximize reuse. No dependency conflicts (go-mssqldb is BSD-style). Apache-2.0 rejected as heavier than needed. |
| Module path | **`github.com/nathanfox/sqlpod`** | Matches the GitHub repo; single-package main, so no internal imports were affected. |
| DB support | **SQL Server + PostgreSQL + MySQL**; no SQLite | All three have pure-Go drivers (CGO stays off, distroless static image preserved). SQLite rejected: a local-file DB contradicts the tool's premise (remote DB reachable only from the cluster). |
| Driver selection | **Infer from DSN scheme** (`sqlserver://`, `postgres://`/`postgresql://`, `mysql://`) | Zero new required config; the scheme already lives inside the secret so nothing new leaks. Rejected: explicit driver env var as the primary mechanism (may still add as an override/hint). |
| Named connections | **Env-var convention** `SQLPOD_CONN_<NAME>` / `SQLPOD_CONN_<NAME>_WRITE` + `--conn <name>` flag | See §5. Rejected: config file, `envFrom` prefix. |
| Client scripts | **Split: `query.sh` (agent) / `manage.sh` (operator)** | Least privilege by file boundary: an agent allowlisted for query.sh can only run SQL — no deploy/delete/secret commands to reach. The boundary is an option, not a usage rule — it exists so *standing/auto-approved* permissions can be scoped safely; in interactive sessions an agent may drive manage.sh too, since each command is approved individually. Subcommand grammar (`query.sh query ...`) kept for symmetry with manage.sh, discoverable `help`, and blind flag passthrough to the pod binary (new Go flags need no script changes). Rejected: one script with allowlisted substrings (fragile), positional SQL grammar (stdin/help ambiguity, flag classification). |
| Deployment model | **Per-developer namespace** (one pod per developer); shared pods supported only as a read-only variant | One pod = one credential set = one trust boundary: RBAC governs who can `kubectl exec` but not the arguments passed, so `--write` capability is inseparable from exec access on the same pod. Per-developer pods give database-side attribution (own login), per-user write control, and small blast radius; pod cost is negligible (50m/64Mi). A shared **read-only** pod (write key never set) is a supported pattern for teams/agent fleets — never set a write key on a shared pod. Rejected: in-pod multi-tenancy or identity passthrough — that road rebuilds an authenticated HTTP service, contradicting the zero-endpoint pillar. |
| Hosting platform | **Kubernetes-only for v1**; the Go binary stays transport-agnostic by design | The binary has no k8s dependency — it reads env vars and speaks argv/stdin/stdout; all platform coupling lives in the driver scripts and manifest, and must stay there. k8s alone covers EKS/GKE/AKS/k3s/kind, matches the tool's premise (DB reachable only from the cluster), and has the sharpest security story (namespace RBAC, secret injection). Designated second platform if demand appears: **SSH bastion** (`ssh host /usr/local/bin/sqlpod "<SQL>"` — likely docs + a ten-line script, no new code). Considered and deferred: Docker-over-SSH (variant of the bastion story), ECS `execute-command` (AWS-specific, demand-driven). Ruled out: serverless — cold starts break the warm-pod model and Cloud Run requires an HTTP endpoint, contradicting the zero-endpoint pillar. |
| CI/CD | **GitHub Actions → GHCR**, multi-arch on tag | See §6. |
| Registry default in manage.sh | **None — explicit required** | A default pointing at anyone's registry is wrong for everyone else; the script errors with a hint (`REGISTRY=ghcr.io/youruser`). |

## 3. Read-only enforcement matrix

Read mode always runs inside a transaction that is rolled back. Per-engine reality:

| Engine | Mechanism | Strength / caveats |
|---|---|---|
| SQL Server | Rollback only — T-SQL has no `SET TRANSACTION READ ONLY` | The read-only login (`db_datareader`) is the real guardrail; rollback catches accidental DML. |
| PostgreSQL | `BeginTx(ctx, &sql.TxOptions{ReadOnly: true})` **+** rollback | Strongest: server rejects writes inside a read-only transaction. |
| MySQL | `TxOptions{ReadOnly: true}` (`START TRANSACTION READ ONLY`) **+** rollback | **Caveat: DDL auto-commits and escapes the transaction.** The read-only login remains the real guardrail; document prominently. |

In all cases, layer 1 (a genuinely read-only login in the read connection string)
is the primary defense; the transaction behavior is defense-in-depth.

## 4. Multi-DB design

- Keep `microsoft/go-mssqldb`; add `jackc/pgx/v5/stdlib` (not `lib/pq` — maintenance
  mode) and `go-sql-driver/mysql`. All pure Go: `CGO_ENABLED=0` and the distroless
  static image are preserved.
- New `drivers.go`: DSN-scheme detection → `(driverName, normalizedDSN, txOptions)`.
  - `sqlserver://` → `sqlserver`
  - `postgres://` / `postgresql://` → `pgx`
  - `mysql://` → `mysql`, translating the URL form to go-sql-driver DSN format and
    auto-appending `parseTime=true` (otherwise DATETIME comes back as `[]byte`).
- `coerce()` (main.go) stays the single JSON-mapping point. Known difference to
  document: MySQL returns `[]byte` for DECIMAL/typed numerics in some cases, which
  serializes as JSON strings. A per-driver coerce hook is the extension point if
  this matters later.
- The one-timeout-covers-everything behavior (connect + query + fetch) is kept and
  the `--timeout` flag help text says so.

## 5. Named connections design

Goal: one pod serving several databases (e.g. `orders`, `warehouse`), each with its
own read and optional write credentials, selected per query.

**Chosen: env-var convention.**

- `--conn <name>` flag. Lookup: `SQLPOD_CONN_<NAME>` (read) and
  `SQLPOD_CONN_<NAME>_WRITE` (write), where `<NAME>` is uppercased with `-` → `_`.
- No `--conn` → the bare `SQLPOD_CONN` / `SQLPOD_CONN_WRITE` (the default
  connection keeps working unchanged).
- The write-never-falls-back invariant is preserved by construction: `--conn orders
  --write` looks up exactly `SQLPOD_CONN_ORDERS_WRITE` and errors if unset — same
  code path as the default connection, just with a computed suffix.
- Secrets: keys `conn-string-<name>` / `conn-string-<name>-write` in the same
  secret (or separate secrets per connection; the manifest maps each env var via
  `secretKeyRef` either way).
- manage.sh: `set-conn --name orders "<dsn>"`, `set-conn-write --name orders ...`;
  deployment env block generated from `SQLPOD_CONNECTIONS="orders,warehouse"`.
  Prerequisite fix: generalize `set_conn_key` to preserve **all** existing secret
  keys (it preserved only the single counterpart key — with more than two keys,
  `kubectl apply`'s 3-way merge would drop the others). *Implemented (session 4)
  with `kubectl patch --type=merge` per key instead of re-applying the whole
  secret: preservation of unrelated keys holds by construction, and secrets are
  never `kubectl apply`'d at all anymore.*
- Deploy discovery: `SQLPOD_CONNECTIONS` was originally the sole source of the
  generated env block, so re-deploying without it silently dropped the
  `SQLPOD_CONN_*` entries via apply's 3-way merge — the same hazard as above,
  one layer up (the secret keys survived; only the wiring vanished). *Fixed:
  the secret is the source of truth.* When the var is unset, deploy derives
  names from the secret's key names — never values; non-name keys in reused
  secrets are warned about and skipped. Unset = discover, explicitly empty =
  wire none (for that deploy only), set = explicit override. This stays inside
  the env-var convention rather than reopening the rejected mounted config
  file: no new artifact, the keys `set-conn --name` already writes are simply
  read back. `status` prints wired-vs-secret connections and warns on drift.

**Rejected:**
- *Mounted YAML config file*: adds a projected volume, a YAML dependency, and a
  second source of truth for what is ultimately a name→env mapping; complicates the
  read-only-rootfs distroless pod.
- *`envFrom` with prefix*: imports every key of a secret wholesale; the read/write
  pairing becomes implicit in key naming, so a stray `-write` key silently becomes
  writable.

## 6. CI/CD design

- `.github/workflows/ci.yml` — on PR and push to `master`:
  `gofmt -l` check, `go vet`, `go test ./...`, `go build`, and `docker build`
  (no push) to catch Dockerfile rot.
- `.github/workflows/release.yml` — on tag `v*`:
  setup-qemu + setup-buildx + login to GHCR with `GITHUB_TOKEN`
  (`permissions: packages: write`) + build-push-action with
  `platforms: linux/amd64,linux/arm64`; tags via metadata-action (`vX.Y.Z`, `latest`).
- Dockerfile change: use buildx `TARGETOS`/`TARGETARCH` build args instead of
  hardcoded `GOOS=linux` so the same Dockerfile cross-compiles both platforms.
  Deliberately **no** `FROM --platform=$BUILDPLATFORM` native-cross-compile pin:
  tested and it breaks plain `docker build` under the classic (non-BuildKit)
  builder that some `manage.sh build` users still run ("" is an invalid platform).
  The cost is QEMU-emulated Go compiles in the release job — acceptable for a
  binary this small.
- No goreleaser / binary artifacts for v1 — the image is the deliverable.

## 7. Roadmap

| # | Session | Contents | Acceptance criteria |
|---|---|---|---|
| 1 | Tests — **done** | Refactor `writeTSV` to take `io.Writer`; add `main_test.go` covering `coerce`, `sanitize`, `readSQL`, `connString` (all four env combinations via `t.Setenv`), `writeTSV` | `go test ./...` green; the read/write env invariants have explicit tests |
| 2 | CI/CD + publish — **done** | Workflows from §6; Dockerfile TARGETOS/TARGETARCH; tag `v0.1.0` | Multi-arch image pullable from GHCR (`ghcr.io/nathanfox/sqlpod`); CI green on PR |
| 3 | Multi-DB — **done** | `drivers.go` per §4; pgx + mysql deps; per-driver read-only enforcement per §3; docs | Queries verified against real Postgres + MySQL instances; MySQL DDL caveat documented |
| 4 | Named connections — **done** | `--conn` flag per §5; manage.sh `set-conn --name`; generalized secret-key preservation; manifest env generation from `SQLPOD_CONNECTIONS` | Two named connections + default coexist on one pod; write isolation verified per connection |
| 5 | Polish — **done** | CONTRIBUTING.md, examples/ (per-DB secret + DSN samples), `--version` via ldflags in release workflow | — |

All five sessions are complete; the remaining step to v0.1.0's original goal —
a public repo — is the visibility flip (repo + GHCR package).

## 8. Open items

- Whether to also publish plain binaries (goreleaser) once there's demand.
- **SSH-bastion transport** (see the hosting-platform decision in §2) — revisit if
  users ask; expected shape is `examples/ssh-bastion.md` plus a small driver
  script, with no changes to the Go binary.
- **Active Directory auth for SQL Server** — demand-driven. NTLM is ruled out as
  the recommended path even though the driver supports it on Linux: it requires an
  AD account password in the connection string, which no AD shop should accept.
  The designated path is **Kerberos via keytab**: a one-line blank import of
  `go-mssqldb/integratedauth/krb5`, plus keytab/`krb5.conf` mounted as secret
  volumes in the deployment template (DSN then references file paths via
  `authenticator=krb5;krb5-configfile=...;krb5-keytabfile=...` — no credential in
  the DSN itself). Honest verification needs a real AD domain with SPNs, so don't
  ship it untested. Entra ID (driver's `azuread` package) is the passwordless
  option if the target is Azure SQL / Arc with workload identity.

## 9. Non-goals

- **SQLite** — local-file DB; contradicts the remote-DB premise.
- **HTTP endpoint / server mode** — the zero-endpoint kubectl-exec model is the point.
- **Bound parameters** — deferred; raw SQL only for now (callers interpolate).
- **Query history / audit log** — kubectl audit logging already covers exec calls.

## 10. Prior art / positioning

No packaged equivalent was found (searched July 2026):

- **DIY bastion pod + generic client** (`psql`/`sqlcmd`/[usql](https://github.com/xo/usql)
  over `kubectl exec`): the closest workflow, but no read-only enforcement, no JSON
  envelope/row caps, shell-oriented, credential hygiene left to the operator.
- **[snowplow/sql-runner](https://github.com/snowplow/sql-runner)**:
  templated SQL playbook orchestration for warehouses — different purpose.
- **MCP database servers**: agent-friendly SQL access with read-only modes, but they
  run as local/HTTP services rather than the zero-endpoint, RBAC-only pod model.

sqlpod's niche: warm distroless pod, auth = kubeconfig RBAC only, rollback-enforced
read-only, agent-consumable JSON with row caps. The README "why" section reflects this.
