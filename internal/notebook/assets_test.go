package notebook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type liveHTTPBackend struct {
	mu        sync.Mutex
	exclusive sync.Mutex
	blobs     map[string][]byte
	retained  map[string]string
	limits    Limits
	leases    int
	puts      int
	failPut   bool
}

func (b *liveHTTPBackend) Limits() Limits { b.mu.Lock(); defer b.mu.Unlock(); return b.limits }
func (b *liveHTTPBackend) MeasureOther(context.Context) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int64
	for _, data := range b.blobs {
		n += int64(len(data))
	}
	return n, nil
}
func (b *liveHTTPBackend) PutBlob(_ context.Context, hash string, data []byte) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts++
	if b.failPut {
		return false, fmt.Errorf("blob write failed")
	}
	if _, ok := b.blobs[hash]; ok {
		return false, nil
	}
	b.blobs[hash] = bytes.Clone(data)
	return true, nil
}
func (b *liveHTTPBackend) ReadBlob(_ context.Context, hash string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.blobs[hash]
	if !ok {
		return nil, fmt.Errorf("blob not found")
	}
	return bytes.Clone(data), nil
}
func (b *liveHTTPBackend) Lease(_ []string) func() {
	b.mu.Lock()
	b.leases++
	b.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { b.mu.Lock(); b.leases--; b.mu.Unlock() }) }
}
func (b *liveHTTPBackend) Exclusive(ctx context.Context, fn func(context.Context, int64) error) error {
	b.exclusive.Lock()
	defer b.exclusive.Unlock()
	n, err := b.MeasureOther(ctx)
	if err != nil {
		return err
	}
	return fn(ctx, n)
}
func (*liveHTTPBackend) OutboxReady() {}
func (b *liveHTTPBackend) RevisionAsset(_ context.Context, hash string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	name, ok := b.retained[hash]
	return name, ok, nil
}

func liveHTTPImage(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func liveHTTPAssetFixture(t *testing.T) (*Instance, *liveHTTPBackend, Ref) {
	t.Helper()
	b := &liveHTTPBackend{blobs: map[string][]byte{}, retained: map[string]string{}, limits: Limits{UploadMax: 1 << 20}}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "notes", Title: "Notes"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Stop)
	var page Ref
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		var id ID
		if err := tx.QueryRowContext(t.Context(), `SELECT id FROM items WHERE kind='page' AND alive=1 LIMIT 1`).Scan(&id); err != nil {
			return err
		}
		r, err := loadRecord(t.Context(), tx, id)
		page = r.Item.Ref
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return in, b, page
}

