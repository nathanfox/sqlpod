package main

import (
	"strings"
	"testing"
	"time"
)

func TestResolveDriver(t *testing.T) {
	tests := []struct {
		name          string
		dsn           string
		wantDriver    string
		wantDSN       string
		wantConnector bool // true → mysql-style connector instead of a text DSN
		wantReadTx    bool // true → expect a ReadOnly read transaction
		wantErr       string
	}{
		{
			name:       "sqlserver url",
			dsn:        "sqlserver://user:pw@host:1433?database=db",
			wantDriver: "sqlserver",
			wantDSN:    "sqlserver://user:pw@host:1433?database=db",
			wantReadTx: false,
		},
		{
			name:       "schemeless ADO string falls back to sqlserver",
			dsn:        "server=host;user id=user;password=pw;database=db",
			wantDriver: "sqlserver",
			wantDSN:    "server=host;user id=user;password=pw;database=db",
			wantReadTx: false,
		},
		{
			name:       "postgres",
			dsn:        "postgres://user:pw@host:5432/db?sslmode=disable",
			wantDriver: "pgx",
			wantDSN:    "postgres://user:pw@host:5432/db?sslmode=disable",
			wantReadTx: true,
		},
		{
			name:       "postgresql alias",
			dsn:        "postgresql://user:pw@host/db",
			wantDriver: "pgx",
			wantDSN:    "postgresql://user:pw@host/db",
			wantReadTx: true,
		},
		{
			name:          "mysql resolves to a connector",
			dsn:           "mysql://user:pw@host:3307/db",
			wantDriver:    "mysql",
			wantConnector: true,
			wantReadTx:    true,
		},
		{
			name:    "unknown scheme",
			dsn:     "oracle://user:pw@host/db",
			wantErr: "unsupported connection scheme",
		},
		{
			// The "scheme" here is everything before the first "://" — an
			// ADO-style parameter value can put credentials there, so it must
			// not be echoed back.
			name:    "unsupported non-scheme prefix not echoed",
			dsn:     "server=db;user id=sa;password=Hunter2;url=https://login.example",
			wantErr: "unsupported connection string",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := resolveDriver(tt.dsn)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveDriver(%q) error = %v, want mention of %q", tt.dsn, err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "sqlserver://") {
					t.Errorf("error should list supported schemes, got %q", err)
				}
				if strings.Contains(err.Error(), "Hunter2") {
					t.Errorf("error echoes credentials: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDriver(%q) unexpected error: %v", tt.dsn, err)
			}
			if info.name != tt.wantDriver {
				t.Errorf("driver = %q, want %q", info.name, tt.wantDriver)
			}
			if info.dsn != tt.wantDSN {
				t.Errorf("dsn = %q, want %q", info.dsn, tt.wantDSN)
			}
			if tt.wantConnector != (info.connector != nil) {
				t.Errorf("connector = %v, want present=%v", info.connector, tt.wantConnector)
			}
			if tt.wantReadTx {
				if info.readTx == nil || !info.readTx.ReadOnly {
					t.Errorf("readTx = %+v, want ReadOnly transaction options", info.readTx)
				}
			} else if info.readTx != nil {
				t.Errorf("readTx = %+v, want nil (rollback-only)", info.readTx)
			}
		})
	}
}

