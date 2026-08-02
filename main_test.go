package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestCoerce(t *testing.T) {
	loc := time.FixedZone("UTC+2", 2*60*60)
	ts := time.Date(2026, 7, 30, 12, 34, 56, 789000000, loc)

	tests := []struct {
		name   string
		in     any
		dbType string
		want   any
	}{
		{"nil", nil, "", nil},
		{"bytes to string", []byte("abc"), "", "abc"},
		{"invalid utf8 bytes to base64", []byte{0x8f, 0x00}, "", "jwA="},
		{"time to RFC3339Nano", ts, "", "2026-07-30T12:34:56.789+02:00"},
		{"int64 passthrough", int64(42), "", int64(42)},
		{"float64 passthrough", 3.14, "", 3.14},
		{"NaN to string", math.NaN(), "", "NaN"},
		{"+Inf to string", math.Inf(1), "", "Infinity"},
		{"-Inf to string", math.Inf(-1), "", "-Infinity"},
		{"bool passthrough", true, "", true},
		{"string passthrough", "hello", "", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coerce(tt.in, tt.dbType); got != tt.want {
				t.Errorf("coerce(%#v, %q) = %#v, want %#v", tt.in, tt.dbType, got, tt.want)
			}
		})
	}
}

func TestCoerceGUID(t *testing.T) {
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	got := coerce(append([]byte(nil), raw...), "UNIQUEIDENTIFIER")
	// SQL Server stores the first three GUID groups little-endian; the
	// canonical rendering reverses them.
	if want := "03020100-0504-0706-0809-0A0B0C0D0E0F"; got != want {
		t.Errorf("coerce(GUID) = %v, want %v", got, want)
	}

	// Cross-check the swizzle against the driver's own implementation.
	var g mssql.UniqueIdentifier
	if err := g.Scan(append([]byte(nil), raw...)); err != nil {
		t.Fatalf("UniqueIdentifier.Scan: %v", err)
	}
	if got != g.String() {
		t.Errorf("coerce(GUID) = %v, want driver rendering %v", got, g.String())
	}

	// Wrong length or wrong type name must not be treated as a GUID.
	if got := coerce([]byte("short"), "UNIQUEIDENTIFIER"); got != "short" {
		t.Errorf("coerce(short, UNIQUEIDENTIFIER) = %v, want passthrough", got)
	}
	if got := coerce(append([]byte(nil), raw...), "VARBINARY"); got == "03020100-0504-0706-0809-0A0B0C0D0E0F" {
		t.Error("coerce treated a VARBINARY column as a GUID")
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
		{
			"tabs and newlines escaped",
			[]string{"note"},
			[][]any{{"line1\nline2\tend\\"}},
			"note\nline1\\nline2\\tend\\\\\n",
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

// --- run()-level regression tests ---

// TestErrorsNeverContainPassword locks the invariant documented in
// CONTRIBUTING.md: connection strings (passwords in particular) must never
// appear in error output, whichever resolution path fails.
func TestErrorsNeverContainPassword(t *testing.T) {
	const pw = "S3cr%t-Hunter2"
	tests := []struct {
		name string
		dsn  string
	}{
		// A bare % makes url.Parse fail; its *url.Error embeds the raw URL.
		{"mysql invalid url escape", "mysql://reader:" + pw + "@db.internal:3306/wh"},
		// The :// inside a parameter value puts everything before it —
		// password included — into the would-be "scheme".
		{"ado string with url param", "server=db;user id=sa;password=" + pw + ";url=https://login.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envConn, tt.dsn)
			err := run([]string{"SELECT 1"}, io.Discard)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), pw) {
				t.Errorf("error leaks the password: %q", err)
			}
		})
	}
}

func TestTrailingFlagRejected(t *testing.T) {
	err := run([]string{"UPDATE t SET x=1", "--write"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "must precede") {
		t.Errorf("run with trailing --write = %v, want flag-placement error", err)
	}
}

func TestMaxRowsValidation(t *testing.T) {
	for _, n := range []string{"-1", "0"} {
		err := run([]string{"--max-rows", n, "SELECT 1"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--max-rows") {
			t.Errorf("run --max-rows %s = %v, want validation error", n, err)
		}
	}
}

// --- execRead tests against an in-memory fake driver ---

type fakeResultSet struct {
	cols []string
	rows [][]driver.Value
}

type fakeConnector struct{ sets []fakeResultSet }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{sets: c.sets}, nil
}
func (c fakeConnector) Driver() driver.Driver { return nil }

type fakeConn struct{ sets []fakeResultSet }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }
func (c *fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &fakeRows{sets: c.sets}, nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct {
	sets     []fakeResultSet
	set, row int
}

func (r *fakeRows) Columns() []string { return r.sets[r.set].cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	rs := r.sets[r.set]
	if r.row >= len(rs.rows) {
		return io.EOF
	}
	copy(dest, rs.rows[r.row])
	r.row++
	return nil
}
func (r *fakeRows) HasNextResultSet() bool { return r.set+1 < len(r.sets) }
func (r *fakeRows) NextResultSet() error {
	if !r.HasNextResultSet() {
		return io.EOF
	}
	r.set++
	r.row = 0
	return nil
}

func execReadJSON(t *testing.T, sets []fakeResultSet, maxRows int) map[string]any {
	t.Helper()
	db := sql.OpenDB(fakeConnector{sets: sets})
	defer db.Close()
	var buf bytes.Buffer
	if err := execRead(context.Background(), db, "SELECT", maxRows, "json", driverInfo{}, "conn", &buf); err != nil {
		t.Fatalf("execRead: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	return out
}

func intRows(n int) fakeResultSet {
	rs := fakeResultSet{cols: []string{"n"}}
	for i := range n {
		rs.rows = append(rs.rows, []driver.Value{int64(i)})
	}
	return rs
}

func TestExecReadTruncationBoundary(t *testing.T) {
	// Exactly maxRows rows: everything returned, not truncated.
	out := execReadJSON(t, []fakeResultSet{intRows(3)}, 3)
	if out["truncated"] != false || out["rowCount"] != float64(3) {
		t.Errorf("3 rows @ max 3: truncated=%v rowCount=%v, want false/3", out["truncated"], out["rowCount"])
	}
	if _, ok := out["moreResultSets"]; ok {
		t.Error("moreResultSets present for a single result set")
	}

	// One more row than maxRows: truncated.
	out = execReadJSON(t, []fakeResultSet{intRows(4)}, 3)
	if out["truncated"] != true || out["rowCount"] != float64(3) {
		t.Errorf("4 rows @ max 3: truncated=%v rowCount=%v, want true/3", out["truncated"], out["rowCount"])
	}
}

func TestExecReadMultiResultSet(t *testing.T) {
	out := execReadJSON(t, []fakeResultSet{intRows(2), intRows(5)}, 100)
	if out["moreResultSets"] != true {
		t.Errorf("moreResultSets = %v, want true", out["moreResultSets"])
	}
	if out["rowCount"] != float64(2) {
		t.Errorf("rowCount = %v, want first set's 2", out["rowCount"])
	}
}

func TestExecReadNaN(t *testing.T) {
	sets := []fakeResultSet{{cols: []string{"x"}, rows: [][]driver.Value{{math.NaN()}}}}
	out := execReadJSON(t, sets, 100)
	rows := out["rows"].([]any)
	if got := rows[0].([]any)[0]; got != "NaN" {
		t.Errorf("NaN emitted as %v, want \"NaN\"", got)
	}
}
