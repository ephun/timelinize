package immich

import (
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timelinize/timelinize/datasources/media"
	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

const currentSchemaDump = `-- PostgreSQL database dump
COPY public.asset ("id", "ownerId", "type", "originalPath", "fileCreatedAt", "fileModifiedAt", "isFavorite", "duration", "livePhotoVideoId", "updatedAt", "createdAt", "originalFileName", "isOffline", "isExternal", "deletedAt", "localDateTime", "stackId", "duplicateId", "status", "visibility", "width", "height", "isEdited") FROM stdin;
photo-1	owner-1	IMAGE	/data/library/owner-1/photo.jpg	2024-03-02 10:11:12+00	2024-03-03 10:11:12+00	t	\N	video-1	2024-03-04 10:11:12+00	2024-03-01 10:11:12+00	photo.jpg	f	f	\N	2024-03-02 10:11:12	stack-1	duplicate-1	active	archive	4000	3000	t
video-1	owner-1	VIDEO	/data/library/owner-1/photo.mov	2024-03-02 10:11:12+00	2024-03-03 10:11:12+00	f	2500	\N	2024-03-04 10:11:12+00	2024-03-01 10:11:12+00	photo.mov	f	f	\N	2024-03-02 10:11:12	\N	\N	active	timeline	1920	1080	f
\.
COPY public.asset_exif ("assetId", "make", "model", "exifImageWidth", "exifImageHeight", "fileSizeInByte", "orientation", "dateTimeOriginal", "modifyDate", "lensModel", "fNumber", "focalLength", "iso", "latitude", "longitude", "city", "state", "country", "description", "fps", "exposureTime", "livePhotoCID", "timeZone", "projectionType", "profileDescription", "colorspace", "bitsPerSample", "autoStackId", "rating", "tags", "updatedAt", "lockedProperties") FROM stdin;
photo-1	Canon	EOS R5	4000	3000	123456	1	2024-03-02 10:11:12+00	2024-03-03 10:11:12+00	RF 24-70mm	2.8	35	200	40.7608	-111.8910	Salt Lake City	Utah	USA	A rich description	\N	1/125	live-content-id	America/Denver	EQUIRECTANGULAR	sRGB IEC61966-2.1	sRGB	14	auto-stack-1	5	{"family","vacation"}	2024-03-04 10:11:12+00	{description,rating}
\.
COPY public.album ("id", "albumName", "createdAt", "updatedAt", "description", "deletedAt") FROM stdin;
album-1	Road Trip	2024-03-01 00:00:00+00	2024-03-05 00:00:00+00	Spring trip	\N
\.
COPY public.album_asset ("albumId", "assetId") FROM stdin;
album-1	photo-1
\.
COPY public.tag ("id", "userId", "value", "color", "parentId") FROM stdin;
tag-1	owner-1	Places/Utah	#ff0000	\N
\.
COPY public.tag_asset ("assetId", "tagId") FROM stdin;
photo-1	tag-1
\.
COPY public.person ("ownerId", "personGroupId", "name", "isHidden", "birthDate", "isFavorite", "color") FROM stdin;
owner-1	person-1	Ethan	f	1990-01-02	t	#00ff00
\.
COPY public.asset_face ("id", "assetId", "personGroupId", "imageWidth", "imageHeight", "boundingBoxX1", "boundingBoxY1", "boundingBoxX2", "boundingBoxY2", "sourceType", "deletedAt", "isVisible") FROM stdin;
face-1	photo-1	person-1	4000	3000	100	200	300	500	machine-learning	\N	t
\.
`

func TestParseDatabaseDumpCurrentSchema(t *testing.T) {
	dump, err := parseDatabaseDump(bytes.NewBufferString(currentSchemaDump))
	if err != nil {
		t.Fatal(err)
	}
	if got := dump.assets["photo-1"].OriginalFileName; got != "photo.jpg" {
		t.Fatalf("original filename = %q", got)
	}
	if got := dump.exif["photo-1"].LensModel; got != "RF 24-70mm" {
		t.Fatalf("lens model = %q", got)
	}
	if got := dump.assetAlbums["photo-1"]; len(got) != 1 || got[0] != "album-1" {
		t.Fatalf("asset albums = %#v", got)
	}
	if got := dump.assetFaces["photo-1"]; len(got) != 1 || got[0].PersonID != "person-1" {
		t.Fatalf("asset faces = %#v", got)
	}
}

func TestParseDatabaseDumpLegacyNames(t *testing.T) {
	legacy := `COPY public.assets (id, "originalPath", "originalFileName") FROM stdin;
asset-1	/usr/src/app/upload/upload/user/file.jpg	file.jpg
\.
COPY public.exif ("assetId", make) FROM stdin;
asset-1	Apple
\.
COPY public.albums (id, "albumName") FROM stdin;
album-1	Legacy Album
\.
COPY public.albums_assets_assets ("albumsId", "assetsId") FROM stdin;
album-1	asset-1
\.
COPY public.people (id, name) FROM stdin;
person-1	Someone
\.
COPY public.asset_faces ("assetId", "personId") FROM stdin;
asset-1	person-1
\.
`
	dump, err := parseDatabaseDump(bytes.NewBufferString(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if dump.exif["asset-1"].Make != "Apple" || dump.albums["album-1"].Name != "Legacy Album" {
		t.Fatalf("legacy rows not parsed: %#v", dump)
	}
	if got := dump.assetFaces["asset-1"][0].PersonID; got != "person-1" {
		t.Fatalf("legacy person ID = %q", got)
	}
}

func TestRecognizeCompressedBackup(t *testing.T) {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write([]byte(currentSchemaDump)); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	fsys := fstest.MapFS{"immich.sql.gz": &fstest.MapFile{Data: compressed.Bytes()}}
	entry := mapEntry(t, fsys, "immich.sql.gz")
	recognition, err := (Importer{}).Recognize(context.Background(), entry, timeline.RecognizeParams{})
	if err != nil {
		t.Fatal(err)
	}
	if recognition.Confidence != 1 {
		t.Fatalf("confidence = %v", recognition.Confidence)
	}
}

func TestFileImportPreservesRichMetadataAndRelationships(t *testing.T) {
	fsys := fstest.MapFS{
		"backup/backups/immich-db-backup-20260302.sql": &fstest.MapFile{Data: []byte(currentSchemaDump)},
		"backup/library/owner-1/photo.jpg":             &fstest.MapFile{Data: []byte("not a real jpeg")},
		"backup/library/owner-1/photo.mov":             &fstest.MapFile{Data: []byte("not a real mov")},
	}
	entry := mapEntry(t, fsys, "backup")
	pipeline := make(chan *timeline.Graph, 2)
	err := (Importer{}).FileImport(context.Background(), entry, timeline.ImportParams{
		Pipeline:          pipeline,
		Log:               zap.NewNop(),
		DataSourceOptions: &Options{OwnerEntityID: 42},
	})
	if err != nil {
		t.Fatal(err)
	}
	close(pipeline)

	graphs := make([]*timeline.Graph, 0, 1)
	for graph := range pipeline {
		graphs = append(graphs, graph)
	}
	if len(graphs) != 1 {
		t.Fatalf("graphs = %d, want 1", len(graphs))
	}
	graph := graphs[0]
	if graph.Item.ID != "photo-1" || graph.Item.Owner.ID != 42 {
		t.Fatalf("item identity = %#v", graph.Item)
	}
	if want := time.Date(2024, 3, 2, 10, 11, 12, 0, time.UTC); !graph.Item.Timestamp.Equal(want) {
		t.Fatalf("timestamp = %s, want %s", graph.Item.Timestamp, want)
	}
	if graph.Item.Location.Latitude == nil || *graph.Item.Location.Latitude != 40.7608 {
		t.Fatalf("latitude = %#v", graph.Item.Location.Latitude)
	}
	if got := graph.Item.Metadata["Lens model"]; got != "RF 24-70mm" {
		t.Fatalf("lens model metadata = %#v", got)
	}
	if got := graph.Item.Metadata["Description"]; got != "A rich description" {
		t.Fatalf("description metadata = %#v", got)
	}
	if got, ok := graph.Item.Metadata["Immich tags"].([]string); !ok || len(got) != 1 || got[0] != "Places/Utah" {
		t.Fatalf("Immich tags = %#v", graph.Item.Metadata["Immich tags"])
	}

	var album, face, motion bool
	for _, edge := range graph.Edges {
		switch edge.Relation.Label {
		case timeline.RelInCollection.Label:
			album = edge.To != nil && edge.To.Item != nil && edge.To.Item.ID == "album-1"
		case timeline.RelIncludes.Label:
			face = edge.To != nil && edge.To.Entity != nil && edge.To.Entity.Name == "Ethan"
		case media.RelMotionPhoto.Label:
			motion = edge.To != nil && edge.To.Item != nil && edge.To.Item.ID == "video-1"
		}
	}
	if !album || !face || !motion {
		t.Fatalf("relationships: album=%v face=%v motion=%v", album, face, motion)
	}
}

func TestResolveOriginalPath(t *testing.T) {
	fsys := fstest.MapFS{
		"root/upload/user/a.jpg":  &fstest.MapFile{},
		"root/library/user/b.jpg": &fstest.MapFile{},
	}
	tests := map[string]string{
		"/usr/src/app/upload/upload/user/a.jpg": "root/upload/user/a.jpg",
		"/data/library/user/b.jpg":              "root/library/user/b.jpg",
	}
	for input, expected := range tests {
		got, err := resolveOriginalPath(fsys, "root", input)
		if err != nil {
			t.Fatalf("resolve %q: %v", input, err)
		}
		if got != expected {
			t.Fatalf("resolve %q = %q, want %q", input, got, expected)
		}
	}
}

func mapEntry(t *testing.T, fsys fstest.MapFS, filename string) timeline.DirEntry {
	t.Helper()
	info, err := fs.Stat(fsys, filename)
	if err != nil {
		t.Fatal(err)
	}
	return timeline.DirEntry{
		DirEntry: fs.FileInfoToDirEntry(info),
		FS:       fsys,
		Filename: filename,
	}
}
