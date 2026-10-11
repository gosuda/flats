package runtimeref

import "testing"

func TestGuideRevisionTracksRuntimeInstructions(t *testing.T) {
	first := GuideRevision(1000)
	if first != GuideRevision(1000) || len(first) != 64 {
		t.Fatal("guide revision must be stable SHA-256")
	}
	if first == GuideRevision(2000) {
		t.Fatal("upload limit change must refresh guide")
	}
	if _, ok := Topic("client-update"); !ok {
		t.Fatal("client update topic must be discoverable")
	}
}
