package notebook

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReadConvertedRowPreservesJSONBytes(t *testing.T) {
	const source = `{"cells":{},"text":"quotes: \" \\ and 한"}`
	const want = `{"cells":{},"text":"quotes: \" \\ and 한","database_ref":{"id":"d_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"7"},"property_refs":{},"option_refs":{}}`
	path := filepath.Join(t.TempDir(), "row.json")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	row := &readConvertedAt{
		data: data, node: spillNode{length: int64(len(source))}, kind: RowKind,
		fields: map[string]Ref{"database_ref": {ID: "d_aaaaaaaaaaaaaaaaaaaaaaaaaa", Inc: 7}},
	}
	size := &readConvertedOutput{source: data}
	if err := row.write(size); err != nil || size.position != int64(len(want)) {
		t.Fatalf("converted size = %d, %v", size.position, err)
	}
	row.length = size.position
	for _, chunk := range []int{1, 7, len(want)} {
		var got bytes.Buffer
		for offset := int64(0); offset < row.length; {
			part := make([]byte, min(chunk, int(row.length-offset)))
			n, err := row.ReadAt(part, offset)
			if err != nil && err != io.EOF {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("converted row made no progress")
			}
			got.Write(part[:n])
			offset += int64(n)
		}
		if got.String() != want {
			t.Fatalf("chunk %d changed row bytes: %s", chunk, got.String())
		}
	}
}
