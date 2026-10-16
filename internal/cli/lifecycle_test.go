package cli

import (
	"strings"
	"testing"
)

const pendingLifecycle = `{"status":"pending_approval","approval":{"id":"apr-control","status":"pending","action":"create_version"},"approval_url":"http://console.test/approvals/apr-control","message":"Waiting for an explicit operator decision; current version keeps serving."}`

const pendingPublicLifecycle = `{"status":"pending_approval","approval":{"id":"apr-control","status":"pending","action":"set_visibility"},"approval_url":"http://console.test/approvals/apr-control","message":"Waiting for an explicit operator decision; current version keeps serving.","notice":"This flat is public: anyone on the internet can open it. A domain or URL is not what makes it public."}`

func TestLifecyclePendingExitAcrossCLI(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		for _, operation := range []string{"delete-version", "create-version", "deploy", "rollback", "public", "private"} {
			t.Run(operation+map[bool]string{false: "-text", true: "-json"}[jsonMode], func(t *testing.T) {
				f, srv := newFakeAPI(t)
				var args []string
				switch operation {
				case "delete-version":
					f.handle("DELETE /api/flats/demo/versions/2", 202, strings.ReplaceAll(pendingLifecycle, "create_version", "delete_version"))
					args = []string{"delete-version", "demo", "--version", "2"}
				case "create-version":
					f.handle("POST /api/flats/demo/versions", 202, pendingLifecycle)
					args = []string{"create-version", "demo", "--revision", "2"}
				case "deploy":
					f.handle("POST /api/flats/demo/deploy", 202, pendingLifecycle)
					args = []string{"deploy", "--flat", "demo", "--version", "1"}
				case "rollback":
					f.handle("POST /api/flats/demo/rollback", 202, pendingLifecycle)
					args = []string{"rollback", "demo", "--restore-data"}
				default:
					body := pendingLifecycle
					if operation == "public" {
						body = pendingPublicLifecycle
					}
					f.handle("POST /api/flats/demo/visibility", 202, body)
					args = []string{"visibility", "demo", operation}
				}
				if jsonMode {
					args = append([]string{"--json"}, args...)
				}
				r := run(t, srv.URL, "", args...)
				if r.code != ExitPending || !strings.Contains(r.stdout, "pending_approval") || !strings.Contains(r.stdout, "/approvals/apr-control") {
					t.Fatalf("pending output: %d %s %s", r.code, r.stdout, r.stderr)
				}
				if strings.Contains(r.stdout, " is live") {
					t.Fatal("pending request claimed activation")
				}
				if operation == "public" && (!strings.Contains(r.stdout, pendingPublicNotice) || strings.Contains(r.stdout, "This flat is public:")) {
					t.Fatalf("pending Public request used present-tense notice: %s", r.stdout)
				}
				for _, request := range f.reqs {
					if strings.HasPrefix(request.path, "/console/") {
						t.Fatalf("agent crossed into operator routes: %s", request.path)
					}
				}
			})
		}
	}
}

func TestCLIApprovalSurfaceIsReadOnly(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("GET /api/approvals", 200, `{"approvals":[{"id":"apr-control","flat":"demo","action":"create_version","status":"pending"}]}`)
	r := run(t, srv.URL, "", "approvals")
	if r.code != ExitOK || !strings.Contains(r.stdout, "pending") {
		t.Fatalf("approval read: %+v", r)
	}
	for _, name := range []string{"approve", "decide", "reject", "operator", "providers"} {
		if r := run(t, srv.URL, "", name, "apr-control"); r.code != ExitUsage {
			t.Fatalf("unexpected decision command %s: %+v", name, r)
		}
	}
	for _, request := range f.reqs {
		if request.method != "GET" || request.path != "/api/approvals" {
			t.Fatalf("approval census mutated server: %+v", request)
		}
	}
}

func TestDeletionCleanupPendingIsExplicit(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonMode], func(t *testing.T) {
			f, srv := newFakeAPI(t)
			f.handle("DELETE /api/flats/demo/versions/2", 200, `{"status":"cleanup_pending","cleanup_pending":true,"message":"Version 2 was deleted; repair storage and restart Flats. Do not repeat deletion."}`)
			args := []string{"delete-version", "demo", "--version", "2"}
			if jsonMode {
				args = append([]string{"--json"}, args...)
			}
			r := run(t, srv.URL, "", args...)
			if r.code != ExitOK || !strings.Contains(r.stdout, "cleanup_pending") || !strings.Contains(r.stdout, "Do not repeat deletion") {
				t.Fatalf("cleanup output: %+v", r)
			}
			if !jsonMode && !strings.Contains(r.stdout, "Status: cleanup_pending") {
				t.Fatal(r.stdout)
			}
		})
	}
}
