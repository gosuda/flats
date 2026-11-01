package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/imagecheck"
)

type notebookBlob struct {
	info imagecheck.AssetInfo
	data []byte
}

// PrepareAssets checks and stages existing image hashes keyed by Markdown path.
func (in *Instance) PrepareAssets(ctx context.Context, assets []AssetInput) (map[string]string, error) {
	result := make(map[string]string, len(assets))
	blobs := make([]notebookBlob, 0, len(assets))
	hashes := make([]string, 0, len(assets))
	for _, asset := range assets {
		if asset.Path == "" || asset.Asset.Tmp != "" || validateAsset(asset.Asset.Hash) != nil {
			return nil, &WriteError{Code: "invalid", Reason: "asset", Detail: "asset paths need a prepared image hash"}
		}
		if _, exists := result[asset.Path]; exists {
			return nil, &WriteError{Code: "invalid", Reason: "asset", Detail: "duplicate asset path"}
		}
		result[asset.Path] = asset.Asset.Hash
		hashes = append(hashes, asset.Asset.Hash)
	}
	if len(assets) == 0 {
		return result, nil
	}
	release := in.b.Lease(hashes)
	defer release()
	for _, asset := range assets {
		data, err := in.b.ReadBlob(ctx, asset.Asset.Hash)
		if err != nil {
			return nil, err
		}
		info, err := imagecheck.CheckImage(data, in.b.Limits().UploadMax)
		if err != nil || info.Hash != asset.Asset.Hash {
			return nil, &WriteError{Code: "invalid", Reason: "asset", Detail: "asset bytes are not the named checked image"}
		}
		blobs = append(blobs, notebookBlob{info, data})
	}
	if err := in.stageNotebookBlobs(ctx, blobs); err != nil {
		return nil, err
	}
	return result, nil
}

// PrepareUploads stages decoded update assets and returns temporary name to hash.
// UploadAsset carries bytes; the frozen AssetInput contract carries only hashes.
func (in *Instance) PrepareUploads(ctx context.Context, assets []UploadAsset) (map[string]string, error) {
	result := make(map[string]string, len(assets))
	blobs := make([]notebookBlob, 0, len(assets))
	var total int64
	limit := in.b.Limits().UploadMax
	for _, asset := range assets {
		if asset.Ref == "" {
			return nil, &WriteError{Code: "invalid", Reason: "asset"}
		}
		if _, exists := result[asset.Ref]; exists {
			return nil, &WriteError{Code: "invalid", Reason: "asset", Detail: "duplicate asset temporary name"}
		}
		var err error
		total, err = quotaAdd(total, int64(len(asset.Data)))
		if err != nil || limit > 0 && total > limit {
			return nil, &WriteError{Code: "notebook_capacity", Reason: "upload_limit"}
		}
		info, err := imagecheck.CheckImage(asset.Data, limit)
		if err != nil {
			return nil, &WriteError{Code: "invalid", Reason: "image", Detail: err.Error()}
		}
		result[asset.Ref] = info.Hash
		blobs = append(blobs, notebookBlob{info, asset.Data})
	}
	if err := in.stageNotebookBlobs(ctx, blobs); err != nil {
		return nil, err
	}
	return result, nil
}

func holdNotebookBlobs(ctx context.Context, tx *sql.Tx, blobs []notebookBlob, until int64) error {
	for _, blob := range blobs {
		a := blob.info
		_, err := tx.ExecContext(ctx, `INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until)
			VALUES(?,?,?,?,?,0,0,?) ON CONFLICT(hash) DO UPDATE SET mime=excluded.mime,width=excluded.width,
			height=excluded.height,bytes=excluded.bytes,protected_until=max(assets.protected_until,excluded.protected_until)`,
			a.Hash, a.Mime, a.W, a.H, a.Bytes, until)
		if err != nil {
			return err
		}
	}
	return nil
}

