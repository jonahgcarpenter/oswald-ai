package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// discordAPIBase is the default Discord REST root.
const discordAPIBase = "https://discord.com/api/v10"

// discordChannel is the subset of a channel object the validator needs.
type discordChannel struct {
	ID      string `json:"id"`
	Type    int    `json:"type"`
	GuildID string `json:"guild_id"`
}

// discordTargetValidator checks discord deliver targets against the live
// Discord API. It is read-only except for the DM-channel lookup, which
// opens a channel object server-side without sending anything.
type discordTargetValidator struct {
	apiBase string
	token   string
	client  *http.Client
}

// NewDiscordTargetValidator returns a TargetValidator backed by the Discord
// REST API. An empty token fails every target closed: without a reachable
// API no discord delivery is possible.
func NewDiscordTargetValidator(apiBase, token string) TargetValidator {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = discordAPIBase
	}
	return &discordTargetValidator{apiBase: strings.TrimRight(apiBase, "/"), token: token, client: &http.Client{Timeout: cronValidateTimeout}}
}

var _ TargetValidator = (*discordTargetValidator)(nil)

// ValidateTarget implements TargetValidator.
func (v *discordTargetValidator) ValidateTarget(ctx context.Context, chatID, userID string) error {
	if v == nil {
		return fmt.Errorf("discord deliver target cannot be verified: no validator")
	}
	chatID = strings.TrimSpace(chatID)
	userID = strings.TrimSpace(userID)
	if chatID == "" {
		return fmt.Errorf("discord deliver chat_id is required")
	}
	if strings.EqualFold(chatID, "dm") || (userID != "" && chatID == userID) {
		return v.validateDMUser(ctx, userID)
	}
	channel, err := v.fetchChannel(ctx, chatID)
	if err != nil {
		return err
	}
	if userID == "" || channel.GuildID == "" {
		return nil
	}
	return v.validateGuildMember(ctx, channel.GuildID, userID)
}

// validateDMUser resolves the recipient through the DM-channel lookup.
// Any failure fails closed: the handle is unknown or unreachable.
func (v *discordTargetValidator) validateDMUser(ctx context.Context, userID string) error {
	if strings.TrimSpace(v.token) == "" {
		return fmt.Errorf("discord deliver target cannot be verified: Discord is not configured")
	}
	if userID == "" {
		return fmt.Errorf("discord deliver user_id is required for mentions and DMs")
	}
	payload, err := json.Marshal(map[string]string{"recipient_id": userID})
	if err != nil {
		return fmt.Errorf("discord deliver target cannot be verified: %v", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, cronValidateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, v.apiBase+"/users/@me/channels", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("discord deliver target cannot be verified: %v", err)
	}
	v.authorize(req)
	status, err := v.roundTripStatus(req)
	if err != nil {
		return fmt.Errorf("discord deliver target cannot be verified: %v", err)
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound || status == http.StatusBadRequest:
		return fmt.Errorf("discord deliver user is unknown")
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return fmt.Errorf("discord deliver user is not reachable (http %d)", status)
	default:
		return fmt.Errorf("discord deliver target cannot be verified (http %d)", status)
	}
}

// fetchChannel rejects unknown or inaccessible channels. Anything else
// fails closed: only a confirmed channel may be scheduled.
func (v *discordTargetValidator) fetchChannel(ctx context.Context, chatID string) (*discordChannel, error) {
	if strings.TrimSpace(v.token) == "" {
		return nil, fmt.Errorf("discord deliver target cannot be verified: Discord is not configured")
	}
	callCtx, cancel := context.WithTimeout(ctx, cronValidateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, v.apiBase+"/channels/"+chatID, nil)
	if err != nil {
		return nil, fmt.Errorf("discord deliver target cannot be verified: %v", err)
	}
	v.authorize(req)
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord deliver target cannot be verified: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var channel discordChannel
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&channel); err != nil {
			return nil, fmt.Errorf("discord deliver target cannot be verified: %v", err)
		}
		return &channel, nil
	case resp.StatusCode == http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("discord deliver channel is unknown")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("discord deliver channel is not accessible (http %d)", resp.StatusCode)
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("discord deliver target cannot be verified (http %d)", resp.StatusCode)
	}
}

// validateGuildMember confirms the mention target belongs to the channel's
// guild. A 404 is affirmative evidence of non-membership.
func (v *discordTargetValidator) validateGuildMember(ctx context.Context, guildID, userID string) error {
	callCtx, cancel := context.WithTimeout(ctx, cronValidateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, v.apiBase+"/guilds/"+guildID+"/members/"+userID, nil)
	if err != nil {
		return fmt.Errorf("discord deliver target cannot be verified: %v", err)
	}
	v.authorize(req)
	status, err := v.roundTripStatus(req)
	if err != nil {
		return fmt.Errorf("discord deliver target cannot be verified: %v", err)
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound:
		return fmt.Errorf("discord deliver user is not a member of the channel's guild")
	default:
		return fmt.Errorf("discord deliver target cannot be verified (http %d)", status)
	}
}

func (v *discordTargetValidator) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bot "+v.token)
	req.Header.Set("Content-Type", "application/json")
}

func (v *discordTargetValidator) roundTripStatus(req *http.Request) (int, error) {
	resp, err := v.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	return resp.StatusCode, nil
}
