package runtime

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const engineLimitsApp = `
export default {
  async fetch(request, env) {
    const u = new URL(request.url);
    switch (u.pathname) {
      case "/len": return Response.json({ len: request.body.length });
      case "/bigquery": {
        const rows = env.DB.query("SELECT hex(zeroblob(5500000)) AS v");
        return Response.json({ len: rows[0].v.length });
      }
      case "/fput": {
        env.FILES.put(u.searchParams.get("k"), request.body);
        return Response.json({ ok: true });
      }
      case "/fget": {
        const v = env.FILES.get(u.searchParams.get("k"));
        return Response.json({ len: v.length });
      }
      case "/fput-toobig": {
        try {
          env.FILES.put("toobig", "x".repeat(10 * 1024 * 1024 + 1));
          return Response.json({ ok: true });
        } catch (e) {
          return Response.json({ err: e.message });
        }
      }
    }
    return new Response("?", { status: 404 });
  }
}`

func newEngineLimitsFlat(t *testing.T) *flat {
	t.Helper()
	m := newManager(t)
	return mustStart(t, m, "engine-limits", map[string]string{"index.js": engineLimitsApp}, "index.js", nil)
}

func wantLen(n int) string {
	return fmt.Sprintf(`"len":%d`, n)
}

func TestRequestEnvelopeLimits(t *testing.T) {
	t.Parallel()
	f := newEngineLimitsFlat(t)

	// NUL bytes escape to six JSON bytes each in the request envelope.
	r := f.do(t, "POST", "/len", strings.Repeat("\x00", 2<<20))
	if r.status != http.StatusOK || !strings.Contains(r.body, wantLen(2<<20)) {
		t.Fatalf("2 MiB NUL body = %d %q; logs:\n%s", r.status, r.body, f.logs)
	}

	// A body of exactly MaxRequestBody fits the envelope.
	r = f.do(t, "POST", "/len", strings.Repeat("x", MaxRequestBody))
	if r.status != http.StatusOK || !strings.Contains(r.body, wantLen(MaxRequestBody)) {
		t.Fatalf("%d-byte ASCII body = %d %q; logs:\n%s", MaxRequestBody, r.status, r.body, f.logs)
	}
}

func TestHostPayloadLimits(t *testing.T) {
	t.Parallel()
	f := newEngineLimitsFlat(t)

	// An 11 MB unescaped query result fits the host result budget.
	r := f.do(t, "POST", "/bigquery", "")
	if r.status != http.StatusOK || !strings.Contains(r.body, wantLen(11000000)) {
		t.Fatalf("hex(zeroblob(5500000)) query = %d %q; logs:\n%s", r.status, r.body, f.logs)
	}

	// A file value of exactly MaxFileValue round-trips.
	r = f.do(t, "POST", "/fput?k=full", strings.Repeat("y", MaxFileValue))
	if r.status != http.StatusOK {
		t.Fatalf("put %d-byte value = %d %q; logs:\n%s", MaxFileValue, r.status, r.body, f.logs)
	}
	r = f.do(t, "POST", "/fget?k=full", "")
	if r.status != http.StatusOK || !strings.Contains(r.body, wantLen(MaxFileValue)) {
		t.Fatalf("get full value = %d %q; logs:\n%s", r.status, r.body, f.logs)
	}

	// One byte over MaxFileValue is refused with the documented message.
	r = f.do(t, "POST", "/fput-toobig", "")
	want := fmt.Sprintf(`value for \"toobig\" is %d bytes; the limit is %d MiB`, MaxFileValue+1, MaxFileValue>>20)
	if r.status != http.StatusOK || !strings.Contains(r.body, want) {
		t.Fatalf("oversize put = %d %q; want %q; logs:\n%s", r.status, r.body, want, f.logs)
	}
}
