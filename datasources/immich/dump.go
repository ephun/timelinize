/*
	Timelinize
	Copyright (c) 2013 Matthew Holt

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published
	by the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.
*/

package immich

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Immich's automatic backups are PostgreSQL text dumps. Keeping this parser
// deliberately limited to COPY statements makes it independent of a local
// PostgreSQL installation while still handling the portable format emitted by
// pg_dump. Unknown tables and columns are ignored for forward compatibility.
type databaseDump struct {
	assets      map[string]assetRecord
	exif        map[string]exifRecord
	albums      map[string]albumRecord
	assetAlbums map[string][]string
	tags        map[string]tagRecord
	assetTags   map[string][]string
	people      map[string]personRecord
	assetFaces  map[string][]faceRecord
}

type assetRecord struct {
	ID                string
	OwnerID           string
	Type              string
	OriginalPath      string
	OriginalFileName  string
	FileCreatedAt     string
	FileModifiedAt    string
	LocalDateTime     string
	CreatedAt         string
	UpdatedAt         string
	Duration          string
	LivePhotoVideoID  string
	StackID           string
	DuplicateID       string
	Visibility        string
	Status            string
	Width             string
	Height            string
	ChecksumAlgorithm string
	IsFavorite        bool
	IsArchived        bool
	IsOffline         bool
	IsExternal        bool
	IsEdited          bool
	DeletedAt         string
}

type exifRecord struct {
	AssetID            string
	Make               string
	Model              string
	ExifImageWidth     string
	ExifImageHeight    string
	FileSizeInByte     string
	Orientation        string
	DateTimeOriginal   string
	ModifyDate         string
	LensModel          string
	FNumber            string
	FocalLength        string
	ISO                string
	Latitude           string
	Longitude          string
	City               string
	State              string
	Country            string
	Description        string
	FPS                string
	ExposureTime       string
	LivePhotoCID       string
	TimeZone           string
	ProjectionType     string
	ProfileDescription string
	Colorspace         string
	BitsPerSample      string
	AutoStackID        string
	Rating             string
	Tags               string
	LockedProperties   string
	UpdatedAt          string
}

type albumRecord struct {
	ID          string
	Name        string
	Description string
	CreatedAt   string
	UpdatedAt   string
	DeletedAt   string
}

type tagRecord struct {
	ID       string
	Value    string
	Color    string
	ParentID string
}

type personRecord struct {
	ID         string
	Name       string
	BirthDate  string
	IsFavorite bool
	IsHidden   bool
	Color      string
}

type faceRecord struct {
	PersonID      string
	ImageWidth    string
	ImageHeight   string
	BoundingBoxX1 string
	BoundingBoxY1 string
	BoundingBoxX2 string
	BoundingBoxY2 string
	SourceType    string
	DeletedAt     string
	IsVisible     bool
}

func newDatabaseDump() *databaseDump {
	return &databaseDump{
		assets:      make(map[string]assetRecord),
		exif:        make(map[string]exifRecord),
		albums:      make(map[string]albumRecord),
		assetAlbums: make(map[string][]string),
		tags:        make(map[string]tagRecord),
		assetTags:   make(map[string][]string),
		people:      make(map[string]personRecord),
		assetFaces:  make(map[string][]faceRecord),
	}
}

func parseDatabaseDump(input io.Reader) (*databaseDump, error) {
	dump := newDatabaseDump()
	reader := bufio.NewReaderSize(input, 128*1024)

	var table string
	var columns []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("reading Immich database dump: %w", err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")

		if table == "" {
			table, columns = parseCopyHeader(line)
		} else if line == `\.` {
			table = ""
			columns = nil
		} else {
			values, decodeErr := decodeCopyRow(line)
			if decodeErr != nil {
				return nil, fmt.Errorf("decoding COPY row for table %s: %w", table, decodeErr)
			}
			if len(values) != len(columns) {
				return nil, fmt.Errorf("COPY row for table %s has %d values, expected %d", table, len(values), len(columns))
			}
			row := make(copyRow, len(columns))
			for i, column := range columns {
				row[strings.ToLower(column)] = values[i]
			}
			dump.addRow(table, row)
		}

		if errors.Is(err, io.EOF) {
			break
		}
	}

	if len(dump.assets) == 0 {
		return nil, errors.New("Immich database dump contains no assets")
	}
	return dump, nil
}

