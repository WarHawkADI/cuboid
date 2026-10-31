// Package tests holds checks that read the repository as a whole: properties no
// single package can test about itself.
package tests

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func walk(t *testing.T, root string, ext []string, f func(path, text string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		for _, e := range ext {
			if strings.HasSuffix(p, e) {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				f(filepath.ToSlash(p), string(b))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// I9: no answer is ever read from a file of expected results.
func TestI9NoGroundTruthInAppCode(t *testing.T) {
	for _, root := range []string{"../internal", "../cmd"} {
		walk(t, root, []string{".go"}, func(p, text string) {
			if !strings.HasSuffix(p, "_test.go") && strings.Contains(text, "ground_truth") {
				t.Errorf("%s mentions ground_truth", p)
			}
		})
	}
}

// I1: the query service cannot reach the main database, because it is never
// given a connection to it.
func TestI1QueryPackageHasNoMainConnection(t *testing.T) {
	walk(t, "../internal/query", []string{".go"}, func(p, text string) {
		if strings.HasSuffix(p, "_test.go") {
			return
		}
		for _, bad := range []string{"MAIN_", "MainVerify", "MainCDC", "pgxpool.New", "pgx.Connect", "5433"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s mentions %q", p, bad)
			}
		}
	})
}

// I2: changes arrive by replication only: Cuboid never installs a trigger or
// polls for rows on the main database.
func TestI2NoTriggers(t *testing.T) {
	for _, root := range []string{"../internal", "../migrations/main"} {
		walk(t, root, []string{".go", ".sql"}, func(p, text string) {
			if strings.Contains(strings.ToUpper(text), "CREATE TRIGGER") {
				t.Errorf("%s creates a trigger", p)
			}
		})
	}
}

// I11: nothing logs a credential.
func TestI11LogsCarryNoSecrets(t *testing.T) {
	logCall := regexp.MustCompile(`\.(Info|Warn|Error|Debug)\([^\n]*"(token|secret|pepper|password|authorization)"`)
	for _, root := range []string{"../internal", "../cmd"} {
		walk(t, root, []string{".go"}, func(p, text string) {
			if m := logCall.FindString(text); m != "" && !strings.HasSuffix(p, "_test.go") {
				t.Errorf("%s logs a secret field: %s", p, m)
			}
		})
	}
}

// Every colour lives in tokens.css, so each one keeps its single job.
func TestUIUsesOnlyTokens(t *testing.T) {
	hex := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	walk(t, "../web/src", []string{".tsx", ".ts", ".css"}, func(p, text string) {
		if strings.HasSuffix(p, "tokens.css") {
			return
		}
		if m := hex.FindString(text); m != "" {
			t.Errorf("%s hardcodes %s", p, m)
		}
	})
}

// A repository going to judges holds no real key.
func TestNoKeysInTheRepository(t *testing.T) {
	key := regexp.MustCompile(`cub_(store|co)_[0-9]+_[A-Za-z0-9]{20,}|BEGIN PRIVATE`)
	for _, root := range []string{"../internal", "../cmd", "../config", "../api", "../docs", "../web/src"} {
		walk(t, root, []string{".go", ".yaml", ".yml", ".json", ".md", ".http", ".tsx", ".ts"}, func(p, text string) {
			if m := key.FindString(text); m != "" {
				t.Errorf("%s holds a key: %s", p, m[:12])
			}
		})
	}
}