func TestMysqlConfig(t *testing.T) {
	tests := []struct {
		name          string
		dsn           string
		wantUser      string
		wantPasswd    string
		wantAddr      string
		wantDBName    string
		wantParseTime bool
	}{
		{
			name:          "explicit port",
			dsn:           "mysql://user:pw@host:3307/db",
			wantUser:      "user",
			wantPasswd:    "pw",
			wantAddr:      "host:3307",
			wantDBName:    "db",
			wantParseTime: true,
		},
		{
			name:          "default port",
			dsn:           "mysql://user:pw@host/db",
			wantUser:      "user",
			wantPasswd:    "pw",
			wantAddr:      "host:3306",
			wantDBName:    "db",
			wantParseTime: true,
		},
		{
			name:          "no database",
			dsn:           "mysql://user:pw@host",
			wantUser:      "user",
			wantPasswd:    "pw",
			wantAddr:      "host:3306",
			wantDBName:    "",
			wantParseTime: true,
		},
		{
			name:          "ipv6 host",
			dsn:           "mysql://user:pw@[::1]/db",
			wantUser:      "user",
			wantPasswd:    "pw",
			wantAddr:      "[::1]:3306",
			wantDBName:    "db",
			wantParseTime: true,
		},
		{
			name:          "url-encoded password decoded",
			dsn:           "mysql://user:p%40ss@host/db",
			wantUser:      "user",
			wantPasswd:    "p@ss",
			wantAddr:      "host:3306",
			wantDBName:    "db",
			wantParseTime: true,
		},
		{
			// A ':' in the username cannot be represented in the driver's
			// text DSN format at all; the config struct carries it fine.
			name:          "url-encoded colon in username",
			dsn:           "mysql://a%3Ab:pw@host/db",
			wantUser:      "a:b",
			wantPasswd:    "pw",
			wantAddr:      "host:3306",
			wantDBName:    "db",
			wantParseTime: true,
		},
		{
			name:          "percent in database name",
			dsn:           "mysql://user:pw@host/100%25stats",
			wantUser:      "user",
			wantPasswd:    "pw",
			wantAddr:      "host:3306",
			wantDBName:    "100%stats",
			wantParseTime: true,
		},
		{
			name:          "explicit parseTime=false respected",
			dsn:           "mysql://user:pw@host/db?parseTime=false",
			wantUser:      "user",
			wantPasswd:    "pw",
			wantAddr:      "host:3306",
			wantDBName:    "db",
			wantParseTime: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := mysqlConfig(tt.dsn)
			if err != nil {
				t.Fatalf("mysqlConfig(%q): %v", tt.dsn, err)
			}
			if cfg.User != tt.wantUser {
				t.Errorf("User = %q, want %q", cfg.User, tt.wantUser)
			}
			if cfg.Passwd != tt.wantPasswd {
				t.Errorf("Passwd = %q, want %q", cfg.Passwd, tt.wantPasswd)
			}
			if cfg.Net != "tcp" {
				t.Errorf("Net = %q, want tcp", cfg.Net)
			}
			if cfg.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", cfg.Addr, tt.wantAddr)
			}
			if cfg.DBName != tt.wantDBName {
				t.Errorf("DBName = %q, want %q", cfg.DBName, tt.wantDBName)
			}
			if cfg.ParseTime != tt.wantParseTime {
				t.Errorf("ParseTime = %v, want %v", cfg.ParseTime, tt.wantParseTime)
			}
		})
	}

	t.Run("known params keep driver-native semantics", func(t *testing.T) {
		cfg, err := mysqlConfig("mysql://user:pw@host/db?timeout=5s&charset=utf8mb4,utf8")
		if err != nil {
			t.Fatalf("mysqlConfig: %v", err)
		}
		if cfg.Timeout != 5*time.Second {
			t.Errorf("Timeout = %v, want 5s (typed param not parsed natively)", cfg.Timeout)
		}
	})

	t.Run("unknown params preserved", func(t *testing.T) {
		cfg, err := mysqlConfig("mysql://user:pw@host/db?foo=bar")
		if err != nil {
			t.Fatalf("mysqlConfig: %v", err)
		}
		if cfg.Params["foo"] != "bar" {
			t.Errorf("Params[foo] = %q, want bar", cfg.Params["foo"])
		}
	})

	t.Run("unparsable url gives static error", func(t *testing.T) {
		const pw = "p%sswd"
		_, err := mysqlConfig("mysql://user:" + pw + "@host/db")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "host") {
			t.Errorf("parse error echoes DSN content: %q", err)
		}
	})
}
