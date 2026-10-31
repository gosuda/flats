package notebook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func notebookAssetUnheld(string) (bool, error) { return false, nil }

func TestAssetsRuntimeRoots(t *testing.T) {
	for _, root := range []string{"live", "deleted", "undo desired", "undo written", "redo desired", "redo written", "outbox", "snapshot", "upload protection", "host protection"} {
		t.Run(root, func(t *testing.T) {
			f := newWriterFixture(t)
			hash, orphan := strings.Repeat("a", 64), strings.Repeat("b", 64)
			err := f.in.write(t.Context(), func(tx *sql.Tx) error {
				for _, h := range []string{hash, orphan} {
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO assets VALUES(?,'image/png',1,1,99,0,0,0)`, h); err != nil {
						return err
					}
				}
				image := itemRecord{Item: Item{Ref: Ref{ID: vocabID(BlockKind, 1), Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Block{Page: f.page.ID, Position: "a", Content: Content{Kind: Image, Asset: hash, Caption: RichText{}}}}, Alive: true}
				switch root {
				case "live", "deleted":
					if root == "deleted" {
						image.Alive, image.DeletedAt = false, 2
					}
					return putRecord(t.Context(), tx, image)
				case "undo desired", "undo written", "redo desired", "redo written":
					text := image
					block := text.Item.Data.(Block)
					block.Content = Content{Kind: Paragraph, Text: RichText{}}
					text.Item.Data = block
					part := undoPart{Written: text, Desired: &image, Groups: []GroupKey{ContentGroup}}
					if strings.HasSuffix(root, "written") {
						part.Written, part.Desired = image, &text
					}
					stack := strings.Fields(root)[0]
					return saveUndoEntry(t.Context(), tx, contentHash([]byte(f.session.Token)), stack, 1, undoEntry{Components: []undoComponent{{Parts: []undoPart{part}}}})
				case "outbox":
					files, err := json.Marshal([]File{{Path: "assets/" + hash + ".png", Hash: hash, Size: 99}})
					if err != nil {
						return err
					}
					_, err = tx.ExecContext(t.Context(), `INSERT INTO outbox(kind,authors,message,files,hashes) VALUES('edit','[]','',?,'{"hashes":[]}')`, string(files))
					return err
				case "snapshot":
					_, err := tx.ExecContext(t.Context(), `INSERT INTO snapshots(hash,data,refs) VALUES(?,X'01',1)`, hash)
					return err
				case "upload protection":
					_, err := tx.ExecContext(t.Context(), `UPDATE assets SET protected_until=? WHERE hash=?`, f.clock.Load()+1, hash)
					return err
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			protect := func(h string) (bool, error) { return root == "host protection" && h == hash, nil }
			roots, err := f.in.SweepReferences(t.Context(), protect)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(roots, hash) || slices.Contains(roots, orphan) {
				t.Fatalf("runtime roots = %v", roots)
			}
			if count := writerCount(t, f.in, "assets"); count != 1 {
				t.Fatalf("surviving assets = %d, want 1", count)
			}
			if root == "upload protection" {
				f.clock.Add(1)
				roots, err = f.in.SweepReferences(t.Context(), notebookAssetUnheld)
				if err != nil || slices.Contains(roots, hash) || writerCount(t, f.in, "assets") != 0 {
					t.Fatalf("expired protection: roots=%v err=%v", roots, err)
				}
			}
		})
	}
}

func TestAssetsSweepFailureIsAtomic(t *testing.T) {
	for _, failure := range []string{"host lookup", "undo lookup", "outbox lookup", "asset deletion"} {
		t.Run(failure, func(t *testing.T) {
			f := newWriterFixture(t)
			a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
			if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
				for _, hash := range []string{a, b} {
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO assets VALUES(?,'image/png',1,1,99,0,0,0)`, hash); err != nil {
						return err
					}
				}
				switch failure {
				case "undo lookup":
					_, err := tx.ExecContext(t.Context(), `INSERT INTO undo VALUES(?,'undo',1,'broken',1)`, contentHash([]byte(f.session.Token)))
					return err
				case "outbox lookup":
					_, err := tx.ExecContext(t.Context(), `INSERT INTO outbox(kind,authors,message,files,hashes) VALUES('edit','[]','','broken','{}')`)
					return err
				case "asset deletion":
					_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER refuse_asset_delete BEFORE DELETE ON assets WHEN OLD.hash='`+b+`' BEGIN SELECT RAISE(ABORT,'unavailable'); END`)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			protect := func(hash string) (bool, error) {
				if failure == "host lookup" && hash == b {
					return false, errors.New("reference lookup unavailable")
				}
				return false, nil
			}
			roots, err := f.in.SweepReferences(t.Context(), protect)
			if err == nil || roots != nil {
				t.Fatalf("failed collection returned roots=%v err=%v", roots, err)
			}
			if count := writerCount(t, f.in, "assets"); count != 2 {
				t.Fatalf("failed collection retained %d assets, want 2", count)
			}
		})
	}
}
