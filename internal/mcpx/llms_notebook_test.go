package mcpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostNotebookDiscovery(t *testing.T) {
	if !strings.Contains(guideDescription, "topic.notebook") {
		t.Error("guide description omits topic.notebook")
	}
	e := newEnv(t)
	srv := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	defer srv.Close()
	for _, tc := range []struct {
		name string
		path string
		want []string
	}{
		{"index", LLMsPath, []string{"notebooks", "get_notebook", "update_notebook"}},
		{"guide", AgentGuidePath, []string{"flats notebook get", "flats notebook apply", "topic.notebook"}},
		{"full", LLMsFullPath, []string{"notebooks", "flats notebook get", "flats notebook apply"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, text := fetch(t, srv.Client(), "GET", srv.URL+tc.path)
			if code != 200 {
				t.Fatalf("discovery status = %d", code)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("discovery omits %q", want)
				}
			}
		})
	}
}
