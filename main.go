// Command sqlpod executes ad-hoc SQL queries (SQL Server, PostgreSQL, or
// MySQL — inferred from the connection string's scheme) and emits results as
// JSON.
//
// It is meant to run inside a pod in a developer namespace — one pod per
// developer, each with its own credentials — and be driven over `kubectl exec`.
// The connection string is supplied via environment variables (sourced from a
// k8s secret), never on the command line, so it does not leak into process
// listings or shell history.
//
// Read-only is the default and is enforced at two layers:
//  1. The default connection (SQLPOD_CONN) should use a read-only login
//     (e.g. db_datareader on SQL Server).
//  2. Read-mode statements run inside a transaction that is always rolled back,
//     so even a misconfigured login cannot persist changes.
//
// Writes require the explicit --write flag, which selects SQLPOD_CONN_WRITE
// and commits. If that variable is unset, --write fails rather than silently
// writing through the read connection.
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	mssql "github.com/microsoft/go-mssqldb"
)

const (
	envConn      = "SQLPOD_CONN"
	envConnWrite = "SQLPOD_CONN_WRITE"
)

// version is stamped by the release build via
// -ldflags "-X main.version=vX.Y.Z"; source builds report "dev".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "idle" {
		// Container entrypoint: stay alive so the pod is warm for `kubectl exec`.
		// The distroless base image has no `sleep`, so the binary idles itself.
		// Block on a signal rather than `select {}` — an empty select trips Go's
		// deadlock detector and aborts. This also gives a clean SIGTERM shutdown
		// when the pod is deleted.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		return
	}

	if err := run(os.Args[1:], os.Stdout); err != nil {
		emitError(err)
		os.Exit(1)
	}
}

func run(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("sqlpod", flag.ContinueOnError)
	var (
		write   = fs.Bool("write", false, "use the write connection and COMMIT (default: read-only, rolled back)")
		conn    = fs.String("conn", "", "named connection: look up SQLPOD_CONN_<NAME> (and _WRITE) instead of SQLPOD_CONN")
		file    = fs.String("file", "", "read SQL from this file instead of an argument")
		maxRows = fs.Int("max-rows", 1000, "maximum rows to return before truncating")
		timeout = fs.Duration("timeout", 30*time.Second, "overall timeout (connect + query + fetch)")
		format  = fs.String("format", "json", "output format: json or tsv")
		showVer = fs.Bool("version", false, "print version and exit")
	)
	// Parse errors are reported once, through the JSON error contract: the
	// flag package's own printing (error line + usage dump) is suppressed,
	// and -h prints the usage explicitly.
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "usage: sqlpod [flags] \"<SQL>\"\n       sqlpod [flags] --file query.sql\n       echo \"<SQL>\" | sqlpod [flags]\n\nflags:\n")
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stderr)
			os.Exit(0)
		}
		return fmt.Errorf("%v (run with -h for usage)", err)
	}
	// The flag package stops at the first non-flag argument, so anything
	// flag-shaped after the SQL would silently become part of the SQL text —
	// on some engines a trailing "--write" turns into a comment and a write
	// runs (and rolls back) in read mode while looking successful.
	if args := fs.Args(); len(args) > 1 {
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("argument %q comes after the SQL; flags must precede the SQL argument", a)
			}
		}
	}

	if *showVer {
		fmt.Println("sqlpod " + version)
		return nil
	}

	sqlText, err := readSQL(fs.Args(), *file)
	if err != nil {
		return err
	}
	if strings.TrimSpace(sqlText) == "" {
		return errors.New("no SQL provided (pass as an argument, --file, or stdin)")
	}
	if *format != "json" && *format != "tsv" {
		return fmt.Errorf("unknown --format %q (want json or tsv)", *format)
	}
	if *maxRows < 1 {
		return fmt.Errorf("invalid --max-rows %d (must be at least 1)", *maxRows)
	}

	connStr, err := connString(*write, *conn)
	if err != nil {
		return err
	}
	info, err := resolveDriver(connStr)
	if err != nil {
		return sanitize(err, connStr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var db *sql.DB
	if info.connector != nil {
		db = sql.OpenDB(info.connector)
	} else {
		db, err = sql.Open(info.name, info.dsn)
		if err != nil {
			return fmt.Errorf("open connection: %w", sanitize(err, connStr, info.dsn))
		}
	}
	defer db.Close()

	if *write {
		return execWrite(ctx, db, sqlText, connStr, info.dsn, out)
	}
	return execRead(ctx, db, sqlText, *maxRows, *format, info, connStr, out)
}

// connString picks the connection string for the requested mode and (optional)
// named connection. The env var name is built as SQLPOD_CONN[_<NAME>][_WRITE],
// so write mode looks up exactly the write variable for that connection and
// refuses to fall back — neither to the connection's read variable nor to the
// default write variable.
func connString(write bool, conn string) (string, error) {
	envVar := envConn
	if conn != "" {
		suffix, err := connEnvSuffix(conn)
		if err != nil {
			return "", err
		}
		envVar += "_" + suffix
	}
	if write {
		envVar += "_WRITE"
	}
	c := os.Getenv(envVar)
	if c == "" {
		if write {
			return "", fmt.Errorf("write mode requested but %s is not set", envVar)
		}
		return "", fmt.Errorf("%s is not set", envVar)
	}
	return c, nil
}

// connEnvSuffix converts a --conn name to its env-var suffix: uppercased with
// "-" mapped to "_". Names are restricted to what maps cleanly onto both env
// vars and k8s secret keys.
func connEnvSuffix(name string) (string, error) {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '-'):
		default:
			return "", fmt.Errorf("invalid connection name %q (want letters, digits, and dashes, starting with a letter)", name)
		}
	}
	if name == "" {
		return "", errors.New("empty connection name")
	}
	return strings.ReplaceAll(strings.ToUpper(name), "-", "_"), nil
}

