package discord

import (
	"errors"
	"net/url"
	"strconv"
)

// This file builds the Discord OAuth2 "add bot to server" invite URL for a
// completed installation. It is a pure URL builder only: no HTTP handler
// (subtask 4.4 owns exposing this over the API), no persistence, no router
// wiring. The app id it consumes is installConfig.AppID /
// PreparedInstall.AppIDKey — the bot user id Discord returned from
// VerifyBotToken, already stored (or about to be stored) on the
// installation.

// Discord permission-flag bit positions this adapter requests, named and
// sourced individually so the requested mask is auditable bit-by-bit rather
// than a single hardcoded integer. Values verified against Discord's
// documented permission flags
// (https://discord.com/developers/docs/topics/permissions#permissions-bitwise-permission-flags).
const (
	// permViewChannel lets the bot see the guild channels it is invited
	// into; without it every other granted permission is moot.
	permViewChannel uint64 = 1 << 10
	// permSendMessages lets the bot post agent replies into a channel.
	permSendMessages uint64 = 1 << 11
	// permEmbedLinks lets agent replies render rich embeds instead of bare
	// link previews.
	permEmbedLinks uint64 = 1 << 14
	// permAttachFiles lets the bot upload files (e.g. generated artifacts)
	// as part of a reply.
	permAttachFiles uint64 = 1 << 15
	// permReadMessageHistory lets the bot read prior messages in a channel
	// it is mentioned in, needed to build conversation context.
	permReadMessageHistory uint64 = 1 << 16
	// permSendMessagesInThreads lets the bot reply inside a thread, not
	// just a top-level channel. This bit position is > 32 bits, so the
	// combined mask below must not be truncated to a 32-bit type.
	permSendMessagesInThreads uint64 = 1 << 38
)

// invitePermissions is the minimal permission mask requested at install
// time: exactly the six bits above, no more. Every bit here must be
// justified in the constant's own doc comment; do not OR in a new
// permission without adding one.
const invitePermissions uint64 = permViewChannel |
	permSendMessages |
	permEmbedLinks |
	permAttachFiles |
	permReadMessageHistory |
	permSendMessagesInThreads

// inviteScope is deliberately just "bot": native slash commands
// (`applications.commands`) are out of scope for v1, and requesting a scope
// this adapter does not use would be an unjustified permission ask shown to
// the installing server admin at Discord's consent screen. Do not add
// "applications.commands" here without a product decision to support slash
// commands.
const inviteScope = "bot"

const inviteAuthorizeURL = "https://discord.com/oauth2/authorize"

// BuildInviteURL returns the Discord OAuth2 URL that adds the bot
// identified by appID to a server, requesting only the "bot" scope and the
// minimal permission mask in invitePermissions. appID is untrusted stored
// config (it round-trips through installConfig.AppID), so it is validated
// rather than assumed well-formed, and it is placed into the URL via
// net/url so any hostile characters are escaped rather than concatenated.
func BuildInviteURL(appID string) (string, error) {
	if appID == "" {
		return "", errors.New("discord: app id is empty")
	}

	q := url.Values{}
	q.Set("client_id", appID)
	q.Set("scope", inviteScope)
	q.Set("permissions", strconv.FormatUint(invitePermissions, 10))

	u, err := url.Parse(inviteAuthorizeURL)
	if err != nil {
		// inviteAuthorizeURL is a fixed literal; a parse failure here would
		// mean the constant itself is broken, not a runtime input problem.
		return "", err
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
