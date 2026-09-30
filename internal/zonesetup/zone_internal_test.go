package zonesetup

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientSpeaksHTTP1ToAnHTTP2Server(t *testing.T) {
	t.Parallel()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	client, err := newHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatalf("transport = %T with no TLS config", client.Transport)
	}
	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("server transport = %T", server.Client().Transport)
	}
	transport.TLSClientConfig.RootCAs = serverTransport.TLSClientConfig.RootCAs
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.Proto != "HTTP/1.1" {
		t.Errorf("proto = %s, want HTTP/1.1", res.Proto)
	}
}
