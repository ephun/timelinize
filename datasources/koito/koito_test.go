package koito_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timelinize/timelinize/datasources/koito"
	"github.com/timelinize/timelinize/timeline"
)

const koitoJSON = `{
  "version": "1",
  "exported_at": "2025-06-18T12:56:53Z",
  "user": "ethan",
  "listens": [
    {
      "listened_at": "2025-03-17T03:16:50Z",
      "client": "Navidrome",
      "track": {
        "mbid": "a4f26836-3894-46c1-acac-227808308687",
        "duration": 218,
        "aliases": [
          {"alias": "虹の色よ鮮やかであれ", "source": "Canonical", "is_primary": true}
        ]
      },
      "album": {
        "image_url": "https://example.test/cover.jpg",
        "mbid": "7114f07c-c1f7-4423-a9c3-585444bef51b",
        "aliases": [
          {"alias": "Nijinoiroyo Azayakadeare", "source": "MusicBrainz", "is_primary": false},
          {"alias": "虹の色よ鮮やかであれ", "source": "Canonical", "is_primary": true}
        ],
        "various_artists": false
      },
      "artists": [
        {
          "image_url": "https://example.test/artist.jpg",
          "mbid": "3d202d36-1219-4e31-bfb9-d73355c66a83",
          "is_primary": false,
          "aliases": [
            {"alias": "NELKE", "source": "Canonical", "is_primary": true}
          ]
        }
      ]
    }
  ]
}`

func TestRecognize(t *testing.T) {
	fsys := fstest.MapFS{
		"koito_export.json": &fstest.MapFile{Data: []byte(koitoJSON)},
	}
	entry := timeline.DirEntry{FS: fsys, Filename: "koito_export.json"}

	rec, err := (koito.Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
}

func TestFileImport(t *testing.T) {
	fsys := fstest.MapFS{
		"koito_export.json": &fstest.MapFile{Data: []byte(koitoJSON)},
	}
	pipeline := make(chan *timeline.Graph, 10)
	params := timeline.ImportParams{
		Pipeline:          pipeline,
		DataSourceOptions: &koito.Options{OwnerEntityID: 7},
	}
	entry := timeline.DirEntry{FS: fsys, Filename: "koito_export.json"}

	if err := new(koito.Importer).FileImport(context.Background(), entry, params); err != nil {
		t.Fatalf("FileImport returned error: %v", err)
	}
	close(pipeline)

	var items []*timeline.Item
	for graph := range pipeline {
		items = append(items, graph.Item)
	}
	if len(items) != 1 {
		t.Fatalf("import emitted %d items, want 1", len(items))
	}
	item := items[0]
	if !strings.HasPrefix(item.ID, "koito:") || len(item.ID) != len("koito:")+64 {
		t.Fatalf("unexpected item ID %q", item.ID)
	}
	if item.Classification.Name != timeline.ClassEvent.Name {
		t.Fatalf("classification = %v, want event", item.Classification)
	}
	if item.Owner.ID != 7 {
		t.Fatalf("owner ID = %d, want 7", item.Owner.ID)
	}
	if got := contentString(t, item); got != "NELKE — 虹の色よ鮮やかであれ [虹の色よ鮮やかであれ]" {
		t.Fatalf("content = %q", got)
	}
	if item.Timespan.Sub(item.Timestamp) != 218*time.Second {
		t.Fatalf("timespan duration = %s, want 3m38s", item.Timespan.Sub(item.Timestamp))
	}
	if got := item.Metadata["Client"]; got != "Navidrome" {
		t.Fatalf("client metadata = %#v", got)
	}
	if got := item.Metadata["Artist MusicBrainz IDs"].([]string); len(got) != 1 || got[0] != "3d202d36-1219-4e31-bfb9-d73355c66a83" {
		t.Fatalf("artist MBIDs = %#v", got)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2025-03-17T03:16:50Z")
	if !item.Timestamp.Equal(wantTime) {
		t.Fatalf("timestamp = %s, want %s", item.Timestamp, wantTime)
	}
}

func contentString(t *testing.T, item *timeline.Item) string {
	t.Helper()
	reader, err := item.Content.Data(context.Background())
	if err != nil {
		t.Fatalf("reading item content: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading item content bytes: %v", err)
	}
	return string(data)
}
