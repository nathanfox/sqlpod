package main

import (
	"strings"
	"testing"
)

func TestResolveDriver(t *testing.T) {
	tests := []struct {
		name       string
		dsn        string
		wantDriver string
		wantDSN    string
		wantReadTx bool // true → expect a ReadOnly read transaction
		wantErr    string
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
			name:       "mysql translated with port",
			dsn:        "mysql://user:pw@host:3307/db",
			wantDriver: "mysql",
			wantDSN:    "user:pw@tcp(host:3307)/db?parseTime=true",
			wantReadTx: true,
		},
		{
			name:       "mysql default port",
			dsn:        "mysql://user:pw@host/db",
			wantDriver: "mysql",
			wantDSN:    "user:pw@tcp(host:3306)/db?parseTime=true",
			wantReadTx: true,
		},
		{
			name:       "mysql no database",
			dsn:        "mysql://user:pw@host",
			wantDriver: "mysql",
			wantDSN:    "user:pw@tcp(host:3306)/?parseTime=true",
			wantReadTx: true,
		},
		{
			name:       "mysql params preserved and parseTime appended",
			dsn:        "mysql://user:pw@host/db?tls=true",
			wantDriver: "mysql",
			wantDSN:    "user:pw@tcp(host:3306)/db?parseTime=true&tls=true",
			wantReadTx: true,
		},
		{
			name:       "mysql parseTime not duplicated or overridden",
			dsn:        "mysql://user:pw@host/db?parseTime=false",
			wantDriver: "mysql",
			wantDSN:    "user:pw@tcp(host:3306)/db?parseTime=false",
			wantReadTx: true,
		},
		{
			name:       "mysql url-encoded password decoded",
			dsn:        "mysql://user:p%40ss@host/db",
			wantDriver: "mysql",
			wantDSN:    "user:p@ss@tcp(host:3306)/db?parseTime=true",
			wantReadTx: true,
		},
		{
			name:    "unknown scheme",
			dsn:     "oracle://user:pw@host/db",
			wantErr: "unsupported connection scheme",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := resolveDriver(tt.dsn)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveDriver(%q) error = %v, want mention of %q", tt.dsn, err, tt.wantErr)
				}
				if err != nil && !strings.Contains(err.Error(), "sqlserver://") {
					t.Errorf("error should list supported schemes, got %q", err)
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
