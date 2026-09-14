package googlephotos

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/timelinize/timelinize/timeline"
)

type testDirectoryEntry struct{}

func (testDirectoryEntry) Name() string               { return "." }
func (testDirectoryEntry) IsDir() bool                { return true }
func (testDirectoryEntry) Type() fs.FileMode          { return fs.ModeDir }
func (testDirectoryEntry) Info() (fs.FileInfo, error) { return nil, nil }

func TestRecognizeExtractedGooglePhotosTakeout(t *testing.T) {
	fsys := fstest.MapFS{
		"shared_album_comments.json": &fstest.MapFile{Data: []byte("[]")},
	}
	entry := timeline.DirEntry{
		DirEntry: testDirectoryEntry{},
		FS:       fsys,
		FSRoot:   "/backup/Google Photos",
		Filename: ".",
	}
	rec, err := (FileImporter{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != .9 {
		t.Fatalf("Recognize confidence = %v, want .9", rec.Confidence)
	}
}
