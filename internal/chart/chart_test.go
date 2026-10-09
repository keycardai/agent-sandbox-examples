package chart_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/chart"
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

	_, err = chart.Render(t.Context(), dir, "release", "default")
	if !errors.Is(err, chart.ErrHelm) {
		t.Fatalf("Render error = %v, want ErrHelm", err)
	}
	want := "mise resolves helm from " + global + ", not " + filepath.Dir(dir) + "/mise.toml"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Render error = %q, want it to contain %q", err, want)
	}
}

// TestRenderUsesThisRepositorysLauncher renders a chart outside the repository
// that ships its own mise.toml and scripts/pinned. Render must still run this
// repository's launcher, so the outside script never runs.
func TestRenderUsesThisRepositorysLauncher(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "outside-launcher-ran")
	for path, body := range map[string]string{
		"Chart.yaml":        "apiVersion: v2\nname: outside\nversion: 0.1.0\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: outside\n",
		"mise.toml":         "",
		"scripts/pinned":    "#!/bin/sh\n: >" + marker + "\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // The outside launcher must be executable to prove it is never run.
		if err := os.WriteFile(full, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	docs, err := chart.Render(t.Context(), dir, "release", "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("Render ran the launcher beside the chart, not this repository's")
	}
	if len(docs) != 1 || !strings.Contains(string(docs[0]), "name: outside") {
		t.Errorf("docs = %q, want the outside chart's ConfigMap", docs)
	}
}
