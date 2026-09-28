package local_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDownRunsPinnedK3d runs down.sh from outside the repository with a
// decoy k3d first on PATH. The fake mise resolves k3d only when asked
// against the repository root. The pinned k3d reports no cluster, so a run
// that reaches it exits 0 without deleting anything.
func TestDownRunsPinnedK3d(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		mise       string
		wantErr    bool
		want       string
		wantPinned bool
	}{
		{"pinned", "resolves", false, "there is no k3d cluster named asp", true},
		{"no mise", "", true, "down.sh: mise is not installed. See README.md.", false},
		{"mise lacks k3d", "fails", true, "down.sh: mise does not provide k3d. Run mise install.", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			pinnedRan := filepath.Join(dir, "pinned-ran")
			decoyRan := filepath.Join(dir, "decoy-ran")
			path := fakeTools(t, dir, root, tc.mise, pinnedRan, decoyRan)
			empty := filepath.Join(dir, "empty.env")
			if err := os.WriteFile(empty, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			//nolint:gosec // The arguments are fixed paths inside this test.
			cmd := exec.CommandContext(t.Context(), "bash",
				filepath.Join(root, "local", "down.sh"), "--cluster", "asp", "--env", empty, "--yes")
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + path}
			out, err := cmd.CombinedOutput()

			if failed := err != nil; failed != tc.wantErr {
				t.Fatalf("failed = %t, want %t\n%s", failed, tc.wantErr, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("output lacks %q\n%s", tc.want, out)
			}
			if ran := exists(t, pinnedRan); ran != tc.wantPinned {
				t.Errorf("pinned k3d ran = %t, want %t\n%s", ran, tc.wantPinned, out)
			}
			if exists(t, decoyRan) {
				t.Errorf("the decoy k3d ran\n%s", out)
			}
		})
	}
}

// fakeTools returns a PATH that holds the tools down.sh runs, a fake docker,
// and a decoy k3d that touches decoyRan. When mise is "resolves" or "fails",
// the PATH ends with a fake mise that resolves or refuses to resolve a pinned
// k3d, which touches pinnedRan and reports no cluster.
func fakeTools(t *testing.T, dir, root, mise, pinnedRan, decoyRan string) string {
	t.Helper()

	bin := linkTools(t, "basename", "bash", "dirname")
	writeScript(t, filepath.Join(bin, "docker"), "exit 0")
	writeScript(t, filepath.Join(bin, "k3d"), ": >"+decoyRan)
	if mise == "" {
		return bin
	}

	pinned := filepath.Join(dir, "pinned")
	miseDir := filepath.Join(dir, "mise")
	for _, d := range []string{pinned, miseDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeScript(t, filepath.Join(pinned, "k3d"), ": >"+pinnedRan+"\nexit 1")
	answer := "exit 1"
	if mise == "resolves" {
		answer = `[ "$1 $2 $3" = "-C ` + root + ` which" ] || exit 1` + "\necho " + pinned + `/"$4"`
	}
	writeScript(t, filepath.Join(miseDir, "mise"), answer)

	return bin + ":" + miseDir
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()

	//nolint:gosec // The fake tool must be executable.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}

	return err == nil
}
