/*
	Timelinize
	Copyright (c) 2013 Matthew Holt

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published
	by the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

// Package yamtrack imports the CSV format shared by Yamtrack and Floppy.
package yamtrack

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

const (
	DataSourceID    = "yamtrack"
	DataSourceTitle = "Yamtrack / Floppy"
)

func init() {
	err := timeline.RegisterDataSource(timeline.DataSource{
		Name:            DataSourceID,
		Title:           DataSourceTitle,
		Icon:            "yamtrack.svg",
		Description:     "A Yamtrack or Floppy CSV export of tracked media state.",
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

// Importer imports the media rows from a Yamtrack/Floppy CSV export.
type Importer struct{}

// Recognize returns whether the input has the shared Yamtrack/Floppy columns.
func (Importer) Recognize(_ context.Context, dirEntry timeline.DirEntry, _ timeline.RecognizeParams) (timeline.Recognition, error) {
	if strings.ToLower(path.Ext(dirEntry.Filename)) != ".csv" {
		return timeline.Recognition{}, nil
	}

	file, err := dirEntry.FS.Open(dirEntry.Filename)
	if err != nil {
		return timeline.Recognition{}, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	headers, err := reader.Read()
	if err != nil {
		if err == io.EOF {
			return timeline.Recognition{}, nil
		}
		return timeline.Recognition{}, fmt.Errorf("reading Yamtrack/Floppy CSV headers: %w", err)
	}

	fields := headerSet(headers)
	if fields["media_id"] && fields["source"] && fields["media_type"] && fields["title"] {
		return timeline.Recognition{Confidence: 1}, nil
	}
	return timeline.Recognition{}, nil
}

// FileImport imports tracked media rows. The export contains the current
// tracking state rather than every watch event, so each row becomes one
// lightweight event at its most useful activity timestamp.
func (i *Importer) FileImport(ctx context.Context, dirEntry timeline.DirEntry, params timeline.ImportParams) error {
	dsOpt, ok := params.DataSourceOptions.(*Options)
	if !ok || dsOpt == nil {
		return fmt.Errorf("invalid Yamtrack/Floppy data source options")
	}

	file, err := dirEntry.FS.Open(dirEntry.Filename)
	if err != nil {
		return fmt.Errorf("opening Yamtrack/Floppy CSV: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return fmt.Errorf("reading Yamtrack/Floppy CSV headers: %w", err)
	}
	fields := headerIndexes(headers)
	for _, required := range []string{"media_id", "source", "media_type", "title"} {
		if _, ok := fields[required]; !ok {
			return fmt.Errorf("Yamtrack/Floppy CSV is missing required column %q", required)
		}
	}

	owner := timeline.Entity{ID: dsOpt.OwnerEntityID}
	rowNumber := 1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		rowNumber++
		if err != nil {
			return fmt.Errorf("reading Yamtrack/Floppy CSV row %d: %w", rowNumber, err)
		}

		row := rowValues(fields, record)
		rowType := strings.ToLower(strings.TrimSpace(row["row_type"]))
		if rowType != "" && rowType != "media" {
			continue
		}
		mediaType := strings.ToLower(strings.TrimSpace(row["media_type"]))
		if mediaType == "" {
			continue
		}

		source := strings.TrimSpace(row["source"])
		mediaID := strings.TrimSpace(row["media_id"])
		title := strings.TrimSpace(row["title"])
		if title == "" {
			title = mediaID
		}
		item := &timeline.Item{
			ID:                   stableID(source, mediaType, mediaID, row["season_number"], row["episode_number"], title, row["created_at"]),
			Classification:       timeline.ClassEvent,
			Timestamp:            rowTimestamp(mediaType, row),
			Owner:                owner,
			OriginalLocation:     fmt.Sprintf("%s:%s", source, mediaID),
			IntermediateLocation: dirEntry.Filename,
			Content: timeline.ItemData{
				Data:      timeline.StringData(title),
				MediaType: "text/plain",
			},
			Metadata: timeline.Metadata{
				"Source":             source,
				"Media ID":           mediaID,
				"Media type":         mediaType,
				"Library media type": strings.TrimSpace(row["library_media_type"]),
				"Season":             strings.TrimSpace(row["season_number"]),
				"Episode":            strings.TrimSpace(row["episode_number"]),
				"Score":              strings.TrimSpace(row["score"]),
				"Progress":           strings.TrimSpace(row["progress"]),
				"Status":             strings.TrimSpace(row["status"]),
				"Start date":         strings.TrimSpace(row["start_date"]),
				"End date":           strings.TrimSpace(row["end_date"]),
				"Progressed at":      strings.TrimSpace(row["progressed_at"]),
				"Created at":         strings.TrimSpace(row["created_at"]),
				"Notes":              strings.TrimSpace(row["notes"]),
				"Image URL":          strings.TrimSpace(row["image"]),
			},
		}
		item.Metadata.StringsToSpecificType()
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
	return nil
}

func headerSet(headers []string) map[string]bool {
	set := make(map[string]bool, len(headers))
	for _, header := range headers {
		set[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(header, "\ufeff")))] = true
	}
	return set
}

func headerIndexes(headers []string) map[string]int {
	indexes := make(map[string]int, len(headers))
	for index, header := range headers {
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(header, "\ufeff")))
		if name != "" {
			indexes[name] = index
		}
	}
	return indexes
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

func parseTimestamp(values ...string) time.Time {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}

func rowTimestamp(mediaType string, row map[string]string) time.Time {
	if mediaType == "episode" {
		return parseTimestamp(row["end_date"], row["progressed_at"], row["created_at"], row["start_date"])
	}
	return parseTimestamp(row["progressed_at"], row["end_date"], row["start_date"], row["created_at"])
}

func stableID(source, mediaType, mediaID, season, episode, title, createdAt string) string {
	if mediaID == "" {
		mediaID = title
	}
	id := fmt.Sprintf("yamtrack:%s:%s:%s/%s/%s",
		source, mediaType, mediaID, strings.TrimSpace(season), strings.TrimSpace(episode))
	if strings.TrimSpace(createdAt) != "" {
		id += "/" + strings.TrimSpace(createdAt)
	}
	return id
}