func (in *Instance) stageNotebookBlobs(ctx context.Context, blobs []notebookBlob) error {
	if len(blobs) == 0 {
		return nil
	}
	unique := make([]notebookBlob, 0, len(blobs))
	seen := map[string]bool{}
	hashes := []string{}
	var reserved int64
	for _, blob := range blobs {
		if seen[blob.info.Hash] {
			continue
		}
		seen[blob.info.Hash] = true
		unique = append(unique, blob)
		hashes = append(hashes, blob.info.Hash)
		var err error
		reserved, err = quotaAdd(reserved, blob.info.Bytes)
		if err != nil {
			return err
		}
	}
	release := in.b.Lease(hashes)
	defer release()
	exclusive := in.b.Exclusive
	if other, ok := ctx.Value(assetMeasurementKey{}).(int64); ok {
		exclusive = func(ctx context.Context, fn func(context.Context, int64) error) error { return fn(ctx, other) }
	}
	return exclusive(ctx, func(ctx context.Context, other int64) error {
		for _, blob := range unique {
			stored, err := in.b.ReadBlob(ctx, blob.info.Hash)
			if err == nil {
				if !bytes.Equal(stored, blob.data) {
					return unavailable("asset_corrupt")
				}
				reserved -= blob.info.Bytes
			}
		}
		if err := in.measure.reserve(reserved); err != nil {
			return err
		}
		defer func() { _ = in.measure.reserve(-reserved) }()
		until := in.now().Add(time.Hour).Unix()
		if err := in.write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SAVEPOINT notebook_assets`); err != nil {
				return err
			}
			err := holdNotebookBlobs(ctx, tx, unique, until)
			if err == nil {
				err = in.checkQuota(ctx, tx, other, 0)
			}
			return rollbackSavepoint(ctx, tx, "notebook_assets", err)
		}); err != nil {
			return err
		}
		for _, blob := range unique {
			created, err := in.b.PutBlob(ctx, blob.info.Hash, blob.data)
			if created {
				in.measure.add(blob.info.Bytes)
			}
			if err != nil {
				in.measure.refresh()
				return err
			}
		}
		if err := in.measure.reserve(-reserved); err != nil {
			return err
		}
		reserved = 0
		err := in.write(ctx, func(tx *sql.Tx) error {
			if err := holdNotebookBlobs(ctx, tx, unique, until); err != nil {
				return err
			}
			return in.checkQuota(ctx, tx, other, 0)
		})
		in.measure.refresh()
		return err
	})
}

func notebookImageExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return ""
}

func (in *Instance) serveNotebookUpload(w http.ResponseWriter, r *http.Request) {
	limit := in.b.Limits().UploadMax
	body := io.Reader(r.Body)
	if limit > 0 {
		body = http.MaxBytesReader(w, r.Body, limit)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		writeNotebookError(w, err)
		return
	}
	info, err := imagecheck.CheckImage(data, limit)
	if err != nil {
		writeNotebookError(w, &WriteError{Code: "invalid", Reason: "image", Detail: err.Error()})
		return
	}
	if err := in.stageNotebookBlobs(r.Context(), []notebookBlob{{info, data}}); err != nil {
		writeNotebookError(w, err)
		return
	}
	ext := notebookImageExt(info.Mime)
	writeNotebookJSON(w, http.StatusOK, map[string]any{"hash": info.Hash, "ext": ext, "mime": info.Mime, "w": info.W, "h": info.H, "bytes": info.Bytes, "url": "/_notebook/a/" + info.Hash + "." + ext})
}

func (in *Instance) serveNotebookAsset(w http.ResponseWriter, r *http.Request, public bool) {
	if !notebookReadMethod(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/_notebook/a/")
	ext := strings.TrimPrefix(path.Ext(name), ".")
	hash := strings.TrimSuffix(name, "."+ext)
	if validateAsset(hash) != nil || ext == "" {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	release := in.b.Lease([]string{hash})
	defer release()
	var mime string
	var allowed bool
	err := in.ReadTx(ctx, func(tx *sql.Tx, _ Sequence) error {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM items WHERE kind='block' AND alive=1
			AND json_extract(data,'$.content.kind')='image' AND json_extract(data,'$.content.asset')=?)`, hash).Scan(&live); err != nil {
			return err
		}
		if public && !live {
			return nil
		}
		err := tx.QueryRowContext(ctx, `SELECT mime FROM assets WHERE hash=?`, hash).Scan(&mime)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		allowed = err == nil && live
		return err
	})
	if err != nil {
		writeNotebookError(w, err)
		return
	}
	if !public && !allowed {
		mime, allowed = in.historyAsset(hash)
	}
	if !public && !allowed {
		if history, ok := in.b.(interface {
			RevisionAsset(context.Context, string) (string, bool, error)
		}); ok {
			var retained string
			retained, allowed, err = history.RevisionAsset(ctx, hash)
			if err != nil {
				writeNotebookError(w, err)
				return
			}
			if allowed {
				mime = map[string]string{"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}[strings.TrimPrefix(path.Ext(retained), ".")]
			}
		}
	}
	if !allowed || notebookImageExt(mime) != ext {
		http.NotFound(w, r)
		return
	}
	data, err := in.b.ReadBlob(ctx, hash)
	if err != nil {
		writeNotebookError(w, err)
		return
	}
	if err := ctx.Err(); err != nil {
		writeNotebookError(w, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	controller := http.NewResponseController(w)
	stopWrite := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()) })
	defer stopWrite()
	for len(data) > 0 {
		if ctx.Err() != nil {
			return
		}
		_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
		n := min(len(data), 64<<10)
		if _, err := w.Write(data[:n]); err != nil {
			return
		}
		data = data[n:]
	}
}
