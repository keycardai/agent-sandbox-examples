package sandbox

import (
	"strings"
	"testing"
)

func TestProxyConfigWritesLinearKeyWithoutScheme(t *testing.T) {
	sb := Sandbox{Resources: []Resource{
		{Slug: "github", APIURL: "https://api.github.com"},
		{Slug: "linear", APIURL: "https://api.linear.app"},
	}}
	config := sb.proxyConfig()
	for _, want := range []string{
		`host: "api.github.com"`,
		`template: "Bearer {credential}"`,
		`host: "api.linear.app"`,
		`template: "{credential}"`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("proxy config is missing %s:\n%s", want, config)
		}
	}
}

func TestProbesForInstalledResources(t *testing.T) {
	resources := []Resource{
		{Slug: "github", APIURL: "https://api.github.com"},
		{Slug: "sentry", APIURL: "https://sentry.io/api/0"},
		{Slug: "linear", APIURL: "https://api.linear.app"},
	}
	probes, err := probesFor(resources, "owner/repo/demo.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 || probes[0].Resource != "github" || probes[1].Resource != "linear" {
		t.Fatalf("want a GitHub and a Linear call, got %+v", probes)
	}
	if got := probes[0].Args[len(probes[0].Args)-1]; got != "https://api.github.com/repos/owner/repo/contents/demo.txt" {
		t.Errorf("GitHub call goes to %s", got)
	}
	if _, err := probesFor(resources, "not-a-path"); err == nil {
		t.Error("a read without OWNER/REPO/PATH must fail")
	}
}

func TestLinearProbeShow(t *testing.T) {
	show := linearProbe("https://api.linear.app").Show
	got, err := show(`{"data":{"viewer":{"name":"Ada","email":"ada@example.com"},"organization":{"name":"Keycard"}}}`)
	if err != nil || got != "Ada <ada@example.com> in the Keycard workspace" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := show(`{"errors":[{"message":"Authentication required, not authenticated"}]}`); err == nil {
		t.Error("a GraphQL error must fail the call")
	}
	if _, err := show(`<html>`); err == nil {
		t.Error("an answer that is not JSON must fail the call")
	}
}
