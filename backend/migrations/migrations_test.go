package migrations

import (
	"bufio"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"
)

var namePattern = regexp.MustCompile(`^(\d{14})_[a-z0-9_]+\.sql$`)

// TestMigrationFiles checks the file-level rules that do not need a database.
func TestMigrationFiles(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}
	seen := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		m := namePattern.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%s: name must be YYYYMMDDHHMMSS_snake_case.sql", name)
			continue
		}
		if _, err := time.Parse("20060102150405", m[1]); err != nil {
			t.Errorf("%s: prefix is not a valid UTC timestamp", name)
		}
		if other, dup := seen[m[1]]; dup {
			t.Errorf("%s: duplicate timestamp with %s", name, other)
		}
		seen[m[1]] = name

		body, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		checkBody(t, name, string(body))
	}
}

func checkBody(t *testing.T, name, body string) {
	t.Helper()
	if strings.Contains(body, "+goose Down") {
		t.Errorf("%s: migrations are forward-only, remove the +goose Down section", name)
	}
	noTx := strings.Contains(body, "-- +goose NO TRANSACTION")
	want := "SET LOCAL lock_timeout = '5s';"
	if noTx {
		want = "SET lock_timeout = '5s';"
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	afterUp := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "-- +goose Up"):
			afterUp = true
			continue
		case strings.HasPrefix(line, "--"):
			continue
		}
		if !afterUp {
			t.Errorf("%s: statement before -- +goose Up", name)
			return
		}
		if line != want {
			t.Errorf("%s: first statement must be %q, got %q", name, want, line)
		}
		return
	}
	t.Errorf("%s: no statements", name)
}
