package memory

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestProfileImagesUseCacheAndRespectDeliveryAndVersions(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: key, Generation: 1, UserText: "synthetic image request", AssistantText: "synthetic image answer", Pressure: SessionPromptPressure{Tokens: 10, Limit: 100, Version: "v1"}, Images: []requestctx.InputImage{{ID: "asset", ImageID: "logical", Version: 1, VersionHighwater: 1, MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(data.Bytes())}}})
	if err != nil {
		t.Fatal(err)
	}
	images, err := s.SessionImages(ctx, "alice", key, 1)
	if err != nil || len(images) != 0 {
		t.Fatal("pending images exposed", err)
	}
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	version, err := s.ReserveImageVersion(ctx, "alice", key, 1, "logical", 1)
	if err != nil || version != 2 {
		t.Fatal("version reservation failed", err)
	}
	images, err = s.SessionImages(ctx, "alice", key, 1)
	if err != nil || len(images) != 1 || images[0].VersionHighwater != 2 || images[0].Data == "" {
		t.Fatal("delivered cache image unavailable", err)
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.activeSession(ctx, tx, "alice", key, 1)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	records, err := s.imageRecords(ctx, tx, id)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(records[0].state.Images[0].Path); err != nil {
		t.Fatal(err)
	}
	images, err = s.SessionImages(ctx, "alice", key, 1)
	if err != nil || len(images) != 0 {
		t.Fatal("missing cache reconstructed from SQLite", err)
	}
}
