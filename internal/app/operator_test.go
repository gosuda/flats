package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func operatorHost(t *testing.T) (*Host, *http.Client) {
	t.Helper()
	var raw [40]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	credential := base64.RawURLEncoding.EncodeToString(raw[:])
	opts := localOptions(t.TempDir())
	opts.Runtime = false
	opts.OperatorCredential = credential
	h, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	if h.Opts.OperatorCredential != "" {
		t.Fatal("Host retained the operator credential")
	}
	jar, _ := cookiejar.New(nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Jar: jar, Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)
	// Provision through the embedding API and unlock through the real session
	// endpoint. Do not put the credential in argv, an environment or evidence.
	body := strings.NewReader(`{"credential":"` + credential + `"}`)
	if code := operatorCall(t, h, client, "POST", "/console/api/operator/session", body, nil); code != 200 {
		t.Fatalf("operator unlock status %d", code)
	}
	return h, client
}

func operatorCall(t *testing.T, h *Host, client *http.Client, method, path string, body io.Reader, out any) int {
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

func TestOperatorWiringRejectsHeadersAndPlainContext(t *testing.T) {
	h, client := operatorHost(t)
	path := "/console/api/settings"
	if code := operatorCall(t, h, http.DefaultClient, "PUT", path, strings.NewReader(`{"keep_versions":"3"}`), nil); code != 403 {
		t.Fatalf("forged console headers were admitted: %d", code)
	}
	if _, err := h.Svc.Decide(context.Background(), "not-an-approval", true); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("plain context gained operator proof: %v", err)
	}
	if code := operatorCall(t, h, client, "PUT", path, strings.NewReader(`{"keep_versions":"3"}`), nil); code != 200 {
		t.Fatalf("authenticated operator failed: %d", code)
	}
	if code := operatorCall(t, h, client, "DELETE", "/console/api/operator/session", nil, nil); code != 200 {
		t.Fatalf("logout %d", code)
	}
	if code := operatorCall(t, h, client, "PUT", path, strings.NewReader(`{"keep_versions":"4"}`), nil); code != 403 {
		t.Fatalf("revoked session admitted: %d", code)
	}
}

func TestOperatorInputIsBoundedAndNotEchoed(t *testing.T) {
	for _, size := range []int{0, 31, 32, 4096, 4097} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			writer.Write([]byte(strings.Repeat("x", size) + "\n"))
			writer.Close()
			var output bytes.Buffer
			value, err := readOperatorCredential(reader, &output)
			valid := size >= 32 && size <= 4096
			if (err == nil) != valid || (valid && len(value) != size) {
				t.Fatalf("input validity size %d: %v", size, err)
			}
			if output.Len() != 0 {
				t.Fatal("pipe credential echoed to output")
			}
		})
	}
}
