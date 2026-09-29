package chart_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/chart"
)

// TestRenderRefusesGlobalHelm puts first on PATH a mise that resolves helm
// from the user's global config, as mise does when it ignores the
// repository's mise.toml. It sets PATH, so it cannot run in parallel.
func TestRenderRefusesGlobalHelm(t *testing.T) {
	const global = "/home/user/.config/mise/config.toml"
	bin := t.TempDir()
	mise := `#!/usr/bin/env bash
case "$3" in
which) echo /opt/helm/4.3.0/helm ;;
ls)
	printf '[{"version": "4.3.0", "install_path": "/opt/helm/4.3.0", ` +
		`"source": {"type": "mise.toml", "path": "` + global + `"}, "installed": true, "active": true}]\n'
	;;
esac
`
	//nolint:gosec // The fake mise must be executable.
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte(mise), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir, err := filepath.Abs("../../install")
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		t.Fatal(err)
	}

	_, err = chart.Render(t.Context(), dir, "release")
	if !errors.Is(err, chart.ErrHelm) {
		t.Fatalf("Render error = %v, want ErrHelm", err)
	}
	want := "mise resolves helm from " + global + ", not " + filepath.Dir(dir) + "/mise.toml"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Render error = %q, want it to contain %q", err, want)
	}
}
