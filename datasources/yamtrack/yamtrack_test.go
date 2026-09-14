package yamtrack_test

import (
	"context"
	"io"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timelinize/timelinize/datasources/yamtrack"
	"github.com/timelinize/timelinize/timeline"
)

const yamtrackCSV = `row_type,media_id,source,media_type,title,image,season_number,episode_number,score,progress,status,start_date,end_date,notes,progressed_at
media,10494,tmdb,movie,Perfect Blue,https://example.test/cover.jpg,,,9.0,1,Completed,,2024-02-09,,2024-02-09T15:30:00Z
media,1668,tmdb,episode,Friends,,1,24,,24,Completed,2024-02-09,2024-02-09,,
list,,,,Favorites,,,,,,,,,,
`

func TestRecognize(t *testing.T) {
	fsys := fstest.MapFS{
		"floppy_2026-09-13.csv": &fstest.MapFile{Data: []byte(yamtrackCSV)},
	}
	entry := timeline.DirEntry{FS: fsys, Filename: "floppy_2026-09-13.csv"}

	rec, err := (yamtrack.Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
}

func TestRecognizeRejectsOtherCSV(t *testing.T) {
	fsys := fstest.MapFS{
		"other.csv": &fstest.MapFile{Data: []byte("name,value\nfoo,bar\n")},
	}
	entry := timeline.DirEntry{FS: fsys, Filename: "other.csv"}

	rec, err := (yamtrack.Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 0 {
		t.Fatalf("Recognize confidence = %v, want 0", rec.Confidence)
	}
}

func TestFileImport(t *testing.T) {
	fsys := fstest.MapFS{
		"floppy_2026-09-13.csv": &fstest.MapFile{Data: []byte(yamtrackCSV)},
	}
	pipeline := make(chan *timeline.Graph, 10)
	params := timeline.ImportParams{
		Pipeline:          pipeline,
		DataSourceOptions: &yamtrack.Options{OwnerEntityID: 42},
	}
	entry := timeline.DirEntry{FS: fsys, Filename: "floppy_2026-09-13.csv"}

	if err := new(yamtrack.Importer).FileImport(context.Background(), entry, params); err != nil {
		t.Fatalf("FileImport returned error: %v", err)
	}
	close(pipeline)

	var items []*timeline.Item
	for graph := range pipeline {
		if graph.Item == nil {
			t.Fatal("import emitted a graph without an item")
		}
		items = append(items, graph.Item)
	}
	if len(items) != 2 {
		t.Fatalf("import emitted %d items, want 2", len(items))
	}

	movie := items[0]
	if movie.ID != "yamtrack:tmdb:movie:10494//" {
		t.Fatalf("movie ID = %q", movie.ID)
	}
	if movie.Classification.Name != timeline.ClassMedia.Name {
		t.Fatalf("movie classification = %v, want media", movie.Classification)
	}
	if movie.Owner.ID != 42 {
		t.Fatalf("movie owner ID = %d, want 42", movie.Owner.ID)
	}
	if got := contentString(t, movie); got != "Perfect Blue" {
		t.Fatalf("movie content = %q", got)
	}
	if got, ok := movie.Metadata["Progress"].(int64); !ok || got != 1 {
		t.Fatalf("movie progress = %#v, want int64(1)", movie.Metadata["Progress"])
	}
	wantTime, _ := time.Parse(time.RFC3339, "2024-02-09T15:30:00Z")
	if !movie.Timestamp.Equal(wantTime) {
		t.Fatalf("movie timestamp = %s, want %s", movie.Timestamp, wantTime)
	}

	episode := items[1]
	if episode.ID != "yamtrack:tmdb:episode:1668/1/24" {
		t.Fatalf("episode ID = %q", episode.ID)
	}
	if !episode.Timestamp.Equal(time.Date(2024, 2, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("episode timestamp = %s", episode.Timestamp)
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
