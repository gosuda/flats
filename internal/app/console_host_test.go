package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func consoleHost(t *testing.T) (*Host, *http.Client) {
	t.Helper()
	return consoleHostWith(t, t.TempDir(), nil)
}

// consoleHostWith starts a legacy-mode host in dir, after edit adjusts its
// options, without the server runtime.
func consoleHostWith(t *testing.T, dir string, edit func(*Options)) (*Host, *http.Client) {
	t.Helper()
	opts := localOptions(dir)
	opts.Overrides["host.server_runtime"] = "false"
	if edit != nil {
		edit(&opts)
	}
	h, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return h, &http.Client{Transport: transport}
}

// consoleCall sends a request the way the console page does.
func consoleCall(t *testing.T, h *Host, client *http.Client, method, path string, body io.Reader, out any) int {
	t.Helper()
	base := "http://" + h.Addr()
	req, _ := http.NewRequest(method, base+path, body)
	req.Header.Set("X-Flats-Console", "1")
	req.Header.Set("Origin", base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}