func readSQL(args []string, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read --file: %w", err)
		}
		return string(b), nil
	}
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	// No argument and no --file: read from stdin if it is piped.
	info, err := os.Stdin.Stat()
	if err == nil && (info.Mode()&os.ModeCharDevice) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	return "", nil
}

// execRead runs a query inside a rolled-back transaction and prints the rows.
func execRead(ctx context.Context, db *sql.DB, sqlText string, maxRows int, format string, info driverInfo, connStr string, w io.Writer) error {
	start := time.Now()
	// info.readTx is ReadOnly where the engine supports it (pgx, mysql), so
	// the server itself rejects writes. On sqlserver it is nil (T-SQL lacks
	// SET TRANSACTION READ ONLY) and the rollback below is the only in-engine
	// guard. Either way the transaction is always rolled back.
	tx, err := db.BeginTx(ctx, info.readTx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", sanitize(err, connStr, info.dsn))
	}
	// Always discard: read mode must never persist side effects.
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, sqlText)
	if err != nil {
		return fmt.Errorf("query: %w", sanitize(err, connStr, info.dsn))
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("columns: %w", sanitize(err, connStr, info.dsn))
	}
	// Database type names drive type-specific coercion (e.g. sqlserver
	// UNIQUEIDENTIFIER arrives as 16 raw bytes). Best-effort: a driver that
	// cannot report types just gets the default coercion.
	var dbTypes []string
	if colTypes, err := rows.ColumnTypes(); err == nil {
		dbTypes = make([]string, len(colTypes))
		for i, ct := range colTypes {
			dbTypes[i] = ct.DatabaseTypeName()
		}
	}

	// Cap the pre-allocation: maxRows is caller input and may be huge.
	results := make([][]any, 0, min(maxRows, 1024))
	truncated := false
	for rows.Next() {
		if len(results) >= maxRows {
			truncated = true
			break
		}
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("scan: %w", sanitize(err, connStr, info.dsn))
		}
		for i := range raw {
			var dbType string
			if dbTypes != nil {
				dbType = dbTypes[i]
			}
			raw[i] = coerce(raw[i], dbType)
		}
		results = append(results, raw)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", sanitize(err, connStr, info.dsn))
	}
	// Statement batches and stored procedures can produce further result
	// sets; only the first is returned, so at least signal that more exist.
	moreResultSets := rows.NextResultSet()

	if format == "tsv" {
		if err := writeTSV(w, cols, results); err != nil {
			return err
		}
		if truncated {
			fmt.Fprintf(os.Stderr, "warning: output truncated at %d rows (raise with --max-rows)\n", maxRows)
		}
		if moreResultSets {
			fmt.Fprintln(os.Stderr, "warning: additional result sets were not read")
		}
		return nil
	}
	result := map[string]any{
		"mode":       "read",
		"columns":    cols,
		"rows":       results,
		"rowCount":   len(results),
		"truncated":  truncated,
		"maxRows":    maxRows,
		"durationMs": time.Since(start).Milliseconds(),
	}
	if moreResultSets {
		result["moreResultSets"] = true
	}
	return emit(w, result)
}

