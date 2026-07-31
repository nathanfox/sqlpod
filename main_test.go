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
			got, err := connString(tt.write)
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
