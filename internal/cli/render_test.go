package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRenderWithoutHeaders guards the status layout: a headerless table must not
// index a zero-length header slice.
func TestRenderWithoutHeaders(t *testing.T) {
	var buf bytes.Buffer
	app := &App{stdout: &buf}

	tbl := &table{}
	tbl.add("daemon", "running")
	tbl.add("room", "not in a room")
	app.render(tbl)

	out := buf.String()
	if !strings.Contains(out, "running") || !strings.Contains(out, "not in a room") {
		t.Fatalf("renderer dropped rows:\n%s", out)
	}
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("expected two lines, got:\n%q", out)
	}
}

func TestRenderWithHeadersAligns(t *testing.T) {
	var buf bytes.Buffer
	app := &App{stdout: &buf}

	tbl := &table{headers: []string{"A", "BBBB"}}
	tbl.add("xx", "y")
	app.render(tbl)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected header + row, got %d lines:\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "A   BBBB") {
		t.Fatalf("header alignment wrong: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "xx  y") {
		t.Fatalf("row alignment wrong: %q", lines[1])
	}
}

func TestRenderRaggedRowsDoNotPanic(t *testing.T) {
	var buf bytes.Buffer
	app := &App{stdout: &buf}
	tbl := &table{headers: []string{"only-one"}}
	tbl.add("a", "b", "c")
	app.render(tbl) // must not panic
	if !strings.Contains(buf.String(), "only-one") {
		t.Fatal("header missing")
	}
}
