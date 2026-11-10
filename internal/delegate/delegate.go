// Package delegate owns subagent delegation: dispatching child turns with
// isolated context, tracking their lifecycle, and delivering results back
// into the originating conversation. Top-level dispatches always run
// asynchronously; nested orchestrator children run inline in the calling
// turn. Nothing here names any external agent system: the protocol is
// Oswald-native (broker lanes, per-profile state, exact-token leases).
package delegate

import (
	"context"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// BlockedTools are withheld from every delegated child turn, no matter the
// parent's catalog: no nested delegation by default (depth is fenced
// separately), no user interaction, no shared-memory writes, no
// parent-history reads, no scheduling in the parent's name, and no durable
// image generation (a child has no session to persist generated images
// into, so generation fails closed instead of erroring mid-turn).
var BlockedTools = []string{"delegate", "clarify", "memory", "session_search", "cronjob_manage", "image_generate"}

// Max delivery attempts before a handoff is parked as failed.
const maxDeliveryAttempts = 20

// Live-check and retention bounds. Stall thresholds mirror the inactivity
// windows: quiet children get 450s, children inside a tool call get 1200s,
// and an interrupted child gets a 120s grace window to unwind before it is
// force-finalized.
const (
	pollInterval       = 5 * time.Second
	stallCheckEvery    = 30 * time.Second
	pruneEvery         = time.Hour
	retentionDays      = 7
	maxRowsRetained    = 1000
	stalledIdleAfter   = 450 * time.Second
	stalledInToolAfter = 1200 * time.Second
	stalledGrace       = 120 * time.Second
)

// BrokerSubmitter is the slice of the conversation broker the manager needs:
// fire-and-forget delivery turns into the origin lane.
type BrokerSubmitter interface {
	Submit(req *broker.Request) error
}

type spawnContextKey struct{}

// Spawn carries the depth and role of the turn executing a delegate call.
// Depth 0 is a normal model turn; children run deeper.
type Spawn struct {
	Depth int
	Role  string
}

// WithSpawn attaches the current turn's delegation depth and role.
func WithSpawn(ctx context.Context, depth int, role string) context.Context {
	return context.WithValue(ctx, spawnContextKey{}, Spawn{Depth: depth, Role: role})
}

// SpawnFromContext reads the current turn's delegation depth and role.
// Turns outside any delegation report depth 0 with an empty role.
func SpawnFromContext(ctx context.Context) Spawn {
	if spawn, ok := ctx.Value(spawnContextKey{}).(Spawn); ok {
		return spawn
	}
	return Spawn{}
}

// Manager owns delegated subagent turns for one profile: dispatch, worker
// pool, stall monitoring, delivery, transcripts, and prune/recovery of the
// durable delegation ledger.
type Manager struct {
	profile     string
	profileRoot string
	log         *config.Logger

	store    *memory.ProfileStore
	broker   BrokerSubmitter
	procs    processLister
	registry *registry.Registry
	mcp      mcp.ProviderView
	soul     *soul.Store
	images   *imagecache.Cache
	window   int
	vkey     string
	cfg      *config.Config

	parentClient llm.Chatter
	newClient    func(url, key, virtualKey string, log *config.Logger) llm.Chatter
	clientsMu    sync.Mutex
	clients      map[string]llm.Chatter

	mu      sync.Mutex
	live    map[string]*liveChild
	started time.Time
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
	pool    chan struct{}
}

// processLister is the subset of the tracked-process manager the delegation
// manager needs. *process.Manager satisfies it; tests substitute fakes.
type processLister interface {
	List() ([]process.Info, error)
	Kill(id string) (process.Info, error)
	Tail(id string, n int) (string, error)
}

// Deps wires a per-profile Manager. Everything is provided by the composition
// root; the manager owns no stores itself.
type Deps struct {
	Profile     string
	ProfileRoot string
	Log         *config.Logger
	Store       *memory.ProfileStore
	Broker      BrokerSubmitter
	Processes   processLister
	Parent      llm.Chatter
	NewClient   func(url, key, virtualKey string, log *config.Logger) llm.Chatter
	Registry    *registry.Registry
	MCP         mcp.ProviderView
	Soul        *soul.Store
	Images      *imagecache.Cache
	Window      int
	VirtualKey  string
	Config      *config.Config
}

// NewManager builds a per-profile delegation manager. Start launches its
// loops; Stop joins them. Abandoned live children are reaped as unknown on
// the next Start.
func NewManager(deps Deps) *Manager {
	return &Manager{
		profile:      deps.Profile,
		profileRoot:  deps.ProfileRoot,
		log:          deps.Log,
		store:        deps.Store,
		broker:       deps.Broker,
		procs:        deps.Processes,
		registry:     deps.Registry,
		mcp:          deps.MCP,
		soul:         deps.Soul,
		images:       deps.Images,
		window:       deps.Window,
		vkey:         deps.VirtualKey,
		cfg:          deps.Config,
		parentClient: deps.Parent,
		newClient:    deps.NewClient,
		live:         make(map[string]*liveChild),
		clients:      make(map[string]llm.Chatter),
		started:      time.Now(),
	}
}

// liveChild tracks one running unit. Units of one batch share the call id;
// each unit reports independently through the delivery loop.
type liveChild struct {
	id         string
	callID     string
	goal       string
	role       string
	depth      int
	origin     originRef
	started    time.Time
	cancel     context.CancelFunc
	steer      *agent.SteerInbox
	activity   *unitActivity
	mu         sync.Mutex
	steers     []acceptedSteer
	done       bool
	finishedAt time.Time
	graceUntil time.Time
	forceFinal bool
	// routeModel and routeProvider record the resolved model routing for
	// terminal write-ups (e.g. force-finalize) that run without a spec.
	routeModel    string
	routeProvider string
	// drop, when set by /new cancellation, tells the worker to mark the
	// terminal record delivered without enqueueing it: the owning
	// conversation is gone, so there is nowhere to deliver to.
	drop bool
}

type originRef struct {
	profile      string
	sessionKey   string
	principal    identity.Principal
	displayName  string
	gateway      string
	enabledTools []string
	mcpServers   []string
	stateless    bool
}

type acceptedSteer struct {
	text      string
	at        time.Time
	toolCalls int64
}
