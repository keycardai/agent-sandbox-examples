//go:build ignore

// Download writes the agent-sandbox controller manifest of one release to
// install/agent-sandbox, and prints its sha256. go generate runs it from
// internal/install.
//
//	go run download.go VERSION
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

const asset = "sandbox-with-extensions.yaml"

// ControllerSHA256 is the sha256 GitHub publishes for the
// sandbox-with-extensions.yaml asset of the release that the go:generate
// directive in install.go names. To move to another release, change the
// version there and this digest, then run go generate ./internal/install.
const ControllerSHA256 = "8cbe7f4c252463667e2286993bd735cb33bb2e47d7f304b92aa2ca97f57052c0"

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: go run download.go VERSION")
	}
	version := os.Args[1]
	url := "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/" + version + "/" + asset
	resp, err := http.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s: %s", url, resp.Status)
	}
	manifest, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		log.Fatalf("%s: %v", url, err)
	}
	digest := sha256.Sum256(manifest)
	if got := hex.EncodeToString(digest[:]); got != ControllerSHA256 {
		log.Fatalf("controller manifest sha256 = %s, want %s", got, ControllerSHA256)
	}
	path := filepath.Join("..", "..", "install", "agent-sandbox", asset)
	if err = os.WriteFile(path, manifest, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%s, sha256 %x)\n", path, version, digest)
}
