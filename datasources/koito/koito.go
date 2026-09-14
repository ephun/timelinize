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

// Package koito imports Koito's versioned listening-history export.
package koito

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

const (
	DataSourceID    = "koito"
	DataSourceTitle = "Koito"
)

func init() {
	err := timeline.RegisterDataSource(timeline.DataSource{
		Name:            DataSourceID,
		Title:           DataSourceTitle,
		Icon:            "koito.svg",
		Description:     "A Koito JSON export of listening history.",
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

// Importer imports Koito's koito_export.json format.
type Importer struct{}

type exportFile struct {
	Version    string        `json:"version"`
	ExportedAt time.Time     `json:"exported_at"`
	User       string        `json:"user"`
	Listens    []listenEntry `json:"listens"`
}

type listenEntry struct {
	ListenedAt time.Time     `json:"listened_at"`
	Client     string        `json:"client"`
	Track      trackEntry    `json:"track"`
	Album      albumEntry    `json:"album"`
	Artists    []artistEntry `json:"artists"`
}

type alias struct {
	Name      string `json:"alias"`
	Source    string `json:"source"`
	IsPrimary bool   `json:"is_primary"`
}

type trackEntry struct {
	MBID     *string `json:"mbid"`
	Duration int     `json:"duration"`
	Aliases  []alias `json:"aliases"`
}

type albumEntry struct {
	ImageURL       string  `json:"image_url"`
	MBID           *string `json:"mbid"`
	Aliases        []alias `json:"aliases"`
	VariousArtists bool    `json:"various_artists"`
}

type artistEntry struct {
	ImageURL  string  `json:"image_url"`
	MBID      *string `json:"mbid"`
	IsPrimary bool    `json:"is_primary"`
	Aliases   []alias `json:"aliases"`
}

// Recognize returns whether the input is a Koito export file.
func (Importer) Recognize(_ context.Context, dirEntry timeline.DirEntry, _ timeline.RecognizeParams) (timeline.Recognition, error) {
	name := strings.ToLower(path.Base(dirEntry.Filename))
	if path.Ext(name) != ".json" || !strings.Contains(name, "koito") {
		return timeline.Recognition{}, nil
	}

	file, err := dirEntry.FS.Open(dirEntry.Filename)
	if err != nil {
		return timeline.Recognition{}, err
	}
	defer file.Close()
	var header struct {
		Version string          `json:"version"`
		Listens json.RawMessage `json:"listens"`
	}
	if err := json.NewDecoder(file).Decode(&header); err != nil {
		return timeline.Recognition{}, nil
	}
	if header.Version == "1" && header.Listens != nil {
		return timeline.Recognition{Confidence: 1}, nil
	}
	return timeline.Recognition{}, nil
}

// FileImport imports each Koito listen as a lightweight media event. Artwork
// URLs and MusicBrainz IDs are retained as metadata; no artwork is downloaded.
func (i *Importer) FileImport(ctx context.Context, dirEntry timeline.DirEntry, params timeline.ImportParams) error {
	dsOpt, ok := params.DataSourceOptions.(*Options)
	if !ok || dsOpt == nil {
		return fmt.Errorf("invalid Koito data source options")
	}

	data, err := fs.ReadFile(dirEntry.FS, dirEntry.Filename)
	if err != nil {
		return fmt.Errorf("reading Koito export: %w", err)
	}
	var export exportFile
	if err := json.Unmarshal(data, &export); err != nil {
		return fmt.Errorf("decoding Koito export: %w", err)
	}
	if export.Version != "1" {
		return fmt.Errorf("unsupported Koito export version %q", export.Version)
	}

	owner := timeline.Entity{ID: dsOpt.OwnerEntityID}
	for index, listen := range export.Listens {
		if err := ctx.Err(); err != nil {
			return err
		}
		if listen.ListenedAt.IsZero() {
			return fmt.Errorf("Koito listen %d has no listened_at timestamp", index+1)
		}

		trackName := primaryAlias(listen.Track.Aliases)
		albumName := primaryAlias(listen.Album.Aliases)
		artistNames := make([]string, 0, len(listen.Artists))
		artistIDs := make([]string, 0, len(listen.Artists))
		for _, artist := range listen.Artists {
			if name := primaryAlias(artist.Aliases); name != "" {
				artistNames = append(artistNames, name)
			}
			if artist.MBID != nil && strings.TrimSpace(*artist.MBID) != "" {
				artistIDs = append(artistIDs, strings.TrimSpace(*artist.MBID))
			}
		}
		artists := strings.Join(artistNames, ", ")
		content := trackName
		if artists != "" {
			content = artists + " — " + trackName
		}
		if albumName != "" {
			content += " [" + albumName + "]"
		}
		if content == "" {
			content = "Koito listen"
		}

		item := &timeline.Item{
			ID:                   listenID(export.User, listen, artists, trackName, albumName),
			Classification:       timeline.ClassMedia,
			Timestamp:            listen.ListenedAt,
			Owner:                owner,
			OriginalLocation:     "koito:" + export.User,
			IntermediateLocation: dirEntry.Filename,
			Content: timeline.ItemData{
				Data:      timeline.StringData(content),
				MediaType: "text/plain",
			},
			Metadata: timeline.Metadata{
				"Koito user":             export.User,
				"Client":                 listen.Client,
				"Artist":                 artists,
				"Track":                  trackName,
				"Album":                  albumName,
				"Track MusicBrainz ID":   pointerValue(listen.Track.MBID),
				"Album MusicBrainz ID":   pointerValue(listen.Album.MBID),
				"Artist MusicBrainz IDs": artistIDs,
				"Track aliases":          aliasNames(listen.Track.Aliases),
				"Album aliases":          aliasNames(listen.Album.Aliases),
				"Duration seconds":       listen.Track.Duration,
				"Album image URL":        listen.Album.ImageURL,
				"Various artists":        listen.Album.VariousArtists,
			},
		}
		if listen.Track.Duration > 0 {
			item.Timespan = listen.ListenedAt.Add(time.Duration(listen.Track.Duration) * time.Second)
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
}

func primaryAlias(aliases []alias) string {
	for _, item := range aliases {
		if item.IsPrimary && strings.TrimSpace(item.Name) != "" {
			return strings.TrimSpace(item.Name)
		}
	}
	for _, item := range aliases {
		if strings.TrimSpace(item.Name) != "" {
			return strings.TrimSpace(item.Name)
		}
	}
	return ""
}

func aliasNames(aliases []alias) []string {
	ret := make([]string, 0, len(aliases))
	for _, item := range aliases {
		if name := strings.TrimSpace(item.Name); name != "" {
			ret = append(ret, name)
		}
	}
	return ret
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func listenID(user string, listen listenEntry, artists, track, album string) string {
	key := strings.Join([]string{
		user,
		listen.ListenedAt.UTC().Format(time.RFC3339Nano),
		listen.Client,
		artists,
		track,
		album,
	}, "\x1f")
	hash := sha256.Sum256([]byte(key))
	return "koito:" + hex.EncodeToString(hash[:])
}
