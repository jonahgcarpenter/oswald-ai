package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// cronValidateTimeout bounds one deliver-target liveness probe.
const cronValidateTimeout = 15 * time.Second

// cronValidateMessageLimit bounds the participant-evidence query.
const cronValidateMessageLimit = 10

var (
	cronPhoneHandle = regexp.MustCompile(`^\+[0-9]+$`)
	cronEmailHandle = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// imessageTargetValidator checks imessage deliver targets against the live
// BlueBubbles server. It is read-only: chat lookups and message queries
// only, never sends.
type imessageTargetValidator struct {
	url      string
	password string
	client   *http.Client
}

// NewIMessageTargetValidator returns a TargetValidator backed by the
// BlueBubbles server. Empty URL or password fails every target closed:
// without a reachable server no imessage delivery is possible.
func NewIMessageTargetValidator(serverURL, password string) TargetValidator {
	return &imessageTargetValidator{url: serverURL, password: password, client: &http.Client{Timeout: cronValidateTimeout}}
}

var _ TargetValidator = (*imessageTargetValidator)(nil)

// ValidateTarget implements TargetValidator.
func (v *imessageTargetValidator) ValidateTarget(ctx context.Context, chatID, userID string) error {
	if v == nil {
		return fmt.Errorf("imessage deliver target cannot be verified: no validator")
	}
	chatID = strings.TrimSpace(chatID)
	userID = strings.TrimSpace(userID)
	if chatID == "" {
		return fmt.Errorf("imessage deliver chat_id is required")
	}
	if strings.EqualFold(chatID, "dm") || (userID != "" && chatID == userID) {
		return validateIMessageHandle(userID)
	}
	if idx := strings.LastIndex(chatID, ";-;"); idx >= 0 {
		return validateIMessageHandle(chatID[idx+3:])
	}
	if !strings.Contains(chatID, ";+;") {
		return fmt.Errorf("imessage deliver chat_id is not a chat GUID (want iMessage;+;<group> or a DM)")
	}
	if err := v.chatExists(ctx, chatID); err != nil {
		return err
	}
	if userID == "" {
		return nil
	}
	if err := validateIMessageHandle(userID); err != nil {
		return err
	}
	return v.mentionIsParticipant(ctx, chatID, userID)
}

func validateIMessageHandle(handle string) error {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return fmt.Errorf("imessage deliver user_id is required for mentions and DMs")
	}
	if cronPhoneHandle.MatchString(handle) || cronEmailHandle.MatchString(handle) {
		return nil
	}
	return fmt.Errorf("imessage deliver user_id is not a phone number or Apple ID email")
}

// blueBubblesEndpoint builds an authenticated BlueBubbles REST endpoint.
func (v *imessageTargetValidator) blueBubblesEndpoint(path string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(v.url))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	query := parsed.Query()
	query.Set("password", v.password)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// chatExists rejects unknown or inaccessible group chats. Any other
// failure fails closed: an unverifiable target must not be scheduled.
func (v *imessageTargetValidator) chatExists(ctx context.Context, chatGUID string) error {
	if strings.TrimSpace(v.url) == "" || strings.TrimSpace(v.password) == "" {
		return fmt.Errorf("imessage deliver target cannot be verified: BlueBubbles is not configured")
	}
	endpoint, err := v.blueBubblesEndpoint("/api/v1/chat/" + chatGUID)
	if err != nil {
		return fmt.Errorf("imessage deliver target cannot be verified: %v", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, cronValidateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("imessage deliver target cannot be verified: %v", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("imessage deliver target cannot be verified: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("imessage deliver chat is unknown to BlueBubbles (stale or mistyped GUID)")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("imessage deliver chat is not accessible (http %d)", resp.StatusCode)
	default:
		return fmt.Errorf("imessage deliver target cannot be verified (http %d)", resp.StatusCode)
	}
}

// mentionIsParticipant checks the mention handle against recent senders in
// the chat. Empty evidence fails open: a new or quiet group has no senders
// to compare, but its existence was already verified.
func (v *imessageTargetValidator) mentionIsParticipant(ctx context.Context, chatGUID, mention string) error {
	endpoint, err := v.blueBubblesEndpoint("/api/v1/message/query")
	if err != nil {
		return nil
	}
	payload, err := json.Marshal(map[string]interface{}{
		"chatGuid": chatGUID, "limit": cronValidateMessageLimit, "sort": "DESC", "with": []string{"handle"},
	})
	if err != nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, cronValidateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return nil
	}
	var result struct {
		Data []struct {
			Handle struct {
				Address string `json:"address"`
			} `json:"handle"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, msg := range result.Data {
		if addr := strings.TrimSpace(msg.Handle.Address); addr != "" {
			seen[addr] = true
		}
	}
	if len(seen) == 0 || seen[strings.TrimSpace(mention)] {
		return nil
	}
	return fmt.Errorf("imessage deliver user_id is not a participant of the group")
}
