package imessage

import (
	"net"
	"net/http"
	"regexp"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// Name returns the human-readable gateway name.
func (g *Gateway) Name() string {
	return "iMessage"
}

// Start initializes the BlueBubbles webhook listener.
func (g *Gateway) Start(b *broker.Broker) error {
	g.Broker = b
	log := g.log()
	if g.messageIndex == nil {
		g.messageIndex = make(map[string]messageContext)
	}
	if g.contactNames == nil {
		g.contactNames = make(map[string]contactNameCacheEntry)
	}
	if g.chatNames == nil {
		g.chatNames = make(map[string]chatNameCacheEntry)
	}
	privateAPIAvailable := g.refreshBlueBubblesCapabilitiesWithRetry(capabilityAttempts, capabilityRetryDelay)

	mux := http.NewServeMux()
	mux.HandleFunc(g.listenPath(), g.handleWebhook)

	listener, err := net.Listen("tcp", ":"+g.Port)
	if err != nil {
		return err
	}
	log.Debug("gateway.listen", "imessage gateway listener starting", config.F("port", g.Port))
	listenAddr := listener.Addr().String()
	if host, _, splitErr := net.SplitHostPort(listenAddr); splitErr == nil {
		listenAddr = host
	}
	privateAPI := "disabled"
	if privateAPIAvailable {
		privateAPI = "enabled"
	}
	log.Info("gateway.connected", "bluebubbles gateway ready",
		config.F("gateway", "imessage"),
		config.F("private_api", privateAPI),
		config.F("webhook_path", g.listenPath()),
		config.F("listen_addr", listenAddr),
		config.F("port", g.Port))
	return http.Serve(listener, mux)
}

// listenPath returns the configured webhook route, falling back to the built-in
// default for directly constructed gateways.
func (g *Gateway) listenPath() string {
	if g.WebhookPath != "" {
		return g.WebhookPath
	}
	return defaultWebhookPath
}

func (g *Gateway) runtimeDependencies() gatewayruntime.Dependencies {
	deps := g.Runtime
	deps.Broker = g.Broker
	if deps.Log == nil {
		deps.Log = g.Log
	}
	return deps
}

// Gateway receives BlueBubbles webhooks and sends replies via its REST API.
type Gateway struct {
	Port                string
	BlueBubblesURL      string
	BlueBubblesPassword string
	WebhookPath         string
	MentionPatterns     []*regexp.Regexp
	Links               identity.Resolver
	Runtime             gatewayruntime.Dependencies
	Log                 *config.Logger
	Broker              *broker.Broker
	HTTPClient          *http.Client
	capabilityMu        sync.Mutex
	capabilitiesLoaded  bool
	privateAPIEnabled   bool
	helperConnected     bool
	messageMu           sync.RWMutex
	messageIndex        map[string]messageContext
	contactMu           sync.RWMutex
	contactNames        map[string]contactNameCacheEntry
	chatNameMu          sync.RWMutex
	chatNames           map[string]chatNameCacheEntry
}

func (g *Gateway) log(scoped ...*config.Logger) *config.Logger {
	if len(scoped) > 0 && scoped[0] != nil {
		return scoped[0]
	}
	return g.Log.Server("gateway.imessage", config.F("gateway", "imessage"))
}
