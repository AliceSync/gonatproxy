package service

import (
	"os"
	"strings"
	"testing"
)

func TestRenderUnitClient(t *testing.T) {
	b, err := RenderUnit("client", "/usr/bin/ds-client", "/etc/deepseek/client.json", "bob", "users")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"ExecStart=/usr/bin/ds-client -config /etc/deepseek/client.json",
		"User=bob", "Group=users", "Type=simple",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unit missing %q:\n%s", want, s)
		}
	}
}

func TestInstallUnitToTempDir(t *testing.T) {
	t.Setenv("DEEPSEEK_SYSTEMD_DIR", t.TempDir())
	p, err := InstallUnit("natclient", "/tmp/nc.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if !strings.HasSuffix(p, "deepseek-natclient.service") {
		t.Fatalf("unexpected path %s", p)
	}
}
