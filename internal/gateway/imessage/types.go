package imessage

import (
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	chatStyleGroup     = 43
	chatStyleDirect    = 45
	defaultWebhookPath = "/imessage/webhook"
	defaultSendMethod  = "private-api"
	fallbackSendMethod = "apple-script"
	messageIndexTTL    = time.Hour
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
	Broker              *broker.Broker
	HTTPClient          *http.Client
	messageMu           sync.RWMutex
	messageIndex        map[string]messageContext
}

type webhookEvent struct {
	Type string         `json:"type"`
	Data webhookMessage `json:"data"`
}

type webhookMessage struct {
	GUID                  string        `json:"guid"`
	TempGUID              string        `json:"tempGuid"`
	Text                  string        `json:"text"`
	IsFromMe              bool          `json:"isFromMe"`
	Handle                messageHandle `json:"handle"`
	GroupTitle            string        `json:"groupTitle"`
	AssociatedMessageGUID string        `json:"associatedMessageGuid"`
	AssociatedMessageType string        `json:"associatedMessageType"`
	ReplyToGUID           string        `json:"replyToGuid"`
	ThreadOriginatorGUID  string        `json:"threadOriginatorGuid"`
	ThreadOriginatorPart  int           `json:"threadOriginatorPart"`
	Chats                 []messageChat `json:"chats"`
	DateCreated           int64         `json:"dateCreated"`
	DateRead              *int64        `json:"dateRead"`
	DateDelivered         *int64        `json:"dateDelivered"`
	DateEdited            *int64        `json:"dateEdited"`
	DateRetracted         *int64        `json:"dateRetracted"`
}

type messageHandle struct {
	Address string `json:"address"`
	Service string `json:"service"`
}

type messageChat struct {
	GUID        string `json:"guid"`
	DisplayName string `json:"displayName"`
	Style       int    `json:"style"`
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
	Status  int    `json:"status"`
	Message string `json:"message"`
	Data    struct {
		GUID     string `json:"guid"`
		TempGUID string `json:"tempGuid"`
	} `json:"data"`
	Error *struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	} `json:"error,omitempty"`
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
