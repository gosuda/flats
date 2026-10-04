package runtime

import (
	"fmt"
	"os"
	"testing"
)

func TestLargeHostAndCodecOwnership(t *testing.T) {
	count := 50
	if os.Getenv("FLATS_FULL_SOAK") == "1" {
		count = 200
	}
	for _, operation := range []string{
		`env.DB.query("SELECT ? AS text", [text])[0].text`,
		`env.FILES.put("large",text);env.FILES.get("large")`,
		`c.length(text);c.digest(text);c.decode(base64);c.encode(bytes);c.textEncoder.encode(text);c.textDecoder.decode(bytes)`,
	} {
		t.Run(operation, func(t *testing.T) {
			code := fmt.Sprintf(`const text="x".repeat(1048576), c=__flats_docsCodec, bytes=c.textEncoder.encode(text),base64=c.encode(bytes); export default {fetch(request,env){for(let i=0;i<10;i++){%s;}return new Response("ok");}};`, operation)
			f := mustStart(t, newManager(t), "ownership", map[string]string{"index.js": code}, "index.js", nil)
			for i := 0; i < count; i += 10 {
				r := f.do(t, "GET", "/", "")
				if r.status != 200 || r.body != "ok" {
					t.Fatalf("iteration %d: %d %s %s", i, r.status, r.body, f.logs)
				}
			}
			t.Logf("%d 1 MiB calls in the reused HTTP VM", count)
		})
	}
}

func TestLargeResponseOwnership(t *testing.T) {
	f := mustStart(t, newManager(t), "large-response", map[string]string{"index.js": `const text="x".repeat(1048576);export default {fetch(){return new Response(text)}};`}, "index.js", nil)
	for i := 0; i < 100; i++ {
		r := f.do(t, "GET", "/", "")
		if r.status != 200 || len(r.body) != 1048576 {
			t.Fatalf("GET %d: %d", i, r.status)
		}
	}
}
