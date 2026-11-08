package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRootConfigKeepsGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	os.WriteFile(path, []byte(`guard:
  enabled: true
  blacklist: /etc/XboardNode-Plus/blacklist.txt
  allow: 1.2.3.4
instances:
  - panel:
      url: https://panel.example.com
      node_id: 1
`), 0o600)
	root, err := loadWritableRootConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRootConfig(path, root); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "guard:") || !strings.Contains(s, "enabled: true") || !strings.Contains(s, "allow: 1.2.3.4") {
		t.Fatalf("guard lost after rewrite:\n%s", s)
	}
	if strings.Count(s, "guard:") != 1 {
		t.Fatalf("guard duplicated into instances:\n%s", s)
	}
}
