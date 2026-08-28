// Package discord is the Discord integration for the channel-agnostic
// engine. It follows the Slack/Telegram bring-your-own-bot (BYO) model: a
// workspace admin creates a Discord application + bot in the Discord
// developer portal, pastes its bot token into Multica, and the installation
// is keyed by the application's numeric id. Inbound runs on a
// per-installation Gateway WebSocket connection (discord_channel.go),
// supervised per-installation by engine.Supervisor — the same
// "persistent per-installation connection" shape Feishu, Slack and WeCom
// use. Outbound agent replies use the Discord REST API.
//
// This package is scaffolding only (MUL task 2, subtask 2.1): the Gateway
// connection, inbound event handling and outbound sending are implemented
// by later subtasks; see discord_channel.go for the exact stub markers.
//
// Maintenance: this package is COMMUNITY-MAINTAINED. Its maintainers, the
// support boundary and the retirement rule are published at
// https://multica.ai/docs/community-maintained
// (apps/docs/content/docs/community-maintained.mdx, four locales). That page
// is the single source of truth — record ownership changes there, not here.
// Changing the shared channel engine? Keep this adapter building, and loop in
// its maintainers for anything that changes Discord-visible behavior.
package discord

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// TypeDiscord is the channel discriminator for the Discord adapter. Defined
// here (not in the channel core) so registering the platform never edits the
// core, mirroring TypeTelegram/TypeSlack.
const TypeDiscord channel.Type = "discord"

// installConfig is the JSON shape stored in channel_installation.config for a
// Discord installation.
//
// app_id holds the Discord application id (the "Application ID" shown in the
// developer portal, also the bot user's id) as a string. It fills the
// generic (channel_type, config->>'app_id') routing slot; the unique index
// guarantees one Discord application maps to one agent across all
// workspaces.
//
// bot_username is the bot's display name, kept for @-mention matching in
// guild channels without a per-message API round trip. Optional: older or
// partially-configured installations may not have it populated yet.
//
// bot_token_encrypted is base64-encoded secretbox ciphertext, never
// plaintext, mirroring Telegram's bot_token_encrypted and Slack's
// bot_token_encrypted.
type installConfig struct {
	AppID             string `json:"app_id"`
	BotUsername       string `json:"bot_username,omitempty"`
	BotTokenEncrypted string `json:"bot_token_encrypted"`
}

// credentials is the decoded, decrypted form the Gateway connection and REST
// sender run on. The installation identity (workspace / agent / installer)
// is deliberately absent: it is resolved per message by the Router, as in
// Telegram, Feishu and Slack.
type credentials struct {
	BotID       string
	BotUsername string
	BotToken    string
}

// Decrypter turns stored ciphertext into plaintext. The wiring injects a
// secretbox-backed implementation; tests inject nil (stored bytes are
// treated as plaintext).
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials parses the per-installation config blob and decrypts the
// stored bot token. It is the single place the Discord config JSON is
// interpreted.
func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	if len(raw) == 0 {
		return credentials{}, errors.New("discord: empty installation config")
	}
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return credentials{}, fmt.Errorf("decode discord installation config: %w", err)
	}
	token, err := decryptToken(cfg.BotTokenEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("decrypt bot token: %w", err)
	}
	return credentials{
		BotID:       cfg.AppID,
		BotUsername: cfg.BotUsername,
		BotToken:    token,
	}, nil
}

// PublicConfig is the non-secret subset of an installation config, safe to
// surface on the management API (the encrypted bot token is never included).
type PublicConfig struct {
	BotID       string
	BotUsername string
}

// DecodePublicConfig extracts the display-safe fields from a stored config
// blob. A decode miss yields a zero PublicConfig rather than an error so the
// management list still renders the row.
func DecodePublicConfig(raw json.RawMessage) PublicConfig {
	var cfg installConfig
	_ = json.Unmarshal(raw, &cfg)
	return PublicConfig{BotID: cfg.AppID, BotUsername: cfg.BotUsername}
}

// decryptToken base64-decodes the stored ciphertext (tolerating MIME newline
// wrapping) and runs it through the injected Decrypter; mirrors the
// Telegram/DingTalk helper of the same name.
func decryptToken(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stripWhitespace(enc))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