func TestLiveHTTPAssetsUploadAndAccess(t *testing.T) {
	in, b, page := liveHTTPAssetFixture(t)
	data := liveHTTPImage(t)
	r := liveHTTPRequest("POST", "/_notebook/api/assets", string(data), "private")
	r.Header.Set("Content-Type", "image/svg+xml")
	w := httptest.NewRecorder()
	in.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var upload struct {
		Hash string `json:"hash"`
		Ext  string `json:"ext"`
		MIME string `json:"mime"`
		URL  string `json:"url"`
		W    int    `json:"w"`
		H    int    `json:"h"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if upload.Hash != hex.EncodeToString(sum[:]) || upload.Ext != "png" || upload.MIME != "image/png" || upload.W != 2 || upload.H != 3 {
		t.Fatalf("upload: %+v", upload)
	}
	var until int64
	if err := in.readers.QueryRowContext(t.Context(), `SELECT protected_until FROM assets WHERE hash=?`, upload.Hash).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if until < in.now().Unix()+3590 {
		t.Fatalf("staged protection: %d", until)
	}
	w = httptest.NewRecorder()
	in.ServeHTTP(w, liveHTTPRequest("GET", upload.URL, "", "public"))
	if w.Code != 404 {
		t.Fatalf("public staged asset: %d", w.Code)
	}
	result, err := in.Update(t.Context(), Update{Slug: "notes", OpID: "live-image", Ops: []Operation{CreateBlockOp{CreateFields: CreateFields{Ref: "image"}, Place: Placement{Page: Target{Ref: &page}}, Content: Content{Kind: Image, Asset: upload.Hash, Caption: RichText{}}}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{"public", "private", ""} {
		w = httptest.NewRecorder()
		in.ServeHTTP(w, liveHTTPRequest("GET", upload.URL, "", access))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("%s: %d %s", access, w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	in.ServeHTTP(w, liveHTTPRequest("HEAD", upload.URL, "", "public"))
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	in.ServeHTTP(w, liveHTTPRequest("GET", strings.TrimSuffix(upload.URL, ".png")+".jpg", "", "private"))
	if w.Code != 404 {
		t.Fatalf("wrong extension: %d", w.Code)
	}
	ref := result.Refs["image"].Ref
	if _, err := in.Update(t.Context(), Update{Slug: "notes", OpID: "remove-image", Ops: []Operation{DeleteBlocksOp{Blocks: []Target{{Ref: &ref}}}}}, 0); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.retained[upload.Hash] = "assets/" + upload.Hash + ".png"
	b.mu.Unlock()
	for _, access := range []string{"public", "private"} {
		w = httptest.NewRecorder()
		in.ServeHTTP(w, liveHTTPRequest("GET", upload.URL, "", access))
		want := 404
		if access == "private" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("historical %s: %d %s", access, w.Code, w.Body.String())
		}
	}
	b.mu.Lock()
	leases := b.leases
	b.mu.Unlock()
	if leases != 0 {
		t.Fatalf("leaked leases: %d", leases)
	}
}

func TestAssetsCaptureImageAccessAfterPruning(t *testing.T) {
	for _, cleanup := range []string{"expiry", "eviction"} {
		t.Run(cleanup, func(t *testing.T) {
			var now atomic.Int64
			now.Store(time.Now().Unix())
			b := &liveHTTPBackend{blobs: map[string][]byte{}, retained: map[string]string{}, limits: Limits{UploadMax: 1 << 20}}
			in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "notes", Title: "Notes", Now: func() time.Time { return time.Unix(now.Load(), 0) }})
			if err != nil {
				t.Fatal(err)
			}
			defer in.Stop()
			var page ID
			if err := in.readers.QueryRowContext(t.Context(), `SELECT id FROM pages LIMIT 1`).Scan(&page); err != nil {
				t.Fatal(err)
			}
			data := liveHTTPImage(t)
			prepared, err := in.PrepareUploads(t.Context(), []UploadAsset{{Ref: "image", Data: data}})
			if err != nil {
				t.Fatal(err)
			}
			hash := prepared["image"]
			pageRef := historyItem(t, in, page).Item.Ref
			created := historyEdit(t, in,
				CreateBlockOp{CreateFields: CreateFields{Ref: "image"}, Place: Placement{Page: Target{Ref: &pageRef}}, Content: Content{Kind: Image, Asset: hash, Caption: RichText{}}},
				CreateBlockOp{CreateFields: CreateFields{Ref: "text"}, Place: Placement{Page: Target{Ref: &pageRef}}, Content: Content{Kind: Paragraph, Text: RichText{{Text: "After the image", Marks: []Mark{}}}}},
			)
			src := historySource(t, in, 2)
			imageRef := created.Refs["image"]
			historyEdit(t, in, DeleteBlocksOp{Blocks: []Target{{Ref: &imageRef}}})
			b.mu.Lock()
			b.retained[hash] = "assets/" + hash + ".png"
			b.mu.Unlock()
			query, err := json.Marshal(map[string]any{"page": page, "limit": 2})
			if err != nil {
				t.Fatal(err)
			}
			captured, err := in.ReadRevision(t.Context(), src, query)
			if err != nil {
				t.Fatal(err)
			}
			var capture struct {
				Assets map[string]string `json:"assets"`
				Next   string            `json:"next"`
			}
			if err := json.Unmarshal(captured, &capture); err != nil {
				t.Fatal(err)
			}
			if capture.Next == "" {
				t.Fatalf("missing continuation: %s", captured)
			}
			if capture.Assets[hash] == "" {
				continuation, err := json.Marshal(map[string]string{"cursor": capture.Next})
				if err != nil {
					t.Fatal(err)
				}
				last, err := in.Read(t.Context(), continuation, false)
				if err != nil {
					t.Fatal(err)
				}
				var tail struct {
					Assets map[string]string `json:"assets"`
				}
				if err := json.Unmarshal(last, &tail); err != nil {
					t.Fatal(err)
				}
				capture.Assets = tail.Assets
			}
			url := capture.Assets[hash]
			if url == "" {
				t.Fatal("capture has no image URL")
			}
			get := func(instance *Instance, url, access string, want int) {
				t.Helper()
				w := httptest.NewRecorder()
				instance.ServeHTTP(w, liveHTTPRequest("GET", url, "", access))
				if w.Code != want {
					t.Errorf("%s image: got %d, want %d: %s", access, w.Code, want, w.Body)
				}
				if want == 200 && (!bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store") {
					t.Errorf("image response: headers=%v body=%x", w.Header(), w.Body.Bytes())
				}
			}
			get(in, url, "private", 200)
			get(in, url, "public", 404)
			b.mu.Lock()
			clear(b.retained)
			b.mu.Unlock()
			get(in, url, "private", 200)
			get(in, url, "public", 404)
			get(in, strings.TrimSuffix(url, ".png")+".jpg", "private", 404)
			other, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "other", Title: "Other"})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Stop()
			get(other, url, "private", 404)
			continuation, err := json.Marshal(map[string]string{"cursor": capture.Next})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := in.Read(t.Context(), continuation, false); err != nil {
				t.Fatalf("capture continuation after pruning: %v", err)
			}
			if cleanup == "expiry" {
				now.Add(61)
			} else {
				for i := 0; i < 4; i++ {
					now.Add(1)
					if _, err := in.ReadRevision(t.Context(), historySource(t, in, i+3), []byte(`{}`)); err != nil {
						t.Fatal(err)
					}
				}
			}
			get(in, url, "private", 404)
			get(in, url, "public", 404)
			if _, err := in.Read(t.Context(), continuation, false); err == nil {
				t.Fatal("cleaned-up capture still readable")
			}
		})
	}
}

func TestLiveHTTPAssetsPreparation(t *testing.T) {
	in, b, _ := liveHTTPAssetFixture(t)
	data := liveHTTPImage(t)
	mapping, err := in.PrepareUploads(t.Context(), []UploadAsset{{Ref: "first", Data: data}, {Ref: "second", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 2 || mapping["first"] != mapping["second"] {
		t.Fatal(mapping)
	}
	paths, err := in.PrepareAssets(t.Context(), []AssetInput{{Path: "picture.png", Asset: AssetTarget{Hash: mapping["first"]}}})
	if err != nil || paths["picture.png"] != mapping["first"] {
		t.Fatalf("prepared paths: %v %v", paths, err)
	}
	if _, err := in.PrepareAssets(t.Context(), []AssetInput{{Path: "picture.png", Asset: AssetTarget{Tmp: "first"}}}); err == nil {
		t.Fatal("unresolved temporary name accepted")
	}
	if _, err := in.PrepareAssets(t.Context(), []AssetInput{{Path: "x", Asset: AssetTarget{Hash: mapping["first"]}}, {Path: "x", Asset: AssetTarget{Hash: mapping["first"]}}}); err == nil {
		t.Fatal("duplicate path accepted")
	}
	if _, err := in.PrepareUploads(t.Context(), []UploadAsset{{Ref: "bad", Data: []byte(`<svg/>`)}}); err == nil {
		t.Fatal("SVG accepted")
	}
	if _, err := in.PrepareUploads(t.Context(), []UploadAsset{{Ref: "x", Data: data}, {Ref: "x", Data: data}}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	b.mu.Lock()
	b.limits.UploadMax = int64(len(data))
	b.mu.Unlock()
	if _, err := in.PrepareUploads(t.Context(), []UploadAsset{{Ref: "x", Data: data}, {Ref: "y", Data: data}}); err == nil {
		t.Fatal("aggregate upload limit not enforced")
	}
	b.mu.Lock()
	b.blobs[mapping["first"]] = []byte("corrupt")
	b.mu.Unlock()
	if _, err := in.PrepareAssets(t.Context(), []AssetInput{{Path: "picture.png", Asset: AssetTarget{Hash: mapping["first"]}}}); err == nil {
		t.Fatal("corrupt blob accepted")
	}
}

func TestLiveHTTPAssetsRefusal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*liveHTTPBackend)
		status    int
	}{
		{"upload limit", func(b *liveHTTPBackend) { b.limits.UploadMax = 1 }, 413},
		{"quota", func(b *liveHTTPBackend) { b.limits.DiskQuota = 1 }, 413},
		{"blob failure", func(b *liveHTTPBackend) { b.failPut = true }, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, b, _ := liveHTTPAssetFixture(t)
			b.mu.Lock()
			tc.configure(b)
			b.mu.Unlock()
			w := httptest.NewRecorder()
			in.ServeHTTP(w, liveHTTPRequest("POST", "/_notebook/api/assets", string(liveHTTPImage(t)), "private"))
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			var count int
			if err := in.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial metadata: %d %v", count, err)
			}
			b.mu.Lock()
			puts, leases := b.puts, b.leases
			b.mu.Unlock()
			if leases != 0 || tc.name == "quota" && puts != 0 {
				t.Fatalf("puts=%d leases=%d", puts, leases)
			}
		})
	}
}
