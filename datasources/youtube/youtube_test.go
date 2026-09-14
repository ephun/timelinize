package youtube

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

type fixtureDirEntry struct {
	name string
	dir  bool
}

func (e fixtureDirEntry) Name() string { return e.name }
func (e fixtureDirEntry) IsDir() bool  { return e.dir }
func (e fixtureDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e fixtureDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

const activityFixture = `<html><body><div class="outer-cell"><div class="header-cell">YouTube</div><div class="content-cell">Watched <a href="https://www.youtube.com/watch?v=abc123">Example video</a><br>Jan 2, 2024, 3:04:05 PM UTC</div></div></body></html>`

func TestRecognizeYouTubeActivityHTML(t *testing.T) {
	entry := timeline.DirEntry{DirEntry: fixtureDirEntry{name: "watch-history.html"}, Filename: "Google/Alt 1/YouTube and YouTube Music/history/watch-history.html"}
	rec, err := (Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
}

func TestRecognizeGoogleMyActivityHTML(t *testing.T) {
	entry := timeline.DirEntry{DirEntry: fixtureDirEntry{name: "MyActivity.html"}, Filename: "Google/Alt 1/My Activity/Search/MyActivity.html"}
	rec, err := (Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
}

func TestImportYouTubeActivityHTML(t *testing.T) {
	fsys := fstest.MapFS{
		"Google/Alt 1/YouTube and YouTube Music/history/watch-history.html": &fstest.MapFile{Data: []byte(activityFixture)},
	}
	pipeline := make(chan *timeline.Graph, 1)
	err := (&Importer{}).FileImport(context.Background(), timeline.DirEntry{
		DirEntry: fixtureDirEntry{name: "watch-history.html"},
		FS:       fsys,
		Filename: "Google/Alt 1/YouTube and YouTube Music/history/watch-history.html",
	}, timeline.ImportParams{
		Pipeline:          pipeline,
		Log:               zap.NewNop(),
		DataSourceOptions: &Options{},
	})
	if err != nil {
		t.Fatalf("FileImport returned error: %v", err)
	}
	graph := <-pipeline
	if graph.Item == nil || graph.Item.Classification.Name != timeline.ClassPageView.Name {
		t.Fatalf("imported graph = %#v, want page-view item", graph)
	}
	if graph.Item.Metadata["Title"] != "Example video" {
		t.Fatalf("title metadata = %v, want Example video", graph.Item.Metadata["Title"])
	}
}

func TestRecognizeAndImportYouTubePlaylistCSV(t *testing.T) {
	const fixture = "Playlist Id,Channel Id,Time Created,Time Updated,Title,Description,Visibility\nplaylist,channel,2024-01-01 00:00:00 UTC,2024-01-01 00:00:00 UTC,Watch later,,Private\n\nVideo Id,Time Added\nabc123,2024-01-02 03:04:05 UTC\n"
	filename := "Google/Alt 1/YouTube and YouTube Music/playlists/Watch later.csv"
	fsys := fstest.MapFS{filename: &fstest.MapFile{Data: []byte(fixture)}}
	entry := timeline.DirEntry{DirEntry: fixtureDirEntry{name: "Watch later.csv"}, FS: fsys, Filename: filename}
	rec, err := (&Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
	pipeline := make(chan *timeline.Graph, 1)
	err = (&Importer{}).FileImport(context.Background(), entry, timeline.ImportParams{
		Pipeline:          pipeline,
		Log:               zap.NewNop(),
		DataSourceOptions: &Options{},
	})
	if err != nil {
		t.Fatalf("FileImport returned error: %v", err)
	}
	graph := <-pipeline
	if graph.Item == nil || graph.Item.Metadata["Playlist"] != "Watch later" {
		t.Fatalf("imported playlist graph = %#v, want Watch later metadata", graph)
	}
}

func TestRecognizeAndImportYouTubeVideoMetadataCSV(t *testing.T) {
	const fixture = "Video Id,Channel Id,Title,Status,Visibility,Time Created,Time Published,Duration,Description,Category,View Count\nabc123,channel,Example,processed,Private,2024-01-01 00:00:00 UTC,2024-01-01 00:00:00 UTC,42 seconds,,Music,3\n"
	filename := "Google/Alt 1/YouTube and YouTube Music/videos/Video Metadata.csv"
	fsys := fstest.MapFS{filename: &fstest.MapFile{Data: []byte(fixture)}}
	entry := timeline.DirEntry{DirEntry: fixtureDirEntry{name: "Video Metadata.csv"}, FS: fsys, Filename: filename}
	rec, err := (&Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
	pipeline := make(chan *timeline.Graph, 1)
	err = (&Importer{}).FileImport(context.Background(), entry, timeline.ImportParams{
		Pipeline:          pipeline,
		Log:               zap.NewNop(),
		DataSourceOptions: &Options{},
	})
	if err != nil {
		t.Fatalf("FileImport returned error: %v", err)
	}
	graph := <-pipeline
	if graph.Item == nil || graph.Item.Metadata["Title"] != "Example" {
		t.Fatalf("imported video graph = %#v, want Example metadata", graph)
	}
}
