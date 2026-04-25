package imessage

import (
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/metrics"
)

const (
	chatStyleGroup     = 43
	chatStyleDirect    = 45
	defaultWebhookPath = "/imessage/webhook"
	defaultSendMethod  = "private-api"
	fallbackSendMethod = "apple-script"
	messageIndexTTL    = time.Hour
	contactCacheTTL    = 6 * time.Hour
)

var mentionRE = regexp.MustCompile(`@?Oswald\b`)

// Gateway receives BlueBubbles webhooks and sends replies via its REST API.
type Gateway struct {
	Port                string
	WebhookPath         string
	BlueBubblesURL      string
	BlueBubblesPassword string
	Links               *accountlink.Service
	Commands            *accountlink.CommandHandler
	Log                 *config.Logger
	Metrics             *metrics.Metrics
	Broker              *broker.Broker
	HTTPClient          *http.Client
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

type messageContext struct {
	SessionKey  string
	ChatGUID    string
	SenderID    string
	DisplayName string
	Text        string
	IsFromBot   bool
	CreatedAt   time.Time
}

func (g *Gateway) httpClient() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}
