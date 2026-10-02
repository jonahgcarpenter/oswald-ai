package memory

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type profileImage struct {
	ID        string `json:"id"`
	ImageID   string `json:"image_id"`
	Version   int    `json:"version"`
	Highwater int    `json:"highwater"`
	Parent    string `json:"parent"`
	Path      string `json:"image_path"`
	MIMEType  string `json:"mime_type"`
}

type profileImageRecord struct {
	key   string
	state profileExchangeState
}

func (s *ProfileStore) imageRecords(ctx context.Context, tx *sql.Tx, id string) ([]profileImageRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT v.key,v.value FROM messages m JOIN state_meta v ON v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) WHERE m.session_id=? AND m.role='assistant' AND json_array_length(v.value,'$.images')>0 ORDER BY m.id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []profileImageRecord
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		state, err := decodeProfileExchange(value)
		if err != nil {
			return nil, err
		}
		records = append(records, profileImageRecord{key, state})
	}
	return records, rows.Err()
}

func (s *ProfileStore) boundImages(ctx context.Context, tx *sql.Tx, id string) error {
	records, err := s.imageRecords(ctx, tx, id)
	if err != nil {
		return err
	}
	remaining := 8
	for _, record := range records {
		keep := len(record.state.Images)
		if keep > remaining {
			keep = remaining
		}
		remaining -= keep
		if keep != len(record.state.Images) {
			record.state.Images = record.state.Images[len(record.state.Images)-keep:]
			encoded, err := json.Marshal(record.state)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=? WHERE key=?`, string(encoded), record.key); err != nil {
				return err
			}
		}
	}
	return nil
}

// SessionImages loads at most eight delivered image references. Missing or
// expired cache files are unavailable; SQLite no longer reconstructs image bytes.
func (s *ProfileStore) SessionImages(ctx context.Context, owner, key string, generation int) ([]requestctx.InputImage, error) {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return nil, err
	}
	records, err := s.imageRecords(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	highwater := map[string]int{}
	for _, record := range records {
		for _, image := range record.state.Images {
			if image.Highwater > highwater[image.ImageID] {
				highwater[image.ImageID] = image.Highwater
			}
		}
	}
	var images []requestctx.InputImage
	for _, record := range records {
		if record.state.Delivery != "delivered" {
			continue
		}
		for index := len(record.state.Images) - 1; index >= 0; index-- {
			image := record.state.Images[index]
			data, mime, err := s.cache.Resolve(ctx, owner, image.Path)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			images = append(images, requestctx.InputImage{ID: image.ID, Path: image.Path, ImageID: image.ImageID, Version: image.Version, VersionHighwater: highwater[image.ImageID], ParentSourceImageID: image.Parent, MIMEType: mime, Data: base64.StdEncoding.EncodeToString(data)})
		}
	}
	return images, tx.Commit()
}

// ReserveImageVersion advances the high-water on retained assets under the
// active generation fence. For a new image, the request keeps its initial counter
// until its first selected asset is persisted; foreground work is serialized.
func (s *ProfileStore) ReserveImageVersion(ctx context.Context, owner, key string, generation int, imageID string, minimum int) (int, error) {
	if imageID == "" || len(imageID) > 64 || minimum < 0 || minimum == int(^uint(0)>>1) {
		return 0, errors.New("invalid profile image version")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return 0, err
	}
	records, err := s.imageRecords(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	maximum := minimum
	for _, record := range records {
		for _, image := range record.state.Images {
			if image.ImageID == imageID && image.Highwater > maximum {
				maximum = image.Highwater
			}
		}
	}
	if maximum == int(^uint(0)>>1) {
		return 0, errors.New("profile image version exhausted")
	}
	maximum++
	for _, record := range records {
		changed := false
		for index := range record.state.Images {
			if record.state.Images[index].ImageID == imageID {
				record.state.Images[index].Highwater = maximum
				changed = true
			}
		}
		if changed {
			encoded, err := json.Marshal(record.state)
			if err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=? WHERE key=?`, string(encoded), record.key); err != nil {
				return 0, err
			}
		}
	}
	return maximum, tx.Commit()
}
