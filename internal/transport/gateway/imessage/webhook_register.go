package imessage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

const webhookEventNewMessage = "new-message"

// webhookDialTimeout bounds outbound-interface detection. The dial sends no
// packets; it only selects the local address used to reach BlueBubbles.
const webhookDialTimeout = 5 * time.Second

type webhookRegistration struct {
	ID     int      `json:"id"`
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

type webhookListResponse struct {
	Data  []webhookRegistration `json:"data"`
	Error *struct {
		Error string `json:"error"`
	} `json:"error,omitempty"`
}

type webhookRegisterRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

// ensureWebhookRegistered verifies the BlueBubbles server forwards new-message
// events to this gateway and registers the detected callback when absent.
// Every failure degrades to a WARN so startup never depends on registration.
func (g *Gateway) ensureWebhookRegistered(scoped ...*config.Logger) {
	log := g.log(scoped...)
	ip, ok := g.webhookCallbackIP()
	if !ok {
		log.Warn("gateway.bluebubbles.webhook_unavailable", "cannot determine BlueBubbles webhook callback", config.F("reason_code", "callback_ip_unresolved"), config.F("status", "degraded"))
		return
	}
	existing, err := g.fetchBlueBubblesWebhooks()
	if err != nil {
		log.Warn("gateway.bluebubbles.webhook_unavailable", "failed to list BlueBubbles webhooks", config.F("reason_code", "list_failed"), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	for _, entry := range existing {
		if webhookCallbackMatches(entry.URL, ip, g.Port, g.listenPath()) && webhookEntryHasEvent(entry, webhookEventNewMessage) {
			log.Debug("gateway.bluebubbles.webhook_ensured", "BlueBubbles webhook already registered", config.F("is_registered", true))
			return
		}
	}
	callback := buildWebhookCallbackURL(ip, g.Port, g.listenPath(), g.BlueBubblesPassword)
	if err := g.registerBlueBubblesWebhook(callback); err != nil {
		log.Warn("gateway.bluebubbles.webhook_unavailable", "failed to register BlueBubbles webhook", config.F("reason_code", "register_failed"), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	log.Debug("gateway.bluebubbles.webhook_ensured", "registered BlueBubbles webhook", config.F("is_registered", true))
}

// webhookCallbackIP returns the local address used to reach the BlueBubbles
// server. DHCP drift changes this over time, which is why the callback is
// re-verified on every start instead of persisted.
func (g *Gateway) webhookCallbackIP() (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(g.BlueBubblesURL))
	if err != nil || parsed.Host == "" {
		return "", false
	}
	host := parsed.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "80"
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		}
		host = net.JoinHostPort(parsed.Hostname(), port)
	}
	conn, err := net.DialTimeout("udp", host, webhookDialTimeout)
	if err != nil {
		return "", false
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return "", false
	}
	ip := addr.IP.String()
	if zone := addr.Zone; zone != "" {
		ip += "%" + zone
	}
	if strings.TrimSpace(ip) == "" {
		return "", false
	}
	return ip, true
}

// buildWebhookCallbackURL assembles the callback BlueBubbles replays verbatim
// on each event. The password is appended raw to match existing registrations;
// it is stripped locally before any comparison and never logged.
func buildWebhookCallbackURL(ip, port, path, password string) string {
	base := "http://" + net.JoinHostPort(ip, port) + path
	if strings.TrimSpace(password) == "" {
		return base
	}
	return base + "?password=" + password
}

// webhookCallbackMatches compares a stored webhook URL against the expected
// callback by host, port, and path only. The auth query is ignored so raw and
// encoded password forms both match; stale host IPs never match.
func webhookCallbackMatches(rawURL, ip, port, path string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	if parsed.Path != path {
		return false
	}
	if !strings.EqualFold(parsed.Hostname(), ip) {
		return false
	}
	if parsed.Port() == port {
		return true
	}
	if parsed.Port() != "" {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	}
	return false
}

func webhookEntryHasEvent(entry webhookRegistration, event string) bool {
	for _, candidate := range entry.Events {
		if strings.EqualFold(strings.TrimSpace(candidate), event) {
			return true
		}
	}
	return false
}

func (g *Gateway) fetchBlueBubblesWebhooks() ([]webhookRegistration, error) {
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/webhook", g.BlueBubblesPassword)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build BlueBubbles webhook list request: %w", err)
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("list BlueBubbles webhooks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list BlueBubbles webhooks status %d", resp.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(resp.Body, replyLookupBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read BlueBubbles webhook list: %w", err)
	}
	if len(encoded) > replyLookupBodyLimit {
		return nil, fmt.Errorf("BlueBubbles webhook list exceeds body limit")
	}
	var result webhookListResponse
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("decode BlueBubbles webhook list: %w", err)
	}
	return result.Data, nil
}

func (g *Gateway) registerBlueBubblesWebhook(callback string) error {
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/webhook", g.BlueBubblesPassword)
	if err != nil {
		return err
	}
	payload := webhookRegisterRequest{URL: callback, Events: []string{webhookEventNewMessage}}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal BlueBubbles webhook register payload: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build BlueBubbles webhook register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("register BlueBubbles webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("register BlueBubbles webhook status %d", resp.StatusCode)
	}
	return nil
}