// execWrite runs a statement and commits, reporting rows affected.
func execWrite(ctx context.Context, db *sql.DB, sqlText, connStr, dsn string, w io.Writer) error {
	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", sanitize(err, connStr, dsn))
	}
	res, err := tx.ExecContext(ctx, sqlText)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("exec: %w", sanitize(err, connStr, dsn))
	}
	affected, affErr := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", sanitize(err, connStr, dsn))
	}
	result := map[string]any{
		"mode":       "write",
		"durationMs": time.Since(start).Milliseconds(),
	}
	if affErr == nil {
		result["rowsAffected"] = affected
	}
	return emit(w, result)
}

// coerce converts driver-native values into JSON-friendly forms. dbType is
// the column's DatabaseTypeName (may be empty when the driver doesn't report
// one). []byte values that aren't valid UTF-8 are base64-encoded — encoding
// them as Go strings would irreversibly mangle them into U+FFFD replacements.
func coerce(v any, dbType string) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		// go-mssqldb scans UNIQUEIDENTIFIER as 16 raw bytes in SQL Server's
		// mixed-endian layout; UniqueIdentifier owns that swizzle.
		if dbType == "UNIQUEIDENTIFIER" && len(t) == 16 {
			var g mssql.UniqueIdentifier
			if err := g.Scan(t); err == nil {
				return g.String()
			}
		}
		if utf8.Valid(t) {
			return string(t)
		}
		return base64.StdEncoding.EncodeToString(t)
	case float64:
		// encoding/json rejects non-finite floats, which would discard the
		// entire result set at output time.
		switch {
		case math.IsNaN(t):
			return "NaN"
		case math.IsInf(t, 1):
			return "Infinity"
		case math.IsInf(t, -1):
			return "-Infinity"
		}
		return t
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		return v
	}
}

// tsvEscaper keeps the row/column grid intact when values contain the
// delimiters themselves; consumers can reverse it unambiguously.
var tsvEscaper = strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r")

func writeTSV(w io.Writer, cols []string, rows [][]any) error {
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = tsvEscaper.Replace(c)
	}
	if _, err := fmt.Fprintln(w, strings.Join(headers, "\t")); err != nil {
		return err
	}
	for _, r := range rows {
		fields := make([]string, len(r))
		for i, v := range r {
			if v == nil {
				fields[i] = "NULL"
			} else {
				fields[i] = tsvEscaper.Replace(fmt.Sprintf("%v", v))
			}
		}
		if _, err := fmt.Fprintln(w, strings.Join(fields, "\t")); err != nil {
			return err
		}
	}
	return nil
}

func emit(w io.Writer, v map[string]any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func emitError(err error) {
	enc := json.NewEncoder(os.Stderr)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"error": err.Error()})
}

// sanitize strips connection strings out of an error message so credentials
// never reach stdout/stderr or logs. The drivers generally avoid embedding the
// DSN in errors, but this guards against that and any wrapped variants. Both
// the original env DSN and the driver-normalized form are passed, since an
// error could embed either.
func sanitize(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "<connection-string-redacted>")
		}
	}
	return errors.New(msg)
}