func parseCopyHeader(line string) (string, []string) {
	if !strings.HasPrefix(line, "COPY ") || !strings.HasSuffix(line, " FROM stdin;") {
		return "", nil
	}
	open := strings.Index(line, " (")
	close := strings.LastIndex(line, ") FROM stdin;")
	if open < 0 || close <= open+2 {
		return "", nil
	}

	tableName := strings.TrimSpace(line[len("COPY "):open])
	if dot := strings.LastIndex(tableName, "."); dot >= 0 {
		tableName = tableName[dot+1:]
	}
	tableName = strings.ToLower(strings.Trim(tableName, `"`))
	if !supportedTable(tableName) {
		return "", nil
	}

	rawColumns := strings.Split(line[open+2:close], ",")
	columns := make([]string, 0, len(rawColumns))
	for _, column := range rawColumns {
		columns = append(columns, strings.Trim(strings.TrimSpace(column), `"`))
	}
	return tableName, columns
}

func supportedTable(table string) bool {
	switch table {
	case "asset", "assets", "asset_exif", "exif", "album", "albums",
		"album_asset", "albums_assets_assets", "tag", "tags", "tag_asset",
		"tags_assets_assets", "person", "people", "asset_face", "asset_faces":
		return true
	default:
		return false
	}
}

// decodeCopyRow implements PostgreSQL's text COPY escaping, including the null
// sentinel. Empty strings and NULL both become empty here because the importer
// only needs value presence, not the distinction between those two states.
func decodeCopyRow(line string) ([]string, error) {
	rawValues := strings.Split(line, "\t")
	values := make([]string, len(rawValues))
	for i, raw := range rawValues {
		if raw == `\N` {
			continue
		}
		var value strings.Builder
		value.Grow(len(raw))
		for pos := 0; pos < len(raw); pos++ {
			if raw[pos] != '\\' {
				value.WriteByte(raw[pos])
				continue
			}
			pos++
			if pos >= len(raw) {
				return nil, errors.New("trailing COPY escape")
			}
			switch raw[pos] {
			case 'b':
				value.WriteByte('\b')
			case 'f':
				value.WriteByte('\f')
			case 'n':
				value.WriteByte('\n')
			case 'r':
				value.WriteByte('\r')
			case 't':
				value.WriteByte('\t')
			case 'v':
				value.WriteByte('\v')
			case '\\':
				value.WriteByte('\\')
			case 'x':
				if pos+2 >= len(raw) {
					return nil, errors.New("short hexadecimal COPY escape")
				}
				n, parseErr := strconv.ParseUint(raw[pos+1:pos+3], 16, 8)
				if parseErr != nil {
					return nil, fmt.Errorf("invalid hexadecimal COPY escape: %w", parseErr)
				}
				value.WriteByte(byte(n))
				pos += 2
			default:
				if raw[pos] >= '0' && raw[pos] <= '7' {
					end := pos + 1
					for end < len(raw) && end < pos+3 && raw[end] >= '0' && raw[end] <= '7' {
						end++
					}
					n, parseErr := strconv.ParseUint(raw[pos:end], 8, 8)
					if parseErr != nil {
						return nil, fmt.Errorf("invalid octal COPY escape: %w", parseErr)
					}
					value.WriteByte(byte(n))
					pos = end - 1
				} else {
					value.WriteByte(raw[pos])
				}
			}
		}
		values[i] = value.String()
	}
	return values, nil
}

