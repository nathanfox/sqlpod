package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoerce(t *testing.T) {
	loc := time.FixedZone("UTC+2", 2*60*60)
	ts := time.Date(2026, 7, 30, 12, 34, 56, 789000000, loc)

	tests := []struct {
		name string
		in   any
		want any
	}{
		{"nil", nil, nil},
		{"bytes to string", []byte("abc"), "abc"},
		{"time to RFC3339Nano", ts, "2026-07-30T12:34:56.789+02:00"},
		{"int64 passthrough", int64(42), int64(42)},
		{"float64 passthrough", 3.14, 3.14},
		{"bool passthrough", true, true},
		{"string passthrough", "hello", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coerce(tt.in); got != tt.want {
				t.Errorf("coerce(%#v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	const conn = "sqlserver://user:secret@db:1433"

	if got := sanitize(nil, conn); got != nil {
		t.Errorf("sanitize(nil) = %v, want nil", got)
	}

	tests := []struct {
		name    string
		err     error
		connStr string
		want    string
	}{
		{
			"conn string redacted",
			errors.New("dial failed: " + conn),
			conn,
			"dial failed: <connection-string-redacted>",
		},
		{
			"multiple occurrences redacted",
			errors.New(conn + " retry " + conn),
			conn,
			"<connection-string-redacted> retry <connection-string-redacted>",
		},
		{
			"unrelated message unchanged",
			errors.New("timeout waiting for server"),
			conn,
			"timeout waiting for server",
		},
		{
			"empty conn string unchanged",
			errors.New("some error"),
			"",
			"some error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitize(tt.err, tt.connStr)
			if got.Error() != tt.want {
				t.Errorf("sanitize() = %q, want %q", got.Error(), tt.want)
			}
		})
	}

	t.Run("multiple secrets each redacted", func(t *testing.T) {
		orig := "mysql://user:pw@host/db"
		normalized := "user:pw@tcp(host:3306)/db?parseTime=true"
		err := errors.New("dial " + normalized + " (from " + orig + ") failed")
		got := sanitize(err, orig, normalized)
		want := "dial <connection-string-redacted> (from <connection-string-redacted>) failed"
		if got.Error() != want {
			t.Errorf("sanitize() = %q, want %q", got.Error(), want)
		}
	})
}

func TestConnString(t *testing.T) {
	const (
		readConn  = "sqlserver://reader:pw@db:1433"
		writeConn = "sqlserver://writer:pw@db:1433"
	)

	// t.Setenv to "" is equivalent to unset: connString treats empty as missing.
	tests := []struct {
		name    string
		conn    string
		connW   string
		write   bool
		want    string
		wantErr string
	}{
		{"read with conn set", readConn, "", false, readConn, ""},
		{"read with conn unset", "", "", false, "", envConn},
		{"write with both set", readConn, writeConn, true, writeConn, ""},
		{"write never falls back to read", readConn, "", true, "", envConnWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envConn, tt.conn)
			t.Setenv(envConnWrite, tt.connW)
			got, err := connString(tt.write, "")
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("connString(%v) = %q, want error mentioning %s", tt.write, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("connString(%v) error = %q, want mention of %s", tt.write, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("connString(%v) unexpected error: %v", tt.write, err)
			}
			if got != tt.want {
				t.Errorf("connString(%v) = %q, want %q", tt.write, got, tt.want)
			}
		})
	}
}

func TestConnStringNamed(t *testing.T) {
	const (
		defRead  = "postgres://def-reader@db/d"
		defWrite = "postgres://def-writer@db/d"
		ordRead  = "postgres://orders-reader@db/o"
		ordWrite = "postgres://orders-writer@db/o"
		wareRead = "mysql://warehouse-reader@db/w"
		euRead   = "postgres://eu-reader@db/e"
	)
	setAll := func(t *testing.T) {
		t.Setenv("SQLPOD_CONN", defRead)
		t.Setenv("SQLPOD_CONN_WRITE", defWrite)
		t.Setenv("SQLPOD_CONN_ORDERS", ordRead)
		t.Setenv("SQLPOD_CONN_ORDERS_WRITE", ordWrite)
		t.Setenv("SQLPOD_CONN_WAREHOUSE", wareRead)
		t.Setenv("SQLPOD_CONN_ORDERS_EU", euRead)
	}

	t.Run("named read", func(t *testing.T) {
		setAll(t)
		got, err := connString(false, "orders")
		if err != nil || got != ordRead {
			t.Errorf("connString(false, orders) = %q, %v; want %q", got, err, ordRead)
		}
	})
	t.Run("named write", func(t *testing.T) {
		setAll(t)
		got, err := connString(true, "orders")
		if err != nil || got != ordWrite {
			t.Errorf("connString(true, orders) = %q, %v; want %q", got, err, ordWrite)
		}
	})
	t.Run("dash maps to underscore", func(t *testing.T) {
		setAll(t)
		got, err := connString(false, "orders-eu")
		if err != nil || got != euRead {
			t.Errorf("connString(false, orders-eu) = %q, %v; want %q", got, err, euRead)
		}
	})
	t.Run("named write never falls back", func(t *testing.T) {
		// warehouse has a read conn, a default write exists, orders has a
		// write conn — none of them may satisfy --conn warehouse --write.
		setAll(t)
		got, err := connString(true, "warehouse")
		if err == nil {
			t.Fatalf("connString(true, warehouse) = %q, want error", got)
		}
		if !strings.Contains(err.Error(), "SQLPOD_CONN_WAREHOUSE_WRITE") {
			t.Errorf("error = %q, want mention of SQLPOD_CONN_WAREHOUSE_WRITE", err)
		}
	})
	t.Run("unknown name errors with exact var", func(t *testing.T) {
		setAll(t)
		_, err := connString(false, "nope")
		if err == nil || !strings.Contains(err.Error(), "SQLPOD_CONN_NOPE") {
			t.Errorf("error = %v, want mention of SQLPOD_CONN_NOPE", err)
		}
	})
	for _, bad := range []string{"bad_name", "bad!", "9lives", "-x"} {
		t.Run("invalid name "+bad, func(t *testing.T) {
			setAll(t)
			if _, err := connString(false, bad); err == nil {
				t.Errorf("connString(false, %q) succeeded, want validation error", bad)
			}
		})
	}
}

func TestReadSQL(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "query.sql")
		if err := os.WriteFile(path, []byte("SELECT 1"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readSQL(nil, path)
		if err != nil {
			t.Fatalf("readSQL: %v", err)
		}
		if got != "SELECT 1" {
			t.Errorf("readSQL = %q, want %q", got, "SELECT 1")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := readSQL(nil, filepath.Join(t.TempDir(), "nope.sql"))
		if err == nil || !strings.Contains(err.Error(), "read --file") {
			t.Errorf("readSQL error = %v, want mention of read --file", err)
		}
	})

	t.Run("args joined", func(t *testing.T) {
		got, err := readSQL([]string{"SELECT", "1"}, "")
		if err != nil {
			t.Fatalf("readSQL: %v", err)
		}
		if got != "SELECT 1" {
			t.Errorf("readSQL = %q, want %q", got, "SELECT 1")
		}
	})

	t.Run("file wins over args", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "query.sql")
		if err := os.WriteFile(path, []byte("SELECT 'file'"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readSQL([]string{"SELECT", "'args'"}, path)
		if err != nil {
			t.Fatalf("readSQL: %v", err)
		}
		if got != "SELECT 'file'" {
			t.Errorf("readSQL = %q, want file contents %q", got, "SELECT 'file'")
		}
	})

	t.Run("piped stdin", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := os.Stdin
		os.Stdin = r
		t.Cleanup(func() {
			os.Stdin = orig
			r.Close()
		})
		if _, err := w.WriteString("SELECT FROM stdin"); err != nil {
			t.Fatal(err)
		}
		w.Close()

		got, err := readSQL(nil, "")
		if err != nil {
			t.Fatalf("readSQL: %v", err)
		}
		if got != "SELECT FROM stdin" {
			t.Errorf("readSQL = %q, want %q", got, "SELECT FROM stdin")
		}
	})

	t.Run("no input", func(t *testing.T) {
		info, err := os.Stdin.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			t.Skip("test stdin is piped; the no-input branch needs a terminal-like stdin")
		}
		got, err := readSQL(nil, "")
		if err != nil {
			t.Fatalf("readSQL: %v", err)
		}
		if got != "" {
			t.Errorf("readSQL = %q, want empty", got)
		}
	})
}

func TestWriteTSV(t *testing.T) {
	tests := []struct {
		name string
		cols []string
		rows [][]any
		want string
	}{
		{
			"header only",
			[]string{"id", "name"},
			nil,
			"id\tname\n",
		},
		{
			"nil becomes NULL",
			[]string{"a", "b"},
			[][]any{{nil, "x"}},
			"a\tb\nNULL\tx\n",
		},
		{
			"mixed types",
			[]string{"id", "name", "active"},
			[][]any{
				{int64(1), "alice", true},
				{int64(2), "bob", false},
			},
			"id\tname\tactive\n1\talice\ttrue\n2\tbob\tfalse\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeTSV(&buf, tt.cols, tt.rows); err != nil {
				t.Fatalf("writeTSV: %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("writeTSV output = %q, want %q", got, tt.want)
			}
		})
	}
}
