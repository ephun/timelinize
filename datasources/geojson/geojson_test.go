package geojson_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/timelinize/timelinize/datasources/geojson"
	"github.com/timelinize/timelinize/timeline"
)

func TestRecognizeDawarichJSONExport(t *testing.T) {
	fsys := fstest.MapFS{
		"points.json": &fstest.MapFile{Data: []byte(`{"type":"FeatureCollection","features":[]}`)},
	}
	entry := timeline.DirEntry{FS: fsys, Filename: "points.json"}

	rec, err := (geojson.FileImporter{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
}

func TestRecognizeRejectsUnrelatedJSON(t *testing.T) {
	fsys := fstest.MapFS{
		"manifest.json": &fstest.MapFile{Data: []byte(`{"name":"not geojson"}`)},
	}
	entry := timeline.DirEntry{FS: fsys, Filename: "manifest.json"}

	rec, err := (geojson.FileImporter{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 0 {
		t.Fatalf("Recognize confidence = %v, want 0", rec.Confidence)
	}
}
