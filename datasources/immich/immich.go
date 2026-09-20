/*
	Timelinize
	Copyright (c) 2013 Matthew Holt

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published
	by the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.
*/

// Package immich imports an offline Immich backup: an automatic PostgreSQL
// .sql/.sql.gz database backup together with the matching UPLOAD_LOCATION.
package immich

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/timelinize/timelinize/datasources/media"
	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

const (
	DataSourceID    = "immich"
	DataSourceTitle = "Immich"
)

func init() {
	err := timeline.RegisterDataSource(timeline.DataSource{
		Name:            DataSourceID,
		Title:           DataSourceTitle,
		Icon:            "immich.svg",
		Description:     "An Immich database backup and its original photo/video files, including EXIF, albums, tags, favorites, faces, and live photos.",
		NewOptions:      func() any { return new(Options) },
		NewFileImporter: func() timeline.FileImporter { return new(Importer) },
	})
	if err != nil {
		timeline.Log.Fatal("registering data source", zap.Error(err))
	}
}

// Options configures the Immich importer.
type Options struct {
	// OwnerEntityID is the Timelinize entity that owns the imported library.
	OwnerEntityID uint64 `json:"owner_entity_id"`

	// IncludeTrashed imports assets whose Immich deletedAt value is set.
	IncludeTrashed bool `json:"include_trashed"`
}

// Importer imports an offline Immich backup without modifying or contacting the
// Immich server.
type Importer struct{}

func (Importer) Recognize(_ context.Context, entry timeline.DirEntry, _ timeline.RecognizeParams) (timeline.Recognition, error) {
	dumpPath, err := findDatabaseDump(entry)
	if err != nil {
		return timeline.Recognition{}, nil
	}
	recognized, err := isImmichDump(entry.FS, dumpPath)
	if err != nil {
		return timeline.Recognition{}, err
	}
	if recognized {
		return timeline.Recognition{Confidence: 1}, nil
	}
	return timeline.Recognition{}, nil
}

func (Importer) FileImport(ctx context.Context, entry timeline.DirEntry, params timeline.ImportParams) error {
	options, ok := params.DataSourceOptions.(*Options)
	if !ok || options == nil {
		return errors.New("invalid Immich data source options")
	}

	dumpPath, err := findDatabaseDump(entry)
	if err != nil {
		return err
	}
	dumpFile, err := entry.FS.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("opening Immich database backup %s: %w", dumpPath, err)
	}
	defer dumpFile.Close()

	dumpReader, closeReader, err := decompressedReader(dumpFile, dumpPath)
	if err != nil {
		return err
	}
	if closeReader != nil {
		defer closeReader()
	}
	database, err := parseDatabaseDump(dumpReader)
	if err != nil {
		return fmt.Errorf("parsing Immich database backup %s: %w", dumpPath, err)
	}

	root := importRoot(entry, dumpPath)
	owner := timeline.Entity{}
	if options.OwnerEntityID > 0 {
		owner.ID = options.OwnerEntityID
	}

	livePhotoChildren := make(map[string]struct{})
	for _, asset := range database.assets {
		if asset.LivePhotoVideoID != "" {
			livePhotoChildren[asset.LivePhotoVideoID] = struct{}{}
		}
	}

	assetIDs := make([]string, 0, len(database.assets))
	for assetID := range database.assets {
		assetIDs = append(assetIDs, assetID)
	}
	sort.Strings(assetIDs)

	imported := make(map[string]struct{})
	missingFiles := 0
	for _, assetID := range assetIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if params.Continue != nil {
			if err := params.Continue(); err != nil {
				return err
			}
		}
		if _, isChild := livePhotoChildren[assetID]; isChild {
			continue
		}

		graph, buildErr := buildGraph(ctx, entry, root, database, assetID, owner, options, params, imported, make(map[string]bool))
		if buildErr != nil {
			if errors.Is(buildErr, fs.ErrNotExist) {
				missingFiles++
				params.Log.Warn("Immich original file not found; asset skipped",
					zap.String("asset_id", assetID),
					zap.String("original_path", database.assets[assetID].OriginalPath))
				continue
			}
			return buildErr
		}
		if graph == nil || graph.Item == nil || !params.Timeframe.ContainsItem(graph.Item, false) {
			continue
		}

		select {
		case params.Pipeline <- graph:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// A corrupt or mismatched backup should fail loudly rather than appear to
	// complete successfully with an empty timeline.
	if len(imported) == 0 && missingFiles > 0 {
		return fmt.Errorf("none of the %d Immich assets could be matched to original files under %s", len(database.assets), root)
	}
	if missingFiles > 0 {
		params.Log.Warn("some Immich assets were skipped because their originals were not in the selected backup",
			zap.Int("missing_files", missingFiles),
			zap.Int("imported_assets", len(imported)))
	}
	return nil
}

