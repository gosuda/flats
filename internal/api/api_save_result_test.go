package api_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func enablePublishedSaves(t *testing.T, svc *core.Service) {
	t.Helper()
	if _, err := svc.SetPermittedNetworks([]string{"portal"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPublicInstance(t.Context(), true, "portal", "test"); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedSaveResponse(t *testing.T) {
	for _, kind := range []string{"website", "page"} {
		t.Run(kind, func(t *testing.T) {
			srv, svc := setup(t)
			enablePublishedSaves(t, svc)
			for n := 1; n <= 2; n++ {
				files := map[string]string{"index.html": fmt.Sprintf("<title>Save %d</title>", n)}
				if kind == "page" {
					files["flats.json"] = `{"type":"page"}`
				}
				code, out := req(t, "POST", srv.URL+"/api/flats/published/draft", bytes.NewReader(archive(files)), nil)
				if code != 201 || out["version"] != float64(n) {
					t.Fatalf("published receipt: %d %v", code, out)
				}
				draft := out["draft"].(map[string]any)
				flat := out["flat"].(map[string]any)
				if draft["revision"] != float64(n) || draft["hash"] == "" || flat["live_version"] != float64(n) || flat["visibility"] != "public" || flat["public_url"] == "" || flat["public_notice"] != core.PublicAccessNotice || flat["private_url"] != "" {
					t.Fatalf("missing exact save or public metadata: %v", out)
				}
			}
			approvals, err := svc.ListApprovals(t.Context(), "pending")
			if err != nil || len(approvals) != 0 {
				t.Fatalf("save requested approval: %v %v", approvals, err)
			}
			if _, err := svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"index.html": "<title>Draft only</title>"}
			if kind == "page" {
				files["flats.json"] = `{"type":"page"}`
			}
			code, out := req(t, "POST", srv.URL+"/api/flats/published/draft", bytes.NewReader(archive(files)), nil)
			if code != 201 || len(out) != 1 || out["draft"] == nil {
				t.Fatalf("off response changed: %d %v", code, out)
			}
			flat, err := svc.GetFlat(t.Context(), "published")
			if err != nil || flat.LiveVersion != 2 || flat.Draft.Revision != 3 {
				t.Fatalf("off save changed live content: %+v %v", flat, err)
			}
		})
	}
}

func TestPublishedSaveQuotaFailureResponse(t *testing.T) {
	srv, svc := setup(t)
	enablePublishedSaves(t, svc)
	code, out := req(t, "POST", srv.URL+"/api/flats/quota/draft", bytes.NewReader(archive(map[string]string{"index.html": "first"})), nil)
	if code != 201 {
		t.Fatalf("first save: %d %v", code, out)
	}
	one := 1
	if err := svc.SetVersionQuotaOverride(t.Context(), "quota", &one); err != nil {
		t.Fatal(err)
	}
	code, out = req(t, "POST", srv.URL+"/api/flats/quota/draft", bytes.NewReader(archive(map[string]string{"index.html": "kept"})), nil)
	message, _ := out["error"].(string)
	if code != 409 || out["category"] != "conflict" || !strings.Contains(message, "Draft revision 2 was saved but not published") || !strings.Contains(message, "Draft was kept") || !strings.Contains(message, "version quota") {
		t.Fatalf("failed save: %d %v", code, out)
	}
	flat, err := svc.GetFlat(t.Context(), "quota")
	if err != nil || flat.LiveVersion != 1 || flat.Versions != 1 || flat.Draft.Revision != 2 {
		t.Fatalf("failed publication changed live content or lost Draft: %+v %v", flat, err)
	}
}

type pendingSaveNetwork struct{}

func (pendingSaveNetwork) ServeExposure(ctx context.Context, r core.ExposureRequest) (core.ExposureResult, error) {
	return pendingSaveNetwork{}.ExposureStatus(ctx, r.Host)
}

func (pendingSaveNetwork) StopPublicRoutes(context.Context, string) (core.PublicStopResult, error) {
	return core.PublicStopResult{}, nil
}

func (pendingSaveNetwork) ExposurePolicy(context.Context) (string, error) { return "pending", nil }

func (pendingSaveNetwork) ExposureStatus(_ context.Context, host string) (core.ExposureResult, error) {
	return core.ExposureResult{Endpoints: []core.ExposureEndpoint{{Provider: core.ProviderPortal, Configured: true, Permitted: true, Audience: core.AudienceCurrent, Host: host, State: "starting", Detail: "waiting for relay", URL: "https://pending.example"}}}, nil
}

func TestPublishedSavePendingPublicLinkResponse(t *testing.T) {
	srv, svc := setupWithLifecycle(t, pendingSaveNetwork{})
	enablePublishedSaves(t, svc)
	code, out := req(t, "POST", srv.URL+"/api/flats/pending/draft", bytes.NewReader(archive(map[string]string{"index.html": "published"})), nil)
	if code != 201 || out["version"] != float64(1) {
		t.Fatalf("pending route failed the save: %d %v", code, out)
	}
	f := out["flat"].(map[string]any)
	if f["connection_state"] != "starting" || f["public_url"] != "https://pending.example" || f["public_notice"] != core.PublicAccessNotice {
		t.Fatalf("pending route claimed readiness or lost notice: %v", f)
	}
}

func TestLegacySaveResponseInPublicInstanceMode(t *testing.T) {
	srv, svc := setup(t)
	if _, err := svc.CreateFlat(t.Context(), "legacy", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	enablePublishedSaves(t, svc)
	code, out := req(t, "POST", srv.URL+"/api/flats/legacy/draft", bytes.NewReader(archive(map[string]string{"index.html": "legacy"})), nil)
	if code != 201 || len(out) != 1 || out["draft"] == nil {
		t.Fatalf("legacy response changed: %d %v", code, out)
	}
	f, err := svc.GetFlat(t.Context(), "legacy")
	if err != nil || f.LiveVersion != 0 || f.Draft.Revision != 1 {
		t.Fatalf("legacy save published: %+v %v", f, err)
	}
}
