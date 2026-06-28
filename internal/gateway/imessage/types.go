package imessage

import (
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
)

const (
	chatStyleGroup       = 43
	chatStyleDirect      = 45
	defaultWebhookPath   = "/imessage/webhook"
	defaultSendMethod    = "private-api"
	fallbackSendMethod   = "apple-script"
	capabilityAttempts   = 5
	capabilityRetryDelay = 500 * time.Millisecond
	typingAfterReadDelay = 150 * time.Millisecond
	messageIndexTTL      = time.Hour
	contactCacheTTL      = 6 * time.Hour
)

var mentionRE = regexp.MustCompile(`@?Oswald\b`)

// Gateway receives BlueBubbles webhooks and sends replies via its REST API.
type Gateway struct {
	Port                string
	WebhookPath         string
	BlueBubblesURL      string
	BlueBubblesPassword string
	Links               *accountlinking.Service
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
}

type webhookEvent struct {
	Type string         `json:"type"`
	Data webhookMessage `json:"data"`
}

type webhookMessage struct {
	GUID                  string        `json:"guid"`
	Text                  string        `json:"text"`
	IsFromMe              bool          `json:"isFromMe"`
	Handle                messageHandle `json:"handle"`
	Attachments           []attachment  `json:"attachments"`
	AssociatedMessageType string        `json:"associatedMessageType"`
	ReplyToGUID           string        `json:"replyToGuid"`
	ThreadOriginatorGUID  string        `json:"threadOriginatorGuid"`
	Chats                 []messageChat `json:"chats"`
}

type attachment struct {
	GUID         string `json:"guid"`
	MimeType     string `json:"mimeType"`
	TransferName string `json:"transferName"`
	TotalBytes   int    `json:"totalBytes"`
}

type messageHandle struct {
	Address string `json:"address"`
}

type messageChat struct {
	GUID  string `json:"guid"`
	Style int    `json:"style"`
}

type sendTextRequest struct {
	ChatGUID            string `json:"chatGuid"`
	Message             string `json:"message"`
	Method              string `json:"method,omitempty"`
	SelectedMessageGUID string `json:"selectedMessageGuid,omitempty"`
	PartIndex           int    `json:"partIndex,omitempty"`
	TempGUID            string `json:"tempGuid,omitempty"`
}

type sendTextResponse struct {
	Data struct {
		GUID string `json:"guid"`
	} `json:"data"`
	Error *struct {
		Error string `json:"error"`
	} `json:"error,omitempty"`
}

type messageLookupResponse struct {
	Data  messageLookupData `json:"data"`
	Error *struct {
		Error string `json:"error"`
	} `json:"error,omitempty"`
}

type messageQueryRequest struct {
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
	With   []string             `json:"with"`
	Where  []messageQueryClause `json:"where"`
}

type messageQueryClause struct {
	Statement string            `json:"statement"`
	Args      map[string]string `json:"args"`
}

type messageQueryResponse struct {
	Data  []messageLookupData `json:"data"`
	Error *struct {
		Error string `json:"error"`
	} `json:"error,omitempty"`
}

type messageLookupData struct {
	GUID        string        `json:"guid"`
	Text        string        `json:"text"`
	IsFromMe    bool          `json:"isFromMe"`
	Handle      messageHandle `json:"handle"`
	Chats       []messageChat `json:"chats"`
	Attachments []attachment  `json:"attachments"`
}

type contactQueryRequest struct {
	Addresses []string `json:"addresses"`
}

type contactQueryResponse struct {
	Data []contactRecord `json:"data"`
}

type contactRecord struct {
	DisplayName string `json:"displayName"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Nickname    string `json:"nickname"`
}

type contactNameCacheEntry struct {
	DisplayName string
	ExpiresAt   time.Time
}

type serverInfoResponse struct {
	Data struct {
		PrivateAPI      bool `json:"private_api"`
		HelperConnected bool `json:"helper_connected"`
	} `json:"data"`
}

type messageContext struct {
	SessionKey  string
	ChatGUID    string
	SenderID    string
	DisplayName string
	Text        string
	Attachments []attachment
	IsFromBot   bool
	CreatedAt   time.Time
}

func (g *Gateway) httpClient() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}
