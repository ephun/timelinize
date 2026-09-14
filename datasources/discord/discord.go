// Package discord imports read-only Discord exports.
package discord

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

const (
	DataSourceID    = "discord"
	DataSourceTitle = "Discord"
)

func init() {
	err := timeline.RegisterDataSource(timeline.DataSource{
		Name:            DataSourceID,
		Title:           DataSourceTitle,
		Icon:            "folder.svg",
		Description:     "Read-only Discord Chat Exporter CSV and official Discord data-package exports.",
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

// Importer imports Discord message exports without contacting Discord or
// downloading any attachments.
type Importer struct{}

// Recognize recognizes the stable columns emitted by Discord Chat Exporter or
// the Messages directory layout in Discord's official data package.
func (Importer) Recognize(_ context.Context, dirEntry timeline.DirEntry, _ timeline.RecognizeParams) (timeline.Recognition, error) {
	if dirEntry.IsDir() && dirEntry.FileExists("Messages/index.json") {
		return timeline.Recognition{Confidence: 1}, nil
	}
	if strings.EqualFold(path.Ext(dirEntry.Name()), ".csv") {
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
			return timeline.Recognition{}, fmt.Errorf("reading Discord CSV headers: %w", err)
		}
		if hasHeaders(headers, "authorid", "author", "date", "content") {
			return timeline.Recognition{Confidence: 1}, nil
		}
	}
	return timeline.Recognition{}, nil
}

// FileImport imports either a CSV file/directory or an official Discord data
// package rooted at Messages/index.json.
func (i *Importer) FileImport(ctx context.Context, dirEntry timeline.DirEntry, params timeline.ImportParams) error {
	dsOpt, ok := params.DataSourceOptions.(*Options)
	if !ok || dsOpt == nil {
		return fmt.Errorf("invalid Discord data source options")
	}
	if dirEntry.IsDir() && dirEntry.FileExists("Messages/index.json") {
		return i.importPackage(ctx, dirEntry, params, dsOpt)
	}
	if !dirEntry.IsDir() && strings.EqualFold(path.Ext(dirEntry.Name()), ".csv") {
		return i.importCSV(ctx, dirEntry.FS, dirEntry.Filename, dirEntry.Filename, params, dsOpt)
	}
	return fs.WalkDir(dirEntry.FS, dirEntry.Filename, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(path.Ext(d.Name()), ".csv") {
			return nil
		}
		return i.importCSV(ctx, dirEntry.FS, fpath, fpath, params, dsOpt)
	})
}

func (i *Importer) importCSV(ctx context.Context, fsys fs.FS, filename, location string, params timeline.ImportParams, dsOpt *Options) error {
	file, err := fsys.Open(filename)
	if err != nil {
		return fmt.Errorf("opening Discord CSV: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return fmt.Errorf("reading Discord CSV headers: %w", err)
	}
	fields := headerIndexes(headers)
	for _, required := range []string{"authorid", "author", "date", "content"} {
		if _, ok := fields[required]; !ok {
			return fmt.Errorf("Discord CSV is missing required column %q", required)
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
		rowNumber++
		if err != nil {
			return fmt.Errorf("reading Discord CSV row %d: %w", rowNumber, err)
		}
		row := rowValues(fields, record)
		when, err := parseTimestamp(row["date"])
		if err != nil {
			return fmt.Errorf("parsing Discord CSV row %d timestamp: %w", rowNumber, err)
		}
		authorID := strings.TrimSpace(row["authorid"])
		author := strings.TrimSpace(row["author"])
		owner := timeline.Entity{Name: author, ID: dsOpt.OwnerEntityID}
		if authorID != "" {
			owner.Attributes = []timeline.Attribute{{
				Name:        "discord_user_id",
				Value:       authorID,
				Identity:    true,
				Identifying: true,
			}}
		}
		item := &timeline.Item{
			ID:                   fmt.Sprintf("discord:csv:%s:%d", location, rowNumber),
			Classification:       timeline.ClassMessage,
			Timestamp:            when,
			Owner:                owner,
			OriginalLocation:     location,
			IntermediateLocation: filename,
			Content: timeline.ItemData{
				Data:      timeline.StringData(row["content"]),
				MediaType: "text/plain",
			},
			Metadata: timeline.Metadata{
				"Discord user ID": authorID,
				"Attachments":     strings.TrimSpace(row["attachments"]),
				"Reactions":       strings.TrimSpace(row["reactions"]),
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

type packageMessage struct {
	ID          int64  `json:"ID"`
	Timestamp   string `json:"Timestamp"`
	Contents    string `json:"Contents"`
	Attachments string `json:"Attachments"`
}

type packageChannel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Guild struct {
		Name string `json:"name"`
	} `json:"guild"`
}

func (i *Importer) importPackage(ctx context.Context, dirEntry timeline.DirEntry, params timeline.ImportParams, dsOpt *Options) error {
	return fs.WalkDir(dirEntry.FS, dirEntry.Filename, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "messages.json" {
			return nil
		}

		data, err := fs.ReadFile(dirEntry.FS, fpath)
		if err != nil {
			return fmt.Errorf("reading Discord messages file: %w", err)
		}
		var messages []packageMessage
		if err := json.Unmarshal(data, &messages); err != nil {
			return fmt.Errorf("decoding Discord messages file: %w", err)
		}
		var channel packageChannel
		channelPath := path.Join(path.Dir(fpath), "channel.json")
		if channelData, readErr := fs.ReadFile(dirEntry.FS, channelPath); readErr == nil {
			if err := json.Unmarshal(channelData, &channel); err != nil {
				return fmt.Errorf("decoding Discord channel file: %w", err)
			}
		}
		for index, message := range messages {
			if err := ctx.Err(); err != nil {
				return err
			}
			when, err := parseTimestamp(message.Timestamp)
			if err != nil {
				return fmt.Errorf("parsing Discord package message %d timestamp: %w", index+1, err)
			}
			messageID := fmt.Sprintf("%d", message.ID)
			item := &timeline.Item{
				ID:                   fmt.Sprintf("discord:package:%s", messageID),
				Classification:       timeline.ClassMessage,
				Timestamp:            when,
				OriginalLocation:     path.Join("Messages", channel.ID, messageID),
				IntermediateLocation: fpath,
				Content: timeline.ItemData{
					Data:      timeline.StringData(message.Contents),
					MediaType: "text/plain",
				},
				Metadata: timeline.Metadata{
					"Channel":      channel.Name,
					"Channel ID":   channel.ID,
					"Channel type": channel.Type,
					"Guild":        channel.Guild.Name,
					"Attachments":  message.Attachments,
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
		return nil
	})
}

func hasHeaders(headers []string, required ...string) bool {
	fields := headerIndexes(headers)
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
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

func parseTimestamp(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}