func buildGraph(
	ctx context.Context,
	entry timeline.DirEntry,
	root string,
	database *databaseDump,
	assetID string,
	owner timeline.Entity,
	options *Options,
	params timeline.ImportParams,
	imported map[string]struct{},
	visiting map[string]bool,
) (*timeline.Graph, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if visiting[assetID] {
		return nil, fmt.Errorf("Immich live-photo relationship contains a cycle at asset %s", assetID)
	}
	asset, ok := database.assets[assetID]
	if !ok {
		return nil, nil
	}
	if asset.DeletedAt != "" && !options.IncludeTrashed {
		return nil, nil
	}

	resolvedPath, err := resolveOriginalPath(entry.FS, root, asset.OriginalPath)
	if err != nil {
		return nil, err
	}
	filename := asset.OriginalFileName
	if filename == "" {
		filename = path.Base(resolvedPath)
	}
	mediaType := mime.TypeByExtension(strings.ToLower(path.Ext(filename)))
	item := &timeline.Item{
		ID:                   asset.ID,
		Classification:       timeline.ClassMedia,
		Owner:                owner,
		OriginalLocation:     asset.OriginalPath,
		IntermediateLocation: resolvedPath,
		Content: timeline.ItemData{
			Filename:  filename,
			MediaType: mediaType,
			Data: func(_ context.Context) (io.ReadCloser, error) {
				return entry.FS.Open(resolvedPath)
			},
		},
		Metadata: make(timeline.Metadata),
	}

	// As with the Google Photos importer, embedded metadata is preferred when
	// present; the database fills gaps and contributes Immich-only properties.
	if _, err := media.ExtractAllMetadata(params.Log, entry.FS, resolvedPath, item, timeline.MetaMergeAppend); err != nil {
		params.Log.Warn("extracting metadata from Immich original",
			zap.Error(err), zap.String("asset_id", asset.ID), zap.String("filepath", resolvedPath))
	}
	exif := database.exif[asset.ID]
	applyDatabaseMetadata(item, asset, exif, database)

	graph := &timeline.Graph{Item: item}
	addAlbums(graph, asset.ID, database, owner)
	addFaces(graph, asset.ID, database)

	visiting[assetID] = true
	if asset.LivePhotoVideoID != "" {
		motionGraph, motionErr := buildGraph(ctx, entry, root, database, asset.LivePhotoVideoID, owner, options, params, imported, visiting)
		if motionErr != nil && !errors.Is(motionErr, fs.ErrNotExist) {
			return nil, motionErr
		}
		if motionGraph != nil {
			graph.Edges = append(graph.Edges, timeline.Relationship{
				Relation: media.RelMotionPhoto,
				To:       motionGraph,
			})
		}
	}
	delete(visiting, assetID)
	imported[assetID] = struct{}{}
	return graph, nil
}