type copyRow map[string]string

func (r copyRow) get(names ...string) string {
	for _, name := range names {
		if value, ok := r[strings.ToLower(name)]; ok {
			return value
		}
	}
	return ""
}

func (d *databaseDump) addRow(table string, row copyRow) {
	switch table {
	case "asset", "assets":
		record := assetRecord{
			ID:                row.get("id"),
			OwnerID:           row.get("ownerId", "owner_id"),
			Type:              row.get("type"),
			OriginalPath:      row.get("originalPath", "original_path"),
			OriginalFileName:  row.get("originalFileName", "original_file_name"),
			FileCreatedAt:     row.get("fileCreatedAt", "file_created_at"),
			FileModifiedAt:    row.get("fileModifiedAt", "file_modified_at"),
			LocalDateTime:     row.get("localDateTime", "local_date_time"),
			CreatedAt:         row.get("createdAt", "created_at"),
			UpdatedAt:         row.get("updatedAt", "updated_at"),
			Duration:          row.get("duration"),
			LivePhotoVideoID:  row.get("livePhotoVideoId", "live_photo_video_id"),
			StackID:           row.get("stackId", "stack_id"),
			DuplicateID:       row.get("duplicateId", "duplicate_id"),
			Visibility:        row.get("visibility"),
			Status:            row.get("status"),
			Width:             row.get("width"),
			Height:            row.get("height"),
			ChecksumAlgorithm: row.get("checksumAlgorithm", "checksum_algorithm"),
			IsFavorite:        parseBool(row.get("isFavorite", "is_favorite")),
			IsArchived:        parseBool(row.get("isArchived", "is_archived")),
			IsOffline:         parseBool(row.get("isOffline", "is_offline")),
			IsExternal:        parseBool(row.get("isExternal", "is_external")),
			IsEdited:          parseBool(row.get("isEdited", "is_edited")),
			DeletedAt:         row.get("deletedAt", "deleted_at"),
		}
		if record.ID != "" {
			d.assets[record.ID] = record
		}

	case "asset_exif", "exif":
		record := exifRecord{
			AssetID:            row.get("assetId", "asset_id"),
			Make:               row.get("make"),
			Model:              row.get("model"),
			ExifImageWidth:     row.get("exifImageWidth", "exif_image_width"),
			ExifImageHeight:    row.get("exifImageHeight", "exif_image_height"),
			FileSizeInByte:     row.get("fileSizeInByte", "file_size_in_byte"),
			Orientation:        row.get("orientation"),
			DateTimeOriginal:   row.get("dateTimeOriginal", "date_time_original"),
			ModifyDate:         row.get("modifyDate", "modify_date"),
			LensModel:          row.get("lensModel", "lens_model"),
			FNumber:            row.get("fNumber", "f_number"),
			FocalLength:        row.get("focalLength", "focal_length"),
			ISO:                row.get("iso"),
			Latitude:           row.get("latitude"),
			Longitude:          row.get("longitude"),
			City:               row.get("city"),
			State:              row.get("state"),
			Country:            row.get("country"),
			Description:        row.get("description"),
			FPS:                row.get("fps"),
			ExposureTime:       row.get("exposureTime", "exposure_time"),
			LivePhotoCID:       row.get("livePhotoCID", "live_photo_cid"),
			TimeZone:           row.get("timeZone", "time_zone"),
			ProjectionType:     row.get("projectionType", "projection_type"),
			ProfileDescription: row.get("profileDescription", "profile_description"),
			Colorspace:         row.get("colorspace", "colorSpace", "color_space"),
			BitsPerSample:      row.get("bitsPerSample", "bits_per_sample"),
			AutoStackID:        row.get("autoStackId", "auto_stack_id"),
			Rating:             row.get("rating"),
			Tags:               row.get("tags"),
			LockedProperties:   row.get("lockedProperties", "locked_properties"),
			UpdatedAt:          row.get("updatedAt", "updated_at"),
		}
		if record.AssetID != "" {
			d.exif[record.AssetID] = record
		}

	case "album", "albums":
		record := albumRecord{
			ID:          row.get("id"),
			Name:        row.get("albumName", "album_name"),
			Description: row.get("description"),
			CreatedAt:   row.get("createdAt", "created_at"),
			UpdatedAt:   row.get("updatedAt", "updated_at"),
			DeletedAt:   row.get("deletedAt", "deleted_at"),
		}
		if record.ID != "" {
			d.albums[record.ID] = record
		}

	case "album_asset", "albums_assets_assets":
		albumID := row.get("albumId", "albumsId", "album_id")
		assetID := row.get("assetId", "assetsId", "asset_id")
		if albumID != "" && assetID != "" {
			d.assetAlbums[assetID] = append(d.assetAlbums[assetID], albumID)
		}

	case "tag", "tags":
		record := tagRecord{
			ID:       row.get("id"),
			Value:    row.get("value", "name"),
			Color:    row.get("color"),
			ParentID: row.get("parentId", "parent_id"),
		}
		if record.ID != "" {
			d.tags[record.ID] = record
		}

	case "tag_asset", "tags_assets_assets":
		tagID := row.get("tagId", "tagsId", "tag_id")
		assetID := row.get("assetId", "assetsId", "asset_id")
		if tagID != "" && assetID != "" {
			d.assetTags[assetID] = append(d.assetTags[assetID], tagID)
		}

	case "person", "people":
		record := personRecord{
			ID:         row.get("personGroupId", "person_group_id", "id"),
			Name:       row.get("name"),
			BirthDate:  row.get("birthDate", "birth_date"),
			IsFavorite: parseBool(row.get("isFavorite", "is_favorite")),
			IsHidden:   parseBool(row.get("isHidden", "is_hidden")),
			Color:      row.get("color"),
		}
		if record.ID != "" {
			d.people[record.ID] = record
		}

	case "asset_face", "asset_faces":
		assetID := row.get("assetId", "asset_id")
		record := faceRecord{
			PersonID:      row.get("personGroupId", "person_group_id", "personId", "person_id"),
			ImageWidth:    row.get("imageWidth", "image_width"),
			ImageHeight:   row.get("imageHeight", "image_height"),
			BoundingBoxX1: row.get("boundingBoxX1", "bounding_box_x1"),
			BoundingBoxY1: row.get("boundingBoxY1", "bounding_box_y1"),
			BoundingBoxX2: row.get("boundingBoxX2", "bounding_box_x2"),
			BoundingBoxY2: row.get("boundingBoxY2", "bounding_box_y2"),
			SourceType:    row.get("sourceType", "source_type"),
			DeletedAt:     row.get("deletedAt", "deleted_at"),
			IsVisible:     !strings.EqualFold(row.get("isVisible", "is_visible"), "false"),
		}
		if assetID != "" && record.PersonID != "" {
			d.assetFaces[assetID] = append(d.assetFaces[assetID], record)
		}
	}
}

func parseBool(value string) bool {
	value = strings.TrimSpace(value)
	return value == "1" || strings.EqualFold(value, "t") || strings.EqualFold(value, "true")
}

func parseTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func parsePGArray(value string) []string {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '{' || value[len(value)-1] != '}' {
		return nil
	}
	value = value[1 : len(value)-1]
	if value == "" {
		return nil
	}

	var values []string
	var current strings.Builder
	quoted := false
	escaped := false
	for _, char := range value {
		switch {
		case escaped:
			current.WriteRune(char)
			escaped = false
		case char == '\\':
			escaped = true
		case char == '"':
			quoted = !quoted
		case char == ',' && !quoted:
			values = append(values, current.String())
			current.Reset()
		default:
			current.WriteRune(char)
		}
	}
	values = append(values, current.String())
	return values
}
