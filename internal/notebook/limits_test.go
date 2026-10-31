package notebook

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContentAdmission(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		before, after int
		wantErr       bool
	}{
		{name: "fresh at limit", after: 8000},
		{name: "fresh excess", after: 8001, wantErr: true},
		{name: "shrinking retained excess", before: 9000, after: 8500},
		{name: "unchanged retained excess", before: 8500, after: 8500},
		{name: "growing retained excess", before: 8500, after: 9000, wantErr: true},
		{name: "growing within limit", before: 1, after: 8000},
		{name: "removing excess", before: 9000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, kind := range []Kind{PageKind, BlockKind, RowKind} {
				t.Run(string(kind), func(t *testing.T) {
					measure := func(n int) int64 {
						text := strings.Repeat("😀", n)
						key := TitleGroup
						var value GroupValue = TitleValue(text)
						switch kind {
						case BlockKind:
							key = ContentGroup
							value = Content{Kind: Paragraph, Text: RichText{{Text: text, Marks: []Mark{}}}}
						case RowKind:
							key = CellGroup(vocabID(PropertyKind, 0))
							value = CellValue{Type: TextType, Value: text}
						}
						encoded, err := EncodeGroup(kind, key, value)
						if err != nil {
							t.Fatal(err)
						}
						decoded, err := DecodeGroup(kind, key, encoded)
						if err != nil {
							t.Fatal(err)
						}
						switch v := decoded.(type) {
						case TitleValue:
							text = string(v)
						case Content:
							text = ""
							for _, run := range v.Text {
								text += run.Text
							}
						case CellValue:
							text = v.Value.(string)
						}
						return int64(utf8.RuneCountInString(text))
					}
					err := CheckAdmission(measure(tt.before), measure(tt.after), MaxTitleCodePoints)
					if (err != nil) != tt.wantErr {
						t.Fatalf("admission %d -> %d: %v", tt.before, tt.after, err)
					}
				})
			}
		})
	}
}

func TestAdmissionMeasuresIndependently(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                 string
		before, after, limit int64
		wantErr              bool
	}{
		{name: "retained count", before: 40, after: 39, limit: MaxPropertiesPerDatabase},
		{name: "growing count", before: 40, after: 41, limit: MaxPropertiesPerDatabase, wantErr: true},
		{name: "URL bytes", before: 3000, after: 3000, limit: MaxURLBytes},
		{name: "all retained usage removed", before: math.MaxInt64, limit: 0},
		{name: "max usage unchanged", before: math.MaxInt64, after: math.MaxInt64, limit: 1},
		{name: "zero bound", after: 1, wantErr: true},
		{name: "negative before", before: -1, wantErr: true},
		{name: "negative after", after: -1, wantErr: true},
		{name: "negative limit", limit: -1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckAdmission(tt.before, tt.after, tt.limit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("admission: %v", err)
			}
		})
	}
}

func TestUpdateBodyBound(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		upload, want int64
	}{
		{"empty", 0, MaxAgentOperationBytes}, {"one byte", 1, MaxAgentOperationBytes + 4}, {"two bytes", 2, MaxAgentOperationBytes + 4}, {"three bytes", 3, MaxAgentOperationBytes + 4}, {"four bytes", 4, MaxAgentOperationBytes + 8},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MaxUpdateBodyBytes(tt.upload)
			if err != nil || got != tt.want {
				t.Fatalf("body bound = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
	for _, n := range []int64{-1, math.MaxInt64} {
		if _, err := MaxUpdateBodyBytes(n); err == nil {
			t.Fatalf("accepted invalid upload bound %d", n)
		}
	}
}
