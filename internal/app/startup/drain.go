package startup

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway"
)

// restartDrainTimeout bounds one graceful restart drain: in-flight turns run
// to completion, then the server exits even if a turn is still running. A
// second signal aborts the wait immediately; SIGKILL (`oswald restart
// --force`) skips the drain entirely.
const restartDrainTimeout = 5 * time.Minute

// restartOutboundDrainTimeout bounds the Discord final-delivery drain that
// runs after the broker goes idle. Delivery leaves the broker before the
// gateway sends it, so broker idle alone can precede the last Discord edit;
// this waits that edit out. A second signal aborts the wait immediately.
const restartOutboundDrainTimeout = 2 * time.Minute

// drainSignalChannel subscribes to process shutdown signals. Tests inject a
// channel through seams; production (nil) gets the real signal.Notify
// subscription, and the returned stop func releases it.
func drainSignalChannel(seams profileDependencies) (<-chan os.Signal, func()) {
	if seams.drainSignals != nil {
		return seams.drainSignals, func() {}
	}
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

// outboundDrainer is implemented by gateways with async final delivery
// (Discord's FIFO outbound worker) that must drain before StopOutbound.
// iMessage sends its final reply synchronously, so it needs no wait.
type outboundDrainer interface {
	WaitOutboundIdle(time.Duration) bool
}

// awaitShutdown blocks until the parent context ends or a shutdown signal
// arrives, then coordinates exit. The first SIGTERM is a restart drain: cron
// tickers stop (no new scheduled work, and drain rejections never count
// toward a job's failure streak), the broker stops admission with the retry
// notice, and in-flight turns run to the drain timeout. Discord final
// delivery leaves the broker before the gateway sends it, so the drain then
// waits for the outbound worker before returning; the deferred cleanup may
// proceed to StopOutbound without canceling the last reply. SIGINT, a second
// signal, or parent cancellation skips the drain: the deferred cleanup runs
// the existing hard shutdown, which cancels leftovers.
func awaitShutdown(ctx context.Context, log *config.Logger, requestBroker *broker.Broker, gateways []gateway.Service, cronTickers []*cron.Ticker, sigCh <-chan os.Signal) {
	select {
	case <-ctx.Done():
		return
	case sig := <-sigCh:
		if sig != syscall.SIGTERM {
			return
		}
	}
	for _, ticker := range cronTickers {
		ticker.Stop()
	}
	requestBroker.BeginDrain()
	log.Debug("app.restart.draining", "draining in-flight turns before restart")
	idle := make(chan bool, 1)
	go func() { idle <- requestBroker.WaitIdle(restartDrainTimeout) }()
	select {
	case <-ctx.Done():
		return
	case <-sigCh:
		// Second signal aborts the drain; deferred cleanup cancels leftovers.
		return
	case <-idle:
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for _, service := range gateways {
			drainer, ok := service.(outboundDrainer)
			if !ok {
				continue
			}
			if !drainer.WaitOutboundIdle(restartOutboundDrainTimeout) {
				log.Warn("app.restart.outbound_timeout", "discord outbound drain timed out with delivery still queued", config.F("status", "degraded"))
			}
		}
	}()
	select {
	case <-ctx.Done():
		return
	case <-sigCh:
		// Second signal aborts the drain; deferred cleanup cancels leftovers.
		return
	case <-drained:
		return
	}
}
