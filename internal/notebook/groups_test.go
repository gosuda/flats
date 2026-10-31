package notebook

import "testing"

func TestFixedGroupVocabulary(t *testing.T) {
	t.Parallel()
	expected := map[Kind][]GroupKey{PageKind: {TitleGroup, PlaceGroup}, BlockKind: {ContentGroup, PlaceGroup}, DatabaseKind: {TitleGroup, PropertyOrderGroup, ViewOrderGroup}, PropertyKind: {DefinitionGroup, OptionOrderGroup}, OptionKind: {DefinitionGroup}, RowKind: {PlaceGroup, CellGroup(vocabID(PropertyKind, 0))}, ViewKind: {DefinitionGroup}}
	all := []GroupKey{TitleGroup, PlaceGroup, ContentGroup, PropertyOrderGroup, ViewOrderGroup, DefinitionGroup, OptionOrderGroup, CellGroup(vocabID(PropertyKind, 0)), "", "content.text", "arbitrary", "cell:", CellGroup(vocabID(PageKind, 0)), "cell:pr_aaaaaaaaaaaaaaaaaaaaaaaaab"}
	for kind, allowed := range expected {
		t.Run(string(kind), func(t *testing.T) {
			for _, key := range all {
				want := false
				for _, k := range allowed {
					if k == key {
						want = true
					}
				}
				err := ValidateGroup(kind, key)
				if (err == nil) != want {
					t.Fatalf("%s group %q = %v, allowed=%v", kind, key, err, want)
				}
			}
		})
	}
	if err := ValidateGroup("unknown", TitleGroup); err == nil {
		t.Fatal("unknown owner kind accepted")
	}
}
