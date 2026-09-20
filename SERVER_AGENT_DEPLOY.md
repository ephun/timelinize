# Server agent deployment instructions

Deploy the Timelinize fork from branch `feat/yamtrack-floppy-koito-sources`. This branch contains the existing Discord, Google Takeout/YouTube, Yamtrack/Floppy, and Koito work plus the new Immich importer.

## What changed

- Registered a new `immich` file datasource and added its import options to the web UI.
- The importer consumes a read-only Immich PostgreSQL text backup (`.sql` or `.sql.gz`) together with the matching original files from `UPLOAD_LOCATION`.
- It supports current singular Immich tables and older plural table names.
- It preserves the Immich asset UUID, original path/name, capture and modification times, GPS coordinates, camera/lens/exposure EXIF, dimensions, color data, descriptions, ratings, favorites, archive/trash/offline/edit state, tags, stacks, duplicate groups, albums, recognized people and face boxes, and live-photo video relationships.
- Embedded EXIF/XMP is read from the original first. Immich database metadata fills missing fields and adds metadata that exists only in Immich.
- Trashed assets are skipped unless the user selects `Import trashed items`; archived assets are imported and marked as archived.
- Added proper Immich, Discord, YouTube, and Gmail icons. Yamtrack and Koito already had branded icons.
- Fixed Yamtrack stable IDs so they match the tested `yamtrack:<source>:<type>:<id>/<season>/<episode>` format.

## Build and deploy

1. Preserve the current Timelinize config, repository/data volume, image tag, and compose file so rollback is possible.
2. Fetch the fork and check out `feat/yamtrack-floppy-koito-sources`.
3. Build using the repository Dockerfile; it installs the required CGO, SQLite, and libvips dependencies:

   ```sh
   docker build --pull -t timelinize:ephun-immich .
   ```

4. Add a read-only mount of the Immich `UPLOAD_LOCATION` root to the Timelinize service. The mounted root must include `backups/` and at least one of `library/` or `upload/`:

   ```yaml
   services:
     timelinize:
       image: timelinize:ephun-immich
       volumes:
         - /actual/immich/upload-location:/imports/immich:ro
   ```

   Do not mount or copy `DB_DATA_LOCATION`; the importer reads Immich's portable automatic dump, not live PostgreSQL files.

5. Recreate only the Timelinize service, preserving all existing Timelinize volume mappings and environment variables:

   ```sh
   docker compose up -d --no-deps --force-recreate timelinize
   docker compose logs --tail=100 timelinize
   ```

6. Confirm the Timelinize UI loads and the datasource list contains branded entries for Immich, Discord, Google Takeout Activity/YouTube, Yamtrack/Floppy, Koito, and Gmail.

## Prepare and import Immich

1. In Immich, create a fresh database backup from **Administration > Job Queues > Create Database Dump**. Wait until the `.sql.gz` file appears under `UPLOAD_LOCATION/backups`.
2. For the most consistent snapshot, prevent uploads/metadata edits while the dump and media snapshot are being made. A filesystem snapshot or copy of the complete `UPLOAD_LOCATION` is preferable for a large library.
3. In Timelinize, start a file import and select `/imports/immich` as the input directory. It should be recognized as **Immich**.
4. Select the Timelinize owner entity, choose whether to include trashed assets, and start the import. Test against a new or backed-up Timelinize repository before importing into the production timeline.
5. Watch the job log. Missing originals are skipped with their Immich asset ID and original path; the job fails rather than silently succeeding if no database assets can be matched to files.

Immich external-library paths are resolved when their original absolute path is reproduced beneath the selected import root. For example, an Immich path `/mnt/photos/a.jpg` should be available as `/imports/immich/mnt/photos/a.jpg` (a read-only bind mount or symlink is suitable).

## Verification performed during development

The following changed and related packages pass:

```sh
go test ./datasources/immich ./datasources/discord ./datasources/youtube ./datasources/yamtrack ./datasources/koito ./datasources/googlephotos ./datasources/applephotos
```

The repository-wide `go test ./...` run passed all packages except the pre-existing Windows-only Firefox recognition cleanup tests, which fail because temporary SQLite files remain open during `TempDir` cleanup. The production Docker/Linux build is the authoritative deployment build.

## Rollback

If startup or smoke checks fail, restore the previous Timelinize image reference and recreate only the Timelinize service. Do not alter Immich: the new integration is read-only and requires no Immich migration or API key.