func applyDatabaseMetadata(item *timeline.Item, asset assetRecord, exif exifRecord, database *databaseDump) {
	if item.Timestamp.IsZero() {
		item.Timestamp = firstTimestamp(exif.DateTimeOriginal, asset.FileCreatedAt, asset.LocalDateTime, asset.CreatedAt)
	}
	if item.Location.Latitude == nil {
		item.Location.Latitude = parseFloat(exif.Latitude)
	}
	if item.Location.Longitude == nil {
		item.Location.Longitude = parseFloat(exif.Longitude)
	}

	metadata := timeline.Metadata{
		"Make":                       exif.Make,
		"Model":                      exif.Model,
		"Image width":                firstNonEmpty(exif.ExifImageWidth, asset.Width),
		"Image height":               firstNonEmpty(exif.ExifImageHeight, asset.Height),
		"File size in bytes":         exif.FileSizeInByte,
		"Orientation":                exif.Orientation,
		"Date/time original":         parseTimestamp(exif.DateTimeOriginal),
		"File modified":              parseTimestamp(asset.FileModifiedAt),
		"EXIF modified":              parseTimestamp(exif.ModifyDate),
		"Lens model":                 exif.LensModel,
		"F-number":                   numberValue(exif.FNumber),
		"Focal length":               numberValue(exif.FocalLength),
		"ISO":                        integerValue(exif.ISO),
		"City":                       exif.City,
		"State":                      exif.State,
		"Country":                    exif.Country,
		"Description":                exif.Description,
		"Frames per second":          numberValue(exif.FPS),
		"Exposure time":              exif.ExposureTime,
		"Live photo content ID":      exif.LivePhotoCID,
		"Time zone":                  exif.TimeZone,
		"Projection type":            exif.ProjectionType,
		"Color profile":              exif.ProfileDescription,
		"Color space":                exif.Colorspace,
		"Bits per sample":            integerValue(exif.BitsPerSample),
		"Rating":                     integerValue(exif.Rating),
		"Auto stack ID":              exif.AutoStackID,
		"Immich metadata updated":    parseTimestamp(exif.UpdatedAt),
		"Immich uploaded":            parseTimestamp(asset.CreatedAt),
		"Immich updated":             parseTimestamp(asset.UpdatedAt),
		"Immich asset type":          asset.Type,
		"Visibility":                 firstNonEmpty(asset.Visibility, archivedVisibility(asset.IsArchived)),
		"Status":                     asset.Status,
		"Video duration (ms)":        integerValue(asset.Duration),
		"Stack ID":                   asset.StackID,
		"Duplicate group ID":         asset.DuplicateID,
		"Checksum algorithm":         asset.ChecksumAlgorithm,
		"Favorite":                   trueOrNil(asset.IsFavorite),
		"Archived":                   trueOrNil(asset.IsArchived || strings.EqualFold(asset.Visibility, "archive")),
		"Trashed":                    trueOrNil(asset.DeletedAt != ""),
		"Offline":                    trueOrNil(asset.IsOffline),
		"External library":           trueOrNil(asset.IsExternal),
		"Edited":                     trueOrNil(asset.IsEdited),
		"EXIF tags":                  parsePGArray(exif.Tags),
		"Locked metadata properties": parsePGArray(exif.LockedProperties),
	}

	var tags []string
	for _, tagID := range database.assetTags[asset.ID] {
		if tag := database.tags[tagID]; tag.Value != "" {
			tags = append(tags, tag.Value)
		}
	}
	sort.Strings(tags)
	if len(tags) > 0 {
		metadata["Immich tags"] = tags
	}
	metadata.Clean()
	item.AddMetadata(metadata, timeline.MetaMergeSkip)
}

func addAlbums(graph *timeline.Graph, assetID string, database *databaseDump, owner timeline.Entity) {
	albumIDs := append([]string(nil), database.assetAlbums[assetID]...)
	sort.Strings(albumIDs)
	for _, albumID := range albumIDs {
		album, ok := database.albums[albumID]
		if !ok || album.DeletedAt != "" || (album.Name == "" && album.Description == "") {
			continue
		}
		name := album.Name
		if name == "" {
			name = album.Description
		}
		metadata := timeline.Metadata{
			"Description": album.Description,
			"Created":     parseTimestamp(album.CreatedAt),
			"Updated":     parseTimestamp(album.UpdatedAt),
		}
		metadata.Clean()
		graph.ToItem(timeline.RelInCollection, &timeline.Item{
			ID:             album.ID,
			Classification: timeline.ClassCollection,
			Owner:          owner,
			Content:        timeline.ItemData{Data: timeline.StringData(name)},
			Metadata:       metadata,
		})
	}
}

func addFaces(graph *timeline.Graph, assetID string, database *databaseDump) {
	for _, face := range database.assetFaces[assetID] {
		if face.DeletedAt != "" || !face.IsVisible {
			continue
		}
		person, ok := database.people[face.PersonID]
		if !ok {
			continue
		}
		entityMetadata := timeline.Metadata{
			"Birth date": parseTimestamp(person.BirthDate),
			"Favorite":   trueOrNil(person.IsFavorite),
			"Hidden":     trueOrNil(person.IsHidden),
			"Color":      person.Color,
		}
		entityMetadata.Clean()
		relationMetadata := timeline.Metadata{
			"Image width":      integerValue(face.ImageWidth),
			"Image height":     integerValue(face.ImageHeight),
			"Bounding box X1":  integerValue(face.BoundingBoxX1),
			"Bounding box Y1":  integerValue(face.BoundingBoxY1),
			"Bounding box X2":  integerValue(face.BoundingBoxX2),
			"Bounding box Y2":  integerValue(face.BoundingBoxY2),
			"Detection source": face.SourceType,
		}
		relationMetadata.Clean()
		graph.Edges = append(graph.Edges, timeline.Relationship{
			Relation: timeline.RelIncludes,
			To: &timeline.Graph{Entity: &timeline.Entity{
				Type:     "person",
				Name:     person.Name,
				Metadata: entityMetadata,
				Attributes: []timeline.Attribute{{
					Name:     "immich_person_group_id",
					Value:    person.ID,
					Identity: true,
				}},
			}},
			Metadata: relationMetadata,
		})
	}
}

