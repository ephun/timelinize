// Package youtube imports the stable activity and playlist files produced by
// Google Takeout's My Activity and YouTube exports.
package youtube

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

const (
	DataSourceID    = "google_takeout_activity"
	DataSourceTitle = "Google Takeout Activity"
)

type fileKind uint8

const (
	kindUnknown fileKind = iota
	kindActivityHTML
	kindPlaylistCSV
	kindVideoMetadataCSV
)

func init() {
	err := timeline.RegisterDataSource(timeline.DataSource{
		Name:            DataSourceID,
		Title:           DataSourceTitle,
		Icon:            "folder.svg",
		Description:     "Read-only Google Takeout My Activity and YouTube history/playlist exports.",
		NewOptions:      func() any { return new(Options) },
		NewFileImporter: func() timeline.FileImporter { return new(Importer) },
	})
	if err != nil {
		timeline.Log.Fatal("registering data source", zap.Error(err))
	}
}

// Options configures the data source.
type Options struct {
	OwnerEntityID uint64 `json:"owner_entity_id"`
}

// Importer imports YouTube history cards and playlist membership timestamps.
type Importer struct{}

func (Importer) Recognize(_ context.Context, dirEntry timeline.DirEntry, _ timeline.RecognizeParams) (timeline.Recognition, error) {
	for _, candidate := range []string{dirEntry.FullPath(), dirEntry.Filename, dirEntry.Name()} {
		if kind := kindForPath(candidate); kind != kindUnknown {
			if kind == kindActivityHTML || strings.EqualFold(path.Ext(candidate), ".csv") {
				return timeline.Recognition{Confidence: 1}, nil
			}
		}
	}
	return timeline.Recognition{}, nil
}

func (i *Importer) FileImport(ctx context.Context, dirEntry timeline.DirEntry, params timeline.ImportParams) error {
	dsOpt, ok := params.DataSourceOptions.(*Options)
	if !ok || dsOpt == nil {
		return fmt.Errorf("invalid YouTube data source options")
	}
	if kind := kindForPath(dirEntry.FullPath()); kind != kindUnknown {
		return i.importFile(ctx, dirEntry.FS, dirEntry.Filename, kind, params, dsOpt)
	}
	if kind := kindForPath(dirEntry.Filename); kind != kindUnknown {
		return i.importFile(ctx, dirEntry.FS, dirEntry.Filename, kind, params, dsOpt)
	}
	if !dirEntry.IsDir() {
		return fmt.Errorf("unrecognized YouTube export path %q", dirEntry.Filename)
	}
	return fs.WalkDir(dirEntry.FS, dirEntry.Filename, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if kind := kindForPath(fpath); kind != kindUnknown {
			return i.importFile(ctx, dirEntry.FS, fpath, kind, params, dsOpt)
		}
		return nil
	})
}

func (i *Importer) importFile(ctx context.Context, fsys fs.FS, filename string, kind fileKind, params timeline.ImportParams, dsOpt *Options) error {
	switch kind {
	case kindActivityHTML:
		return i.importActivityHTML(ctx, fsys, filename, params, dsOpt)
	case kindPlaylistCSV:
		return i.importPlaylistCSV(ctx, fsys, filename, params, dsOpt)
	case kindVideoMetadataCSV:
		return i.importVideoMetadataCSV(ctx, fsys, filename, params, dsOpt)
	default:
		return fmt.Errorf("unrecognized YouTube export file %q", filename)
	}
}

func kindForPath(filename string) fileKind {
	if filename == "" {
		return kindUnknown
	}
	normalized := "/" + strings.TrimPrefix(strings.ToLower(filepath.ToSlash(filename)), "/")
	if strings.Contains(normalized, "/my activity/") && strings.HasSuffix(normalized, "/myactivity.html") {
		return kindActivityHTML
	}
	if strings.Contains(normalized, "/youtube and youtube music/history/") {
		if strings.HasSuffix(normalized, "/watch-history.html") || strings.HasSuffix(normalized, "/search-history.html") {
			return kindActivityHTML
		}
	}
	if strings.Contains(normalized, "/youtube and youtube music/playlists/") && strings.HasSuffix(normalized, ".csv") {
		return kindPlaylistCSV
	}
	if strings.Contains(normalized, "/youtube and youtube music/videos/") && strings.HasSuffix(normalized, "/video metadata.csv") {
		return kindVideoMetadataCSV
	}
	return kindUnknown
}

