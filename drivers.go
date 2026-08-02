package main

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"  // registers the "pgx" driver
	_ "github.com/microsoft/go-mssqldb" // registers the "sqlserver" driver
)

// driverInfo is the result of resolving a connection string: which database/sql
// driver to use, the DSN in that driver's native format, and the transaction
// options for read mode.
type driverInfo struct {
	name string // driver name for sql.Open
	dsn  string // normalized DSN handed to the driver ("" when connector is set)

	// connector, when non-nil, is opened via sql.OpenDB instead of
	// sql.Open(name, dsn). mysql:// URLs resolve to a connector so
	// credentials and database names never round-trip through a text DSN
	// (whose format cannot represent e.g. ':' in a username).
	connector driver.Connector

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
		cfg, err := mysqlConfig(dsn)
		if err != nil {
			return driverInfo{}, err
		}
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			return driverInfo{}, fmt.Errorf("mysql config: %w", err)
		}
		return driverInfo{name: "mysql", connector: connector, readTx: &sql.TxOptions{ReadOnly: true}}, nil
	default:
		// Echo the scheme only when it is actually scheme-shaped: an ADO-style
		// string with a URL-valued parameter can put credentials before the
		// first "://", and those must never reach the error message.
		if schemeLike(scheme) {
			return driverInfo{}, fmt.Errorf("unsupported connection scheme %q (supported: sqlserver://, postgres://, postgresql://, mysql://)", scheme)
		}
		return driverInfo{}, errors.New("unsupported connection string (supported schemes: sqlserver://, postgres://, postgresql://, mysql://)")
	}
}

// schemeLike reports whether s looks like a URL scheme (RFC 3986) of sane
// length, i.e. is safe to echo back in an error message.
func schemeLike(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '.' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// mysqlConfig translates a mysql:// URL into a mysql.Config. Userinfo, host,
// and database name are set as decoded struct fields rather than re-encoded
// into go-sql-driver's text DSN format, which cannot represent characters like
// ':' in a username or '%' in a database name. Query parameters are handed to
// the driver's own DSN parser so known options (charset, timeout, tls, ...)
// keep their native semantics. parseTime defaults to true so DATETIME/
// TIMESTAMP columns scan as time.Time instead of []byte.
func mysqlConfig(dsn string) (*mysql.Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// Deliberately not wrapped: url.Error's message embeds the full raw
		// URL, credentials included.
		return nil, errors.New("parse mysql DSN: invalid mysql:// URL (check URL-encoding of special characters)")
	}
	cfg, err := mysql.ParseDSN("/?" + u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("parse mysql DSN params: %w", err)
	}
	if !u.Query().Has("parseTime") {
		cfg.ParseTime = true
	}
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net = "tcp"
	host := u.Host
	if u.Port() == "" {
		host += ":3306"
	}
	cfg.Addr = host
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	return cfg, nil
}