func findDatabaseDump(entry timeline.DirEntry) (string, error) {
	if !entry.IsDir() {
		if isDumpFilename(entry.Filename) {
			return entry.Filename, nil
		}
		return "", fs.ErrNotExist
	}

	var backupCandidates, rootCandidates []string
	for index, directory := range []string{path.Join(entry.Filename, "backups"), entry.Filename} {
		children, err := fs.ReadDir(entry.FS, directory)
		if err != nil {
			continue
		}
		for _, child := range children {
			if !child.IsDir() && isDumpFilename(child.Name()) {
				candidate := path.Join(directory, child.Name())
				if index == 0 {
					backupCandidates = append(backupCandidates, candidate)
				} else {
					rootCandidates = append(rootCandidates, candidate)
				}
			}
		}
	}
	candidates := backupCandidates
	if len(candidates) == 0 {
		candidates = rootCandidates
	}
	if len(candidates) == 0 {
		return "", fs.ErrNotExist
	}
	sort.Strings(candidates)
	return candidates[len(candidates)-1], nil
}

func isDumpFilename(filename string) bool {
	lower := strings.ToLower(filename)
	return strings.HasSuffix(lower, ".sql") || strings.HasSuffix(lower, ".sql.gz")
}

func isImmichDump(fsys fs.FS, filename string) (bool, error) {
	file, err := fsys.Open(filename)
	if err != nil {
		return false, err
	}
	defer file.Close()
	reader, closeReader, err := decompressedReader(file, filename)
	if err != nil {
		return false, err
	}
	if closeReader != nil {
		defer closeReader()
	}
	content, err := io.ReadAll(io.LimitReader(reader, 4*1024*1024))
	if err != nil {
		return false, err
	}
	text := string(content)
	hasAssets := strings.Contains(text, "COPY public.asset (") ||
		strings.Contains(text, "COPY public.assets (") ||
		strings.Contains(text, "CREATE TABLE public.asset (") ||
		strings.Contains(text, "CREATE TABLE public.assets (")
	hasExif := strings.Contains(text, "COPY public.asset_exif (") ||
		strings.Contains(text, "COPY public.exif (") ||
		strings.Contains(text, "CREATE TABLE public.asset_exif (") ||
		strings.Contains(text, "CREATE TABLE public.exif (")
	return hasAssets && hasExif, nil
}

func decompressedReader(file fs.File, filename string) (io.Reader, func() error, error) {
	buffered := bufio.NewReader(file)
	header, _ := buffered.Peek(2)
	isGzip := strings.HasSuffix(strings.ToLower(filename), ".gz") || (len(header) == 2 && header[0] == 0x1f && header[1] == 0x8b)
	if !isGzip {
		return buffered, nil, nil
	}
	gzipReader, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, nil, fmt.Errorf("opening compressed Immich database backup %s: %w", filename, err)
	}
	return gzipReader, gzipReader.Close, nil
}

func importRoot(entry timeline.DirEntry, dumpPath string) string {
	if entry.IsDir() {
		return entry.Filename
	}
	root := path.Dir(dumpPath)
	if strings.EqualFold(path.Base(root), "backups") {
		root = path.Dir(root)
	}
	return root
}

func resolveOriginalPath(fsys fs.FS, root, originalPath string) (string, error) {
	normalized := strings.ReplaceAll(originalPath, "\\", "/")
	normalized = strings.TrimPrefix(path.Clean("/"+normalized), "/")
	seen := make(map[string]struct{})
	var candidates []string
	add := func(candidate string) {
		candidate = strings.TrimPrefix(path.Clean(candidate), "./")
		if candidate == "." || candidate == "" {
			return
		}
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}

	add(path.Join(root, normalized))
	parts := strings.Split(normalized, "/")
	for i, part := range parts {
		switch strings.ToLower(part) {
		case "upload", "library":
			add(path.Join(root, path.Join(parts[i:]...)))
		}
	}
	// Current Immich containers use /data as the media root; older releases
	// commonly used /usr/src/app/upload.
	for _, prefix := range []string{"data/", "usr/src/app/upload/"} {
		if index := strings.Index(strings.ToLower(normalized), prefix); index >= 0 {
			add(path.Join(root, normalized[index+len(prefix):]))
		}
	}

	for _, candidate := range candidates {
		info, err := fs.Stat(fsys, candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fs.ErrNotExist
}

func firstTimestamp(values ...string) time.Time {
	for _, value := range values {
		if parsed := parseTimestamp(value); !parsed.IsZero() {
			return parsed
		}
	}
	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseFloat(value string) *float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func numberValue(value string) any {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return nil
	}
	return parsed
}

func integerValue(value string) any {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return nil
	}
	return parsed
}

func trueOrNil(value bool) any {
	if value {
		return true
	}
	return nil
}

func archivedVisibility(archived bool) string {
	if archived {
		return "archive"
	}
	return ""
}