func (i *Importer) importActivityHTML(ctx context.Context, fsys fs.FS, filename string, params timeline.ImportParams, dsOpt *Options) error {
	file, err := fsys.Open(filename)
	if err != nil {
		return fmt.Errorf("opening YouTube activity HTML: %w", err)
	}
	defer file.Close()
	doc, err := goquery.NewDocumentFromReader(file)
	if err != nil {
		return fmt.Errorf("parsing YouTube activity HTML: %w", err)
	}

	owner := timeline.Entity{ID: dsOpt.OwnerEntityID}
	rowNumber := 0
	doc.Find(".outer-cell").EachWithBreak(func(_ int, card *goquery.Selection) bool {
		if ctx.Err() != nil {
			return false
		}
		rowNumber++
		text := cleanText(card.Text())
		when := parseActivityTimestamp(text)
		title, href := firstLink(card)
		kind := cleanText(card.Find(".header-cell").Text())
		if kind == "" {
			kind = path.Base(filename)
		}
		content := title
		if content == "" {
			content = text
		}
		item := &timeline.Item{
			ID:                   fmt.Sprintf("youtube:activity:%s:%d", filename, rowNumber),
			Classification:       timeline.ClassPageView,
			Timestamp:            when,
			Owner:                owner,
			OriginalLocation:     filename,
			IntermediateLocation: filename,
			Content: timeline.ItemData{
				Data:      timeline.StringData(content),
				MediaType: "text/plain",
			},
			Metadata: timeline.Metadata{
				"Activity": text,
				"Type":     kind,
				"Title":    title,
				"URL":      href,
			},
		}
		item.Metadata.Clean()
		if params.Timeframe.ContainsItem(item, false) {
			select {
			case <-ctx.Done():
			case params.Pipeline <- &timeline.Graph{Item: item}:
			}
		}
		return true
	})
	return ctx.Err()
}

func (i *Importer) importPlaylistCSV(ctx context.Context, fsys fs.FS, filename string, params timeline.ImportParams, dsOpt *Options) error {
	file, err := fsys.Open(filename)
	if err != nil {
		return fmt.Errorf("opening YouTube playlist CSV: %w", err)
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	var playlistTitle string
	playlistTitleIndex := -1
	videoIDIndex, timeAddedIndex := -1, -1
	rowNumber := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading YouTube playlist CSV row %d: %w", rowNumber+1, err)
		}
		rowNumber++
		if len(record) == 0 || (len(record) == 1 && strings.TrimSpace(record[0]) == "") {
			continue
		}
		header := headerIndexes(record)
		if index, ok := header["video id"]; ok {
			videoIDIndex = index
		}
		if index, ok := header["time added"]; ok {
			timeAddedIndex = index
		}
		if index, ok := header["title"]; ok && playlistTitleIndex < 0 {
			playlistTitleIndex = index
		}
		if playlistTitle == "" && playlistTitleIndex >= 0 && playlistTitleIndex < len(record) {
			candidate := strings.TrimSpace(record[playlistTitleIndex])
			if !strings.EqualFold(candidate, "title") {
				playlistTitle = candidate
			}
		}
		if videoIDIndex < 0 || timeAddedIndex < 0 || videoIDIndex >= len(record) || timeAddedIndex >= len(record) {
			continue
		}
		videoID := strings.TrimSpace(record[videoIDIndex])
		if strings.EqualFold(videoID, "video id") || videoID == "" {
			continue
		}
		when, err := parsePlaylistTimestamp(strings.TrimSpace(record[timeAddedIndex]))
		if err != nil {
			return fmt.Errorf("parsing YouTube playlist CSV row %d timestamp: %w", rowNumber, err)
		}
		item := &timeline.Item{
			ID:                   fmt.Sprintf("youtube:playlist:%s:%d", filename, rowNumber),
			Classification:       timeline.ClassEvent,
			Timestamp:            when,
			Owner:                timeline.Entity{ID: dsOpt.OwnerEntityID},
			OriginalLocation:     filename,
			IntermediateLocation: filename,
			Content: timeline.ItemData{
				Data:      timeline.StringData("https://www.youtube.com/watch?v=" + videoID),
				MediaType: "text/plain",
			},
			Metadata: timeline.Metadata{
				"Video ID": videoID,
				"Playlist": playlistTitle,
			},
		}
		item.Metadata.Clean()
		if !params.Timeframe.ContainsItem(item, false) {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case params.Pipeline <- &timeline.Graph{Item: item}:
		}
	}
}

