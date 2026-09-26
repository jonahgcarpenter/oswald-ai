package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func TestRebuildFailureDoesNotInventCoverage(t *testing.T) {
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	s := NewService(nil, nil, nil, "", log)
	s.health("index.rebuild.failed", memory.DerivedIndexRevision{Kind: memory.IndexKindMemoryFTS, Revision: 2}, 0, 0, "degraded", time.Second, errors.New("private error sentinel"))
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["level"] != "warn" || record["phase"] == nil || record["coverage"] != nil || record["expected_count"] != nil || strings.Contains(output.String(), "private error sentinel") {
		t.Fatalf("failure record=%s", output.String())
	}
	output.Reset()
	s.health("index.rebuild.complete", memory.DerivedIndexRevision{Kind: memory.IndexKindMemoryFTS, Revision: 2}, 4, 4, "ok", time.Second, nil)
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["level"] != "info" || record["coverage"] != float64(1) || record["expected_count"] != float64(4) {
		t.Fatalf("success record=%s", output.String())
	}
}

func TestSnapshotUnavailableDoesNotEmitZeroJobGauges(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	NewService(store, nil, nil, "", log).snapshot(context.Background())
	if !strings.Contains(output.String(), `"event":"memory.health.failed"`) || strings.Contains(output.String(), `"event":"memory.jobs.health"`) || strings.Contains(output.String(), `"queued_count":0`) {
		t.Fatalf("unavailable gauges: %s", output.String())
	}
	if !strings.Contains(output.String(), `"is_last_maintenance_known":false`) || strings.Contains(output.String(), "last_maintenance_age_ms") {
		t.Fatalf("unknown maintenance timestamp: %s", output.String())
	}
	if !strings.Contains(output.String(), `"record_kind":"snapshot"`) {
		t.Fatalf("missing snapshot tag: %s", output.String())
	}
}
