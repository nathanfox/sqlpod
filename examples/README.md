# Examples

Sample Kubernetes Secret manifests for each supported engine, as an
alternative to `manage.sh set-conn` (which creates the same thing). Pick one,
edit the DSN, and:

```bash
kubectl apply -n <your-namespace> -f secret-postgres.yaml
NAMESPACE=<your-namespace> REGISTRY=ghcr.io/nathanfox ./manage.sh deploy
```

How the pieces fit:

- The deployment reads the secret's `conn-string` key into `SQLPOD_CONN`
  (and `conn-string-write` into `SQLPOD_CONN_WRITE`, if present).
- Named connections add `conn-string-<name>[-write]` keys, wired into the pod
  with `SQLPOD_CONNECTIONS=<name>,... ./manage.sh deploy` and queried with
  `./query.sh query --conn <name> ...` — see `named-connections.yaml`.

| File | Shows |
|---|---|
| `secret-sqlserver.yaml` | SQL Server DSN, read + optional write key |
| `secret-postgres.yaml` | PostgreSQL DSN, read + optional write key |
| `secret-mysql.yaml` | MySQL DSN (note the DDL caveat in the README) |
| `named-connections.yaml` | one secret serving a default + two named connections |

Use a genuinely read-only login for every `conn-string*` key that isn't
`-write` — the transaction-level guard is defense-in-depth, not the primary
control (and on MySQL it cannot stop DDL at all).
