package discord

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

func TestRecognizeChatExporterCSV(t *testing.T) {
	fsys := fstest.MapFS{
		"discord.csv": &fstest.MapFile{Data: []byte("AuthorID,Author,Date,Content,Attachments,Reactions\n123,Pat,2024-01-02T03:04:05Z,hello,,\n")},
	}
	entry := timeline.DirEntry{DirEntry: fixtureDirEntry{name: "discord.csv"}, FS: fsys, Filename: "discord.csv"}
	rec, err := (Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatalf("Recognize returned error: %v", err)
	}
	if rec.Confidence != 1 {
		t.Fatalf("Recognize confidence = %v, want 1", rec.Confidence)
	}
}

func TestImportChatExporterCSV(t *testing.T) {
	fsys := fstest.MapFS{
		"discord.csv": &fstest.MapFile{Data: []byte("AuthorID,Author,Date,Content,Attachments,Reactions\n123,Pat,2024-01-02T03:04:05Z,hello,,\n")},
	}
	pipeline := make(chan *timeline.Graph, 1)
	err := (&Importer{}).FileImport(context.Background(), timeline.DirEntry{DirEntry: fixtureDirEntry{name: "discord.csv"}, FS: fsys, Filename: "discord.csv"}, timeline.ImportParams{
		Pipeline:          pipeline,
		Log:               zap.NewNop(),
		DataSourceOptions: &Options{},
	})
	if err != nil {
		t.Fatalf("FileImport returned error: %v", err)
	}
	graph := <-pipeline
	if graph.Item == nil || graph.Item.Classification.Name != timeline.ClassMessage.Name {
		t.Fatalf("imported graph = %#v, want message item", graph)
	}
	if graph.Item.Owner.AttributeValue("discord_user_id") != "123" {
		t.Fatalf("owner Discord ID = %v, want 123", graph.Item.Owner.AttributeValue("discord_user_id"))
	}
}

func TestRecognizeOfficialDataPackage(t *testing.T) {
	fsys := fstest.MapFS{
		"Messages/index.json":         &fstest.MapFile{Data: []byte(`{"123": "General"}`)},
		"Messages/c123/channel.json":  &fstest.MapFile{Data: []byte(`{"id":"123","name":"General","type":"GuildText","guild":{"name":"Test"}}`)},
		"Messages/c123/messages.json": &fstest.MapFile{Data: []byte(`[{"ID":42,"Timestamp":"2024-01-02T03:04:05Z","Contents":"hello","Attachments":""}]`)},
	}
	entry := timeline.DirEntry{DirEntry: fixtureDirEntry{name: ".", dir: true}, FS: fsys, Filename: "."}
	rec, err := (Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
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
	if graph.Item == nil || graph.Item.ID != "discord:package:42" {
		t.Fatalf("imported package graph = %#v", graph)
	}
	if graph.Item.Metadata["Channel"] != "General" {
		t.Fatalf("channel metadata = %v, want General", graph.Item.Metadata["Channel"])
	}
}