func (i *Importer) importVideoMetadataCSV(ctx context.Context, fsys fs.FS, filename string, params timeline.ImportParams, dsOpt *Options) error {
	file, err := fsys.Open(filename)
	if err != nil {
		return fmt.Errorf("opening YouTube video metadata CSV: %w", err)
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return fmt.Errorf("reading YouTube video metadata CSV headers: %w", err)
	}
	fields := headerIndexes(headers)
	for _, required := range []string{"video id", "title", "time created"} {
		if _, ok := fields[required]; !ok {
			return fmt.Errorf("YouTube video metadata CSV is missing required column %q", required)
		}
	}
	rowNumber := 1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading YouTube video metadata CSV row %d: %w", rowNumber+1, err)
		}
		rowNumber++
		row := rowValues(fields, record)
		videoID := strings.TrimSpace(row["video id"])
		if videoID == "" {
			continue
		}
		when := firstTimestamp(row["time recorded"], row["time published"], row["time created"])
		item := &timeline.Item{
			ID:                   fmt.Sprintf("google_takeout_activity:video:%s:%d", filename, rowNumber),
			Classification:       timeline.ClassMedia,
			Timestamp:            when,
			Owner:                timeline.Entity{ID: dsOpt.OwnerEntityID},
			OriginalLocation:     filename,
			IntermediateLocation: filename,
			Content: timeline.ItemData{
				Data:      timeline.StringData("https://www.youtube.com/watch?v=" + videoID),
				MediaType: "text/plain",
			},
			Metadata: timeline.Metadata{
				"Video ID":       videoID,
				"Title":          strings.TrimSpace(row["title"]),
				"Status":         strings.TrimSpace(row["status"]),
				"Visibility":     strings.TrimSpace(row["visibility"]),
				"Time created":   strings.TrimSpace(row["time created"]),
				"Time published": strings.TrimSpace(row["time published"]),
				"Time recorded":  strings.TrimSpace(row["time recorded"]),
				"Duration":       strings.TrimSpace(row["duration"]),
				"Description":    strings.TrimSpace(row["description"]),
				"Category":       strings.TrimSpace(row["category"]),
				"View count":     strings.TrimSpace(row["view count"]),
			},
		}
		item.Metadata.Clean()
		if !params.Timeframe.ContainsItem(item, false) {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case params.Pipeline <- &timeline.Graph{Item: item}:
		}
	}
}

func firstTimestamp(values ...string) time.Time {
	for _, value := range values {
		if parsed, err := parsePlaylistTimestamp(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func firstLink(selection *goquery.Selection) (string, string) {
	var title, href string
	selection.Find("a").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		href = strings.TrimSpace(link.AttrOr("href", ""))
		title = cleanText(link.Text())
		return href == "" && title == ""
	})
	return title, href
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(value, "\u00a0", " ")), " ")
}

var (
	humanTimestampPattern = regexp.MustCompile(`(?i)\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2},\s+\d{4},\s+\d{1,2}:\d{2}:\d{2}\s+(?:AM|PM)\s+(?:[A-Z]{2,5})\b`)
	rfc3339Pattern        = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})\b`)
)

func parseActivityTimestamp(text string) time.Time {
	matches := humanTimestampPattern.FindAllString(text, -1)
	if len(matches) > 0 {
		value := matches[len(matches)-1]
		if parsed, err := parseHumanTimestamp(value); err == nil {
			return parsed
		}
	}
	if value := rfc3339Pattern.FindString(text); value != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func parseHumanTimestamp(value string) (time.Time, error) {
	parts := strings.Fields(value)
	zone := parts[len(parts)-1]
	offsets := map[string]int{"UTC": 0, "PST": -8 * 60 * 60, "PDT": -7 * 60 * 60, "MST": -7 * 60 * 60, "MDT": -6 * 60 * 60, "CST": -6 * 60 * 60, "CDT": -5 * 60 * 60, "EST": -5 * 60 * 60, "EDT": -4 * 60 * 60}
	if offset, ok := offsets[strings.ToUpper(zone)]; ok {
		return time.ParseInLocation("Jan 2, 2006, 3:04:05 PM MST", value, time.FixedZone(zone, offset))
	}
	return time.Parse("Jan 2, 2006, 3:04:05 PM MST", value)
}

func parsePlaylistTimestamp(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

func rowValues(fields map[string]int, record []string) map[string]string {
	row := make(map[string]string, len(fields))
	for field, index := range fields {
		if index < len(record) {
			row[field] = record[index]
		}
	}
	return row
}

func headerIndexes(record []string) map[string]int {
	indexes := make(map[string]int, len(record))
	for index, field := range record {
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(field, "\ufeff")))
		if name != "" {
			indexes[name] = index
		}
	}
	return indexes
}
