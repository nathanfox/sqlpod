package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/go-sql-driver/mysql"  // registers the "mysql" driver
	_ "github.com/jackc/pgx/v5/stdlib"  // registers the "pgx" driver
	_ "github.com/microsoft/go-mssqldb" // registers the "sqlserver" driver
)

// driverInfo is the result of resolving a connection string: which database/sql
// driver to use, the DSN in that driver's native format, and the transaction
// options for read mode.
type driverInfo struct {
	name string // driver name for sql.Open
	dsn  string // normalized DSN handed to the driver

	// readTx is the transaction options for read mode. Where the driver
	// supports it (pgx, mysql), ReadOnly makes the server itself reject
	// writes inside the transaction. nil (sqlserver — T-SQL has no read-only
	// transaction) means a plain transaction: the always-rollback in
	// execRead is the only in-engine guard, and the read-only login is the
	// real one.
	readTx *sql.TxOptions
}

// resolveDriver infers the driver from the DSN's scheme. A DSN with no scheme
// is handed to the sqlserver driver unchanged, which keeps ADO-style strings
// ("server=...;user id=...") from existing deployments working.
func resolveDriver(dsn string) (driverInfo, error) {
	scheme, _, found := strings.Cut(dsn, "://")
	if !found {
		return driverInfo{name: "sqlserver", dsn: dsn}, nil
	}
	switch strings.ToLower(scheme) {
	case "sqlserver":
		return driverInfo{name: "sqlserver", dsn: dsn}, nil
	case "postgres", "postgresql":
		return driverInfo{name: "pgx", dsn: dsn, readTx: &sql.TxOptions{ReadOnly: true}}, nil
	case "mysql":
		normalized, err := mysqlDSN(dsn)
		if err != nil {
			return driverInfo{}, err
		}
		return driverInfo{name: "mysql", dsn: normalized, readTx: &sql.TxOptions{ReadOnly: true}}, nil
	default:
		return driverInfo{}, fmt.Errorf("unsupported connection scheme %q (supported: sqlserver://, postgres://, postgresql://, mysql://)", scheme)
	}
}

// mysqlDSN translates a mysql:// URL into go-sql-driver's native format
// (user:pass@tcp(host:port)/db?params). parseTime=true is added unless already
// present, so DATETIME/TIMESTAMP columns scan as time.Time instead of []byte.
func mysqlDSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse mysql DSN: %w", err)
	}

	var b strings.Builder
	if u.User != nil {
		b.WriteString(u.User.Username())
		if pw, ok := u.User.Password(); ok {
			b.WriteString(":")
			b.WriteString(pw)
		}
		b.WriteString("@")
	}
	host := u.Host
	if u.Port() == "" {
		host += ":3306"
	}
	fmt.Fprintf(&b, "tcp(%s)/", host)
	b.WriteString(strings.TrimPrefix(u.Path, "/"))

	q := u.Query()
	if q.Get("parseTime") == "" {
		q.Set("parseTime", "true")
	}
	b.WriteString("?")
	b.WriteString(q.Encode())
	return b.String(), nil
}
