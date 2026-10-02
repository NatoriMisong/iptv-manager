package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDatabasePathKeepsExistingInstallation(t *testing.T) {
	dir := t.TempDir()
	modern := filepath.Join(dir, "iptv-manager.db")
	legacy := filepath.Join(dir, "youtube-tv.db")
	if databasePath(dir) != modern {
		t.Fatal("new installation used legacy name")
	}
	if err := os.WriteFile(legacy, []byte("legacy fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if databasePath(dir) != legacy {
		t.Fatal("upgrade lost existing database")
	}
	if err := os.WriteFile(modern, []byte("modern fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if databasePath(dir) != modern {
		t.Fatal("explicit modern database not preferred")
	}
}
