package cli

import (
	"strings"
	"testing"
)

const pendingLifecycle = `{"status":"pending_approval","approval":{"id":"apr-control","status":"pending","action":"publish"},"approval_url":"http://console.test/approvals/apr-control","message":"Waiting for an explicit operator decision; current version keeps serving."}`

const pendingPublicLifecycle = `{"status":"pending_approval","approval":{"id":"apr-control","status":"pending","action":"set_visibility"},"approval_url":"http://console.test/approvals/apr-control","message":"Waiting for an explicit operator decision; current version keeps serving.","notice":"This flat is public: anyone on the internet can open it. A domain or URL is not what makes it public."}`

func TestLifecyclePendingExitAcrossCLI(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		for _, operation := range []string{"upload", "publish", "deploy", "rollback", "public", "private"} {
			t.Run(operation+map[bool]string{false: "-text", true: "-json"}[jsonMode], func(t *testing.T) {
				f, srv := newFakeAPI(t)
				var args []string
				switch operation {
				case "upload":
					dir := t.TempDir()
					writeTree(t, dir, map[string]string{"index.html": "DRAFT"})
					f.handle("POST /api/flats/demo/versions", 202, `{"version":{"number":0,"role":"draft","revision":2},"deploy":`+pendingLifecycle+`}`)
					args = []string{"deploy", dir, "--flat", "demo"}
				case "publish":
					f.handle("POST /api/flats/demo/publish", 202, pendingLifecycle)
					args = []string{"publish", "demo", "--revision", "2"}
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
	f.handle("GET /api/approvals", 200, `{"approvals":[{"id":"apr-control","flat":"demo","action":"publish","status":"pending"}]}`)
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
