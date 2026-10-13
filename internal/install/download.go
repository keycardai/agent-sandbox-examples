//go:build ignore

// Download writes the agent-sandbox controller manifest of one release to
// install/agent-sandbox, and prints its sha256. go generate runs it from
// internal/install.
//
//	go run download.go VERSION
package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

const asset = "sandbox-with-extensions.yaml"

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
	path := filepath.Join("..", "..", "install", "agent-sandbox", asset)
	if err = os.WriteFile(path, manifest, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%s, sha256 %x)\n", path, version, sha256.Sum256(manifest))
}
