package comfy_ui

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func generationStageLogger(ctx context.Context, log *config.Logger, strength *float64) func(string, time.Time, error) {
	if log == nil {
		return func(string, time.Time, error) {}
	}
	if strength != nil {
		log = log.With(config.F("strength", *strength))
	}
	meta := requestctx.MetadataFromContext(ctx)
	parent := meta.OperationID
	if parent == "" {
		parent = meta.ParentOperationID
	}
	log = log.Server("provider.comfyui", requestctx.LogFields(ctx)...).With(config.F("operation_id", rand.Text()), config.F("parent_operation_id", parent))
	return func(phase string, started time.Time, err error) {
		status, outcome := "ok", "ok"
		if err != nil {
			status, outcome = "error", "error"
			if errors.Is(err, context.Canceled) || (phase != "cleanup" && errors.Is(ctx.Err(), context.Canceled)) {
				status, outcome = "ok", "canceled"
				err = context.Canceled
			}
		}
		fields := []config.Field{config.F("record_kind", "measurement"), config.F("operation", "generate"), config.F("phase", phase), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", status), config.F("outcome", outcome)}
		if err != nil {
			fields = append(fields, config.ErrorField(err))
		}
		if code := config.HTTPStatus(err); code != 0 {
			fields = append(fields, config.F("http_status", code))
		}
		log.Info("provider.comfyui.stage.complete", "ComfyUI stage completed", fields...)
	}
}
