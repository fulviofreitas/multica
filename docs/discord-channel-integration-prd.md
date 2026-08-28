# PRD: Discord Channel Integration on a Multica Fork

> Status: **proposal**. Written 2026-08-27 against upstream `main` @ `3d37828e9` (v0.4.35,
> 2026-08-26). Per repo convention (`docs/db-backed-execution-scheduler-rfc.md` precedent;
> MUL-5698 cleanup policy in PR #6364), this document lives in `docs/` while the work is
> live and is deleted once the decisions are captured in code.
>
> Every number, path, and PR reference in this document was measured or read directly from
> the repository, its GitHub project, or the two forks named below, on 2026-08-27.
> Statements that are inferences or recommendations are marked **[inference]** or
> **[recommendation]**. External Discord-platform figures are marked **[Discord docs]**
> (fetched from docs.discord.com on 2026-08-27) and the one figure I could not re-verify is
> flagged as an open question.

---

## 1. Problem statement and user value

Multica is an AI-native task manager where agents are first-class assignees. Its channel
subsystem lets a team talk to an agent from the IM tool they already live in: Feishu/Lark,
Slack, WeCom, DingTalk, and (since 2026-08-19, PR #6944) Telegram. Discord is absent —
despite Multica running its own community on Discord (sidebar promo shipped in #4388/#4400)
and bug #7215 being reported *through* Discord because the reporter had no in-product path.

Discord is the default home of open-source, gaming, and indie-dev teams — exactly the
small-team segment Multica targets (`VISION.md`). For those teams, "assign an issue by
@-mentioning the bot in our Discord server", "DM the agent and get a reply", and "each
Discord thread is its own agent session" is the same value Telegram/Slack users already
get. Upstream has no Discord channel work in flight: a GitHub search of
`multica-ai/multica` for "discord" in titles returns only community-Discord promo UI
(#4388, #4400, #6138, #6359) and community requests (#1852, #4311, #4398) — no channel
integration issue or PR (verified 2026-08-27).

This PRD covers three coupled decisions: (a) standing up and maintaining a fork of
`multica-ai/multica`, (b) building a first-class Discord channel on it, and (c) how the
work is decomposed across the agents actually available in this execution environment.

## 2. Goals

1. A Discord adapter registered as `channel.Type("discord")` on the existing
   `channel.Channel` engine — Gateway inbound, REST outbound — with feature parity with
   the Telegram adapter (session bindings, `/new`–`/clear` directives, `/issue`-style
   intake via origin widening, user binding, streaming message edits).
2. Delivered on a fork with a maintenance discipline that provably avoids the two failure
   modes measured in the Mininglamp-OSS fork (§8): parallel-table architecture requiring a
   later convergence rewrite, and migration-number collision requiring a renumber hook.
3. An eventual upstream contribution into the community-maintained channel tier, using the
   fork as the staging ground (§8.1).
4. A delegation plan where every workstream names a subagent, skill, or MCP server that
   was confirmed present in this environment (§9).

## 3. Non-goals

- **Voice channels, stage channels, forum-post creation, presence.** The `Capability`
  bitmask has a `CapVoice` bit; Discord will not declare it (§7.4).
- **Rich embeds as the primary render path.** v1 renders Discord-flavored markdown text;
  `CapRichCard` is not declared (§7.4).
- **Native slash-command registration in v1** (§7.5) — deferred to a scoped follow-up.
- **Ambient listening in guild channels.** The engine's group model is @-mention-gated
  (see the Router's group @bot filtering, §5); we deliberately design v1 to not require
  the `MESSAGE_CONTENT` privileged intent (§7.2).
- **Sharding.** Mandatory only at 2,500 guilds per bot **[Discord docs]**; Multica's
  BYO-bot model (one bot per workspace-agent installation) makes this unreachable in
  practice. Documented as a declared ceiling, not built.
- **Squad-specific channel behavior.** Verified non-existent for every channel (§7.7);
  Discord matches the (empty) parity bar.
- **Multica's community-Discord presence** (marketing). Unrelated to the channel adapter.

## 4. Scope boundaries and measured surface area

### 4.1 What "a channel" costs in this codebase (measured)

Adapter package sizes at `3d37828e9` (`server/internal/integrations/<ch>/`, `wc -l` over
`*.go`):

| Channel | Go files | Total lines | Test lines | Files referencing it *outside* its own dirs¹ |
|---|---|---|---|---|
| Telegram | 18 | 6,193 | 2,690 | 66 |
| Slack | 30 | 8,284 | 3,789 | 219 |
| DingTalk | 43 | 9,582 | 5,117 | 131 |
| WeCom | 56 | 18,638 | 10,997 | 89 |
| Lark | 66 | 22,137 | 11,880 | 178 (as "lark") / 136 (as "feishu") |

¹ `rg -li <name>` minus paths containing `/<name>/` as a component; includes docs and
locale files. The prior analysis' reference figures (DingTalk ~125, WeCom ~80, Telegram
~62; Telegram ~6.5k/18, Slack ~8.4k/30, DingTalk ~9.9k/43, WeCom ~18.9k/56, Lark
~21.9k/65) were all within a few percent of these measurements but stale in detail —
this table is the measurement of record. The repo ships near-daily (12 release tags in
the 15 days to 2026-08-26, `v0.4.24`→`v0.4.35`), so expect these to drift again.

Telegram's 66 external touchpoints break down as: 20 `apps/docs`, 19 `packages/views`,
8 `packages/core`, 4 `server/internal`, 4 `apps/web`, 3 `server/migrations`, 2
`server/cmd`, 2 `apps/mobile`, 1 `server/pkg`, 1 root. Telegram is the floor for what
Discord will touch; Discord's Gateway transport adds adapter-internal complexity but no
extra external surface **[inference]**.

### 4.2 The Telegram shipping precedent — corrected

The prior analysis claimed Telegram shipped as "PR #6944 with backend inbound core staged
separately in PR #6009". **This is wrong in an instructive way** (verified via `git log`
and the GitHub API):

- **PR #6009** ("feat(telegram): task intake via bot — Plan 1 (backend inbound core)", 21
  files, +4,070, author `zdavison`) was **closed unmerged** with the author's own comment:
  *"Closing, this should target our fork (zdavison/multica), not upstream."*
- **PR #6944** (MUL-6166, author `leonzone`, branch `codex/telegram-integration`) merged
  2026-08-19 as **one squashed commit** `67c6aa05c`: **83 files, +9,196 / −59** —
  adapter, handler, router wiring, migrations 366/367, core types/schemas/queries,
  settings tab, bind page, and four-locale docs, all at once.

So upstream's actual precedent is: community contributors develop channels on their own
forks, and the channel lands upstream as a single consolidated PR into the
community-maintained tier. This PRD keeps a phased plan (§10) for *our* fork — a single
9k-line change is unreviewable internally — and treats "squash to one PR shaped like
#6944" as the *upstreaming* step, which resolves the apparent contradiction.

## 5. Architecture

### 5.1 Where Discord plugs in

The contract is `server/internal/integrations/channel/channel.go:34-76`: five methods —
`Type`, `Connect`, `Disconnect`, `Send`, `Capabilities`. `Connect` **blocks**, running
the receive loop; error return means "this attempt failed", and the engine's `Supervisor`
(`server/internal/integrations/channel/engine/supervisor.go`) reconnects under exponential
backoff (2s doubling to 60s cap, jitter in [0.5d, 1.5d), reset after 60s healthy uptime —
`supervisor.go:140-143`, `:934-950`). One goroutine per installation, fenced by a
token-CAS WS lease (Postgres or Redis backend, `CHANNEL_WS_LEASE_*` env knobs) so at most
one replica globally holds any installation's connection. Credential rotation is detected
per sweep via `Installation.Fingerprint` and tears down/rebuilds the connection
(`supervisor.go:36-43`); revocation propagates within one `PollInterval` (30s default).

Discord fits this shape: one Gateway WebSocket per installation, `Connect` blocks on the
read loop, `Disconnect` a no-op (Lark, WeCom, and Slack all do this), and the lease
already gives us Discord's "don't run concurrent sessions for one bot" requirement for
free. What does *not* exist anywhere in the engine is resume machinery — see §7.1.

### 5.2 The plugin claim, verified and bounded

`registry.go:16` claims *"Adding a platform is 'register a factory here', never 'edit the
core'."* **Measured verdict: true for the engine, false for the product.** The engine
itself (Supervisor, Router, lease store, capability bitmask) needed zero Telegram edits.
But shipping Telegram required editing every one of these central files, and Discord will
require the same list:

| File | Why |
|---|---|
| `server/cmd/server/router.go` | env-gated registration block (Telegram's is `:1015-1056`, gated on `MULTICA_TELEGRAM_SECRET_KEY`), resolver-set registration, HTTP routes (`:1676-1681`, `:1709`) |
| `server/cmd/server/main.go` | outbound worker start/shutdown (`:628`, `:744`) |
| `server/internal/handler/handler.go` | nilable per-channel service fields (`:326-336`) |
| `server/internal/handler/<channel>.go` | new install/list/revoke/bind endpoints (+265 lines for Telegram) |
| `server/pkg/protocol/events.go` | `<channel>_installation` realtime events (`:215-216`) |
| `server/internal/daemon/execenv/channel_type.go` | discriminator constant, `SurfacePersistsTranscript`, `ChannelDisplayName` — see §7.6 |
| `packages/views/settings/components/integrations-tab.tsx` | **hardcoded JSX list** — no dynamic registry; each channel is an import + a hand-written section (`:10`, `:97-108`) |
| `packages/views/settings/components/<channel>-tab.tsx`, `<channel>-mark.tsx`, `integration-channel-icon.tsx` | settings tab, icon |
| `packages/core/api/client.ts`, `schemas.ts`, `types/`, `<channel>/queries.ts` | API client + zod schemas (repo rule: never cast network JSON) |
| `packages/core/realtime/use-realtime-sync.ts` | literal event→invalidation map (`:855-877`) |
| `packages/views/locales/{en,zh-Hans,ja,ko}/{common,settings}.json` | four-locale strings |
| `apps/docs/content/docs/` | `channels(.zh/.ja/.ko).mdx`, a `discord-bot-integration` guide ×4, `meta*.json`, `environment-variables*` |

### 5.3 Discord adapter package layout **[recommendation]**

`server/internal/integrations/discord/`, mirroring Telegram's file roles (Telegram file →
Discord analogue): `config.go` (install config JSONB, `bot_token_encrypted`),
`install.go` (token validation via `GET /users/@me`, ownership-conflict classification),
`gateway.go` + `gateway_frames.go` (Gateway client: hello/heartbeat/identify/resume/
dispatch — replaces Telegram's `api.go` polling), `discord_channel.go`
(`channel.Channel` impl + `RegisterDiscord`), `inbound.go` (MESSAGE_CREATE →
`channel.InboundMessage` normalization, @-mention stripping), `resolvers.go`
(`engine.ResolverSet`: binding-key derivation, session ensure/start), `outbound.go`
(streaming `EventChatDone`/edit worker), `sender.go` (chunking at Discord's cap),
`replier.go`, `binding.go` (bind-token DM flow), `markdown.go` (Multica markdown →
Discord markdown). Use `gorilla/websocket` directly (already a repo dependency; Lark's
custom WS connector is the in-tree precedent at `lark/ws_connector.go`) rather than
adding `discordgo`/`disgo` — a full SDK drags in state caching and its own reconnect
loop, which fights the Supervisor's contract **[recommendation; the Lark adapter made
the same call against the Lark SDK]**.

## 6. Data model and migrations

### 6.1 Existing generalized tables (no new tables needed)

Migration `124_channel_generalization` created the platform-agnostic family Discord slots
into unchanged: `channel_installation`, `channel_user_binding`,
`channel_chat_session_binding`, `channel_inbound_message_dedup`, `channel_inbound_audit`,
`channel_outbound_card_message`, `channel_binding_token`; later additions
`channel_media_pending_object` (227), `channel_chat_context_generation` (377),
`channel_task_delivery` (420), `channel_outbound_message` (425).

Key rows for Discord:

- `channel_installation`: `UNIQUE (workspace_id, agent_id, channel_type)`; routing key is
  the functional unique index `(channel_type, config->>'app_id')` — Discord stores its
  **application ID** in the `app_id` slot (the same slot Slack fills with `team_id` and
  Telegram with the numeric bot id). Bot token encrypted at rest with
  `internal/util/secretbox` (AES-256-GCM, per-integration master key env var; ours:
  `MULTICA_DISCORD_SECRET_KEY`). Every `InstallService` refuses a nil box, dev included.
- `channel_chat_session_binding`: `chat_type TEXT CHECK IN ('p2p','group')`, partial
  unique active-route index `(installation_id, channel_chat_id) WHERE retired_at IS NULL`
  (421/422), `route_revision` generations backing the `/new` rotation (#7468). The query
  comment at `server/pkg/db/queries/channel.sql:532-537` documents the pattern Discord
  reuses: `channel_chat_id` is the session-isolation key and may be a *composite*
  (Slack: channel+thread-root; Telegram forum topics: `chat:thread`), with the raw
  platform ids stashed in the binding `config` JSONB.
- `channel_binding_token`: SHA-256-hashed one-time tokens, DB-enforced 15-minute TTL cap
  — reused verbatim for the Discord `/discord/bind` flow.

### 6.2 New migrations (exactly one pair, mirroring Telegram)

Telegram needed only `366_issue_origin_telegram_chat` (widen the `issue.origin_type`
CHECK, recreated `NOT VALID` to keep the ACCESS EXCLUSIVE lock brief) and
`367_..._validate` (`VALIDATE CONSTRAINT` separately under SHARE UPDATE EXCLUSIVE).
Discord needs the identical pair adding `discord_chat`. Hard rules from `CLAUDE.md`
apply: no FKs, every index `CONCURRENTLY` in its own single-statement file, idempotent
DDL for anything conditionally present.

Numbering strategy is a fork-maintenance question — §8.3.

## 7. Resolved Discord decisions

### 7.1 Transport: Gateway WebSocket, with an adapter-local resume cache

**Options.** (a) Gateway WebSocket; (b) webhooks/interactions-only (HTTP).

**Recommendation: Gateway.** Interactions-only cannot receive plain messages or DMs at
all — it would reduce Discord to slash commands, below every existing channel's bar. The
webhook mode also requires a public HTTPS endpoint, which self-hosted deployments (the
`docker-compose.selfhost.yml` audience) often lack; every recent channel (WeCom aibot,
DingTalk stream, Lark long-conn, Telegram long-poll) deliberately chose an *outbound*
connection for exactly this reason **[inference from the four adapters' uniform choice]**.

**Fit with the supervisor — verified, with three gaps.** The engine contract is
stateless-reconnect-from-scratch: Lark re-bootstraps a single-use WS URL per `Connect`,
WeCom re-subscribes, Slack builds a new Socket Mode client, and *nothing* persists
session state across attempts — no sequence tracking, no resume path, no connect rate
limiter beyond backoff jitter (verified by sweep of all five adapters and the engine).
Discord's Gateway adds three things the engine has never needed **[Discord docs]**:

1. **RESUME.** On non-fatal disconnects a session stays resumable for several minutes via
   `resume_gateway_url` + `session_id` + last `seq`; close codes 1000/1001 invalidate.
   *Resolution:* keep the Supervisor untouched; give the adapter a process-local resume
   cache keyed by installation ID (precedent: WeCom's `sendersRegistry`, a process-wide
   map the lease makes safe). `Connect` consults it — resume if fresh, else IDENTIFY.
   Not persisted to DB: the resume window is minutes, and a process restart falling back
   to IDENTIFY is correct. **[recommendation]**
2. **IDENTIFY budget.** 1,000 session starts per 24h; exhausting it **resets the bot
   token**, killing the installation until the human re-pastes a token. The engine's
   60s-cap backoff yields a worst case of ~1,440 attempts/day on a permanently failing
   link — over budget. *Resolution:* the adapter enforces its own IDENTIFY spacing
   (RESUME attempts exempt; fresh IDENTIFYs spaced ≥90s, i.e. ≤960/day) by sleeping
   inside `Connect` before re-identifying. No engine change; the Supervisor just sees a
   slower failing connection. **[recommendation]**
3. **Close-code triage.** Discord distinguishes resumable, re-identify, and fatal
   (bad-token, invalid-intents) closes. Today every adapter error is uniformly "backoff
   and retry". Fatal auth errors should follow the Telegram 409-conflict precedent
   (`telegram_channel.go`): return unretried with an operator-actionable log, and let the
   next credential rotation restart supervision. **[recommendation]**

Heartbeats (interval from HELLO, ack-watchdog) live entirely inside `Connect`, like
Lark's app-layer ping loop. The ctx→read-interrupt watchdog invariant documented at
`lark/ws_connector.go:41-53` (a blocking `gorilla/websocket` read must be interruptible
by lease loss) is mandatory for the Discord client too.

### 7.2 Privileged intents: design for zero privileged intents

`MESSAGE_CONTENT`, `GUILD_MEMBERS`, and `GUILD_PRESENCES` are privileged **[Discord
docs]**. Critically, *without* `MESSAGE_CONTENT` a bot still receives full message
content for **DMs, messages that @-mention it, and interactions** **[Discord docs]** —
and that is exactly Multica's interaction model: the engine Router group-filters to
@-bot mentions (documented in the Mininglamp blueprint's description of the Router:
dedup, group @bot filtering, identity+member checks), and DMs are p2p sessions.

**Recommendation: declare `GUILD_MESSAGES` + `DIRECT_MESSAGES` + the non-privileged
`MESSAGE_CONTENT`-exempt surface only; do not require any privileged intent.** The
product ceiling this accepts: no ambient listening in guild channels (already a
non-goal), and member-list lookups go through REST on demand rather than the
`GUILD_MEMBERS` intent. Consequences: no verification dependency at any guild count,
BYO-bot installs work on a fresh developer-portal app with zero toggles, and the
fallback question ("what if verification is denied") dissolves. The verification
threshold itself (the prior analysis said 100 guilds; my fetch of the current gateway
doc surfaced a 10,000-**user** figure for privileged-intent review) is a fact I could
not conclusively pin — the support-portal FAQ returned HTTP 403 to automated fetch. It
only matters if a future phase wants ambient listening; parked as open question OQ-2
with a named human owner (§14), since developer-portal work is human-only anyway.

### 7.3 Thread mapping onto `chat_type`

Discord surfaces: DMs; guild text channels; threads (public/private, incl. forum posts).

**Decision (schema-shaped, resolves migration design — no schema change needed):**

| Discord surface | `chat_type` | `channel_chat_id` | binding `config` |
|---|---|---|---|
| DM channel | `p2p` | DM channel id | `{"channel_id": ...}` |
| Guild text channel | `group` | channel id | `{"guild_id", "channel_id"}` |
| Thread / forum post | `group` | the thread's own id | `{"guild_id", "channel_id"}` |

> **CORRECTED 2026-08-28 during implementation (subtask 3.3).** This table originally
> specified a `parent_channel_id:thread_id` composite for threads, copied from the Slack
> and Telegram precedents. That was a false carry-over and is now fixed above.
> Two facts, both verified in implementation:
> 1. **Discord snowflake ids are globally unique across every object type** — a thread id
>    can never collide with a channel id in any guild. Slack and Telegram need composites
>    precisely because their ids are scoped *per chat*; Discord's are not, so the thread id
>    alone already gives perfect session isolation.
> 2. **The composite was not constructible anyway.** A `MESSAGE_CREATE` payload carries
>    `channel_id` (set to the *thread's* id when posted in a thread) but never `parent_id` —
>    Discord puts `parent_id` on the channel object, not the message. Building the composite
>    would have required a REST fetch per unseen channel plus a cache, buying nothing.
>
> Consequence: no separate thread branch exists in the routing code, and a thread and its
> parent channel naturally produce different keys. Verified by
> `TestDiscordSessionRouting_ThreadVsParentChannelIsolation`.

The partial unique active-route index and `/new` route-revision rotation work as-is. Each
thread being its own session matches Slack's per-@bot-thread behavior and is what makes
Discord threads genuinely useful for parallel agent conversations.
Message IDs are already unique per Discord deployment (snowflakes), but the dedup key is
`(installation, message_id)` so no composite is needed there **[inference from
`channel_inbound_message_dedup` usage]**.

### 7.4 Rendering and capabilities

**Declared bits** (mirroring `telegram_channel.go:35-38`'s declaration style):

```
CapText | CapThreadReply | CapQuoteReply | CapAttachment |
CapTypingIndicator | CapMessageEdit
```

- **Not `CapRichCard`**: embeds exist, but the engine's card path is Lark/Slack-shaped;
  v1 degrades rich output to markdown text, which the capability system is explicitly
  designed to let callers do (declaration-only bitmask, `capability.go:5-10`).
- **Not `CapVoice`**.
- **Markdown**: `markdown.go` converts Multica markdown → Discord flavor. Discord
  supports bold/italic/strike/underline, inline+fenced code, block quotes, headers,
  bulleted lists, and masked links for bots; **no tables** — tables degrade to fenced
  code blocks. Telegram's converter (119 lines, placeholder-protected inline code) is the
  template; Discord's flavor is nearer to Multica's source markdown so the converter is
  smaller **[inference]**.
- **2,000-character cap**: chunk at 1,900 with newline-preferring splits, quoting only on
  the first chunk — the `sender.go` `maxMessageUnits`/`chunkMessage` pattern verbatim.
- **Streaming**: placeholder message + throttled `PATCH /channels/{id}/messages/{id}`
  edits, final chunked send on `EventChatDone` — Telegram's `outbound.go` worker shape.
  Discord's per-route REST rate limits are handled by honoring `X-RateLimit-*` /
  `Retry-After` headers in the REST client (precedent: `ghsnapshot`'s Retry-After
  handling; Telegram's 429 `retryAfter()` sleep).
- **Ephemeral interaction responses**: only meaningful with native slash commands —
  deferred with them (§7.5).
- **Outbound transport is stateless REST, never the Gateway socket.** This is the direct
  lesson of open bug #7215 (verified open, filed 2026-08-19): WeCom's only outbound path
  is the in-process WS in `sendersRegistry`, and the in-tree comment at
  `wecom/outbound.go:15-23` documents that on multi-replica deployments the event
  publisher may not hold the socket, silently dropping replies ("Slack/Lark are immune —
  their outbound is stateless HTTP any replica can perform"). Discord has a full REST
  send path, so we take the immune side of that fork by construction.

### 7.5 Slash commands: deferred, with a concrete v1 bridge

No adapter syncs platform-registered commands from code (Slack's `/issue` slash command
is configured in the Slack app console; the handler is `slack/slash_command.go`). Discord
uniquely *requires* REST registration (`PUT /applications/{id}/commands`) for native
commands, and typing `/x` in Discord opens the command picker.

**Decision: defer native command registration to Phase D (post-v1).** v1 relies on
@-mention + DM text, where the engine's existing plain-text directives (`/new`, `/clear`
— channel-generic since #7468, with durable context boundaries per #7362) arrive as
ordinary message content; Discord delivers unmatched `/text` as a plain message, so the
directives keep working **[inference — verify during Phase A manual testing]**. Phase D
scope, when picked up: idempotent guild-command sync on `Connect`, `/new`, `/clear`,
`/issue`, interaction-token replies (ephemeral where sensible), and the
`applications.commands` OAuth scope added to the invite URL.

### 7.6 Install flow and the daemon constants

**Install** mirrors Telegram's pasted-token BYO flow (`telegram/install.go` — not OAuth;
the "OAuth2" part of Discord bot install is only the *invite URL*):

1. Human creates the app + bot in the Discord developer portal, copies the token
   (**human-only**, §9.4).
2. Settings tab: paste token → shape check → live `GET /users/@me` validation (yields
   bot user id + username for mention matching) → classify HTTP 401 as
   `ErrCredentialsRejected` vs anything else as `ErrCredentialsUnverifiable` (Telegram's
   `:181` proxy-outage distinction) → `secretbox.Seal` → upsert `channel_installation`
   keyed `(workspace, agent, 'discord')`, with the Telegram ownership-conflict taxonomy
   (other-workspace / same-workspace / archived-agent / dead-reclaim) on the
   `(channel_type, app_id)` unique index.
3. UI shows the generated invite URL
   (`https://discord.com/oauth2/authorize?client_id=...&scope=bot&permissions=<mask>`);
   the human authorizes it into their guild.
4. User binding: unbound Discord user DMs/mentions the bot → bot replies with a
   `/discord/bind?token=...` link → authenticated redeem, hashed token, 15-min TTL
   (`channel_binding_token`, Telegram's `binding.go` flow unchanged).

**Daemon constants — explicit decisions the Telegram PR missed.** Verified: `telegram`
never made it into `server/internal/daemon/execenv/channel_type.go`, so today
`SurfacePersistsTranscript("telegram")` silently returns false (its transcripts *do*
persist via the shared `AppendUserMessage` path) and `ChannelDisplayName` renders
lowercase "telegram" in prompts. Discord will not repeat this: add
`ChannelTypeDiscord = "discord"`, include it in `SurfacePersistsTranscript` → **true**
(Discord sessions persist via the same shared path), and `ChannelDisplayName` → "Discord".
Filing the Telegram gap upstream is a free-standing goodwill PR (§8.1) **[recommendation]**.

### 7.7 Squad routing parity: verified — there is nothing to reach parity with

Confirmed in code, not the README: Squads is a large feature (416 files; schema 084–090,
096, 127; `handler/squad.go` leader dispatch), but it has **no channel-facing surface**.
The only squad reference in all of `server/internal/integrations/` is Slack's
`slash_command.go:272-279`, which passes a **zero** squad UUID with the comment "no squad
— dispatch straight to the installation agent". Leader delegation is issue/task-layer
(`service/task.go` `SourceDelegation` / `DelegatedFromTaskID`); the leader briefing is
injected into agent instructions (`handler/squad_briefing.go:104-117`), never posted to
an IM. So no parity constraint lands on Discord's capability declaration or thread
mapping. If upstream later gives Squads a channel surface, it will arrive through the
generic engine and Discord inherits it like every other channel.

## 8. Fork strategy

### 8.1 Fork vs. upstream: fork-first, upstream-target

Evidence for each path:

- **Upstream accepts community channels.** `apps/docs/content/docs/channels.mdx:28`
  (verbatim): *"DingTalk, WeCom, and Telegram are community-maintained: they ship in
  every release, but carry no official support SLA."* `community-maintained.mdx` defines
  the tier with a maintainer table (DingTalk @yyclaw; WeCom @leroy-chen, @seacen), and
  Telegram itself entered through a community PR (#6944, leonzone).
- **Upstream expects channels to be developed on forks.** PR #6009's own author closed it
  with "this should target our fork… not upstream", and #6944 arrived fully formed from
  a fork branch.
- **Fork drift is measurably expensive** (§8.2).

**Recommendation: develop on our fork; target the community-maintained tier upstream once
Phases A–C are stable; keep the fork's delta strictly to the Discord surface** (no
netguard/scheduler-style side subsystems, which are what pushed Mininglamp's delta to 161
files and made their sync surface wide). Accepting the community tier means accepting its
explicit terms: we become the named maintainer, with no upstream SLA — this is the
support-tier call that belongs to a human (§9.4, OQ-1).

### 8.2 What drift actually cost Mininglamp-OSS (measured)

Fork head `ccc0d3e546b6c8b256101197eb761f42202d0294` (matches the given sha), merge-base
`aecd47b59` = upstream 2026-07-09. Measured: **133 commits ahead, 1,065 behind** (prior
analysis said ~1,061 — mine is the fetch-day measurement) after ~7 weeks of silence.
Total delta: 161 files, +18,752/−402; Octo IM proper: 56 octo-named files, +9,414
(matches the reference exactly); plus netguard, webhooksign, scheduler, and outbound
webhook subscriptions. Their history quantifies three distinct drift costs:

1. **A dead PR.** Their first convergence attempt (PR #48) went **203 commits stale and
   was discarded wholesale** — the replacement blueprint (recovered from
   `docs/octo-channel-convergence-blueprint.md`, deleted in #59) opens with a table of
   five assumptions main had invalidated, including the engine having moved directories
   and Slack having grown four features.
2. **The migration renumber.** Octo's schema landed as migration **120**; upstream's sync
   brought its own 120–148, forcing a 120–123→149–152 renumber plus a
   `runOctoWebhookRenumberHook` pre-migration reconcile hook for already-deployed
   databases, plus repair commits (`647d87abe`, `f3abd5410` restoring a lost CHECK
   member, `8788773a7` adding 153 to restore `octo_chat`, `6f73bd8c4` tests,
   `45a835063` docs) — **five commits and an upgrade-path risk to fix a numbering
   choice**. (The prior analysis' "12 migrations" is 12 *files* = 6 up/down pairs,
   149–154.)
3. **The convergence rewrite itself.** The initial Octo build (2026-06-10/11, a WBS-driven
   burst by essentially one contributor, lml2468) created **seven parallel `octo_*`
   tables and its own WS-lease/dispatch pipeline** instead of building on the
   then-existing `channel_*` engine. PR #57 ("converge Octo IM onto the shared
   channel.Channel engine") a month later folded all seven tables into `channel_*` with
   `channel_type='octo'` (migration 154, both upgrade paths verified, down-migration
   round-trips). **That rewrite is the mistake this project skips**: Discord builds on
   `channel.Channel` and the generic tables from the first commit.

Their sequencing discipline (#55/#56 sync PRs — two in one day — then #57 convergence,
#58 selfhost env forwarding, #59 doc cleanup; sync PRs named `chore/sync-upstream-<date>`)
is worth copying; their post-#59 sync stop (0 syncs in 7 weeks against a repo moving
~150 commits/week and ~6 migrations/day) is the anti-pattern.

The contrasting fork, `code2rich/multica` (head `486e6dfcc` matches the given sha;
merge-base 2026-07-17; 44 ahead, **850 behind**; 668 files, +29,305/−29,193): they
deleted `apps/desktop` entirely (137 files) and built an `agentwaker` subsystem (53
files touching it; 34 migration files = 17 pairs — the prior "40 migrations" figure did
not reproduce). Scope *reduction* bought them a smaller sync surface per week but a
permanently divergent tree that can never upstream. Relevant only if we wanted to drop
surfaces (we don't — Discord needs the web settings UI, and desktop shares it via
`packages/views`).

### 8.3 Sync cadence, branch model, migration numbering, conflict forecast

**[recommendation, grounded in the measurements above]**

- **Branch model.** `main` tracks upstream `main` plus our delta; feature branches per
  phase; sync via `chore/sync-upstream-<date>` merge PRs (Mininglamp's naming), **at
  least weekly** — at ~150 upstream commits/week, a weekly sync is ~1/7th the conflict
  mass that killed their PR #48. Every sync PR must pass `make check` + `pnpm typecheck`
  + Go/TS suites before merge.
- **Conflict surface forecast.** Our steady-state delta is the §5.2 file list. The
  genuinely contested files are `router.go`, `handler.go`, `events.go`,
  `integrations-tab.tsx`, `use-realtime-sync.ts`, and the locale JSONs — all
  append-a-block edits where conflicts are frequent but mechanical. The adapter package
  itself conflicts only when the engine contract moves (once since June: the
  UUIDv7 PK migration #7230 touched all channels). Expect **minutes-to-an-hour per
  weekly sync, spiking when `channel/engine` refactors land**; a human arbitrates
  rebase conflicts in contested files (§9.4).
- **Migration numbering.** Upstream burned 291 numbers in the 7 weeks to HEAD-439
  (~6/day, gaps allowed — verified gaps at 069→072 … 432→437; a lint test exists at
  `server/internal/migrations/migrations_lint_test.go`). A "next free number" choice is
  guaranteed to collide within days. **Plan: reserve a far-offset block `9NN`
  (`900_issue_origin_discord_chat` + `901_..._validate`) for fork-local migrations.**
  Numbering is lexicographic-ordered by the runner and non-contiguity is demonstrably
  tolerated; whether the lint test caps gap size must be checked in Phase A (task in
  §10) — if it forbids the offset, fall back to Mininglamp's proven renumber-hook
  pattern, budgeted at their measured five-commit cost. On upstreaming, the block is
  renumbered once, cleanly, to upstream's then-next numbers — a fresh-install-only
  event for upstream, with our fork keeping a reconcile hook for our own deployed
  databases.
- **If upstream ships Discord independently.** No signal today (§1). If it happens:
  converge, don't compete — adopt upstream's adapter, write one migration mapping our
  `config` JSONB field names and binding composites onto theirs (the #57 playbook, whose
  down-migration round-trip is the safety standard), and re-target our remaining deltas
  as upstream fixes. This is the strongest argument for upstreaming early: it converts
  the risk from "rewrite" to "already merged".

## 9. Execution and delegation model

### 9.1 Step 0 inventory — what this environment actually has (verified 2026-08-27)

- **Subagents**: 46 definition files, present identically at project level
  (`/workspace/.claude/agents/`) and user level (`~/.claude/agents/`) — the VoltAgent
  catalog (each with Read/Write/Edit/Bash/Glob/Grep): `golang-pro`, `sql-pro`,
  `typescript-pro`, `react-specialist`, `nextjs-developer`, `javascript-pro`,
  `security-engineer`, `deployment-engineer`, `docker-expert`, `database-administrator`,
  plus ~36 stack specialists irrelevant here (Rails, Flutter, PowerShell, …) and one
  bespoke `k8s-homelab`. Built-in agent types additionally available: **Explore**
  (read-only, fan-out search), **Plan**, **general-purpose** (full tools), **claude**
  (catch-all, full tools). A `voltagent-research` plugin (v1.0.3) adds read-only
  research agents (`research-analyst`, `search-specialist`, `competitive-analyst`, …)
  with WebFetch/WebSearch.
- **Slash commands**: none — `.claude/commands/` does not exist at project, workspace, or
  user level.
- **Skills**: `graphify` (user-installed; `graphify-out/` exists for this project but is
  empty — no graph built). Harness-provided skills: `code-review` (multi-level review,
  incl. cloud "ultra"), `security-review`, `simplify`, `run`, `loop`, `schedule`,
  `dataviz`, `claude-api`, `init`, `update-config`.
- **MCP servers**: `github` (full API, authenticated as `fulviofreitas` — used to verify
  every PR/issue claim in this document), `context7` (live library docs — the designated
  path for Discord API doc lookups during implementation), **`discord`** (a ~90-tool
  Discord management server: create/read channels, threads, webhooks, messages,
  reactions — meaning parts of live-guild verification are automatable, §9.3), and a
  `bgpt` paper-search server (irrelevant).
- **Project instructions**: root `CLAUDE.md` (259 lines, authoritative — DB rules,
  package boundaries, state rules, testing layers), `AGENTS.md` (pointer doc),
  `apps/mobile/CLAUDE.md` (mobile-only; Discord does not touch mobile),
  `CONTRIBUTING.md` (621 lines — notably full worktree tooling).
- **Parallelism mechanics**: confirmed. The Agent tool runs concurrent background
  subagents (three ran in parallel to produce this PRD's research, 50k–89k tokens each);
  agents can be isolated in git worktrees. The repo itself is built for this:
  `CONTRIBUTING.md` has dedicated sections for worktree env files, `make worktree-env` /
  `setup-worktree` / `start-worktree` / `remove-worktree`, per-worktree isolated
  databases sharing one PostgreSQL container, and "Running Main and Worktree at the Same
  Time". Host has 8 CPUs — practical ceiling ~6 concurrent agents.
- **Context budget**: the repo is 5,344 files / 468 migration pairs; the three read-only
  survey agents each consumed 50k–89k tokens *summarizing single subsystems*. One
  context cannot hold the channel engine + an adapter + the frontend + migration history
  simultaneously. Hence the structure below: one integrating agent, specialists per
  workstream, and written handoff contracts instead of shared context.

### 9.2 Workstream → agent mapping

| # | Workstream | Executor (confirmed to exist) | Tools | Parallel? | Hands off |
|---|---|---|---|---|---|
| W1 | Recon per subsystem (engine, schema, frontend) — *done for this PRD* | `Explore` agents ×3 | read-only | parallel | written subsystem reports (this doc's §5–§7 sources) |
| W2 | Migration pair + sqlc queries + lint-gap check | `sql-pro` | R/W/E/Bash | **serial, first** — everything downstream reads the schema | applied migrations + regenerated `server/pkg/db` via `make sqlc`; column contract note |
| W3 | Discord adapter Go package (gateway, inbound, install, resolvers) + its tests | `golang-pro` | R/W/E/Bash | after W2; parallel with W5 | compiling package + green `make test`; frozen JSON API response shapes for W5 |
| W4 | Outbound path (sender, markdown, streaming edits, replier) + tests | `golang-pro` (second engagement, own worktree) | R/W/E/Bash | after W3's channel skeleton | green tests incl. chunking/edit-throttle matrices |
| W5 | `packages/core` types + zod schemas + api client + queries + realtime map | `typescript-pro` | R/W/E/Bash | parallel with W3/W4 once shapes frozen | typechecked package + malformed-response tests (repo rule) |
| W6 | Settings tab, icon, bind page, `apps/web` route | `react-specialist` | R/W/E/Bash | after W5 | `pnpm typecheck` + views tests green |
| W7 | Handler endpoints + router wiring + env gating | `golang-pro` (with W3) | R/W/E/Bash | with W3 | `testutil.Call`-style handler tests |
| W8 | Four-locale docs + locale JSONs + daemon execenv constants | **primary agent** | all | last | docs build; conventions.mdx glossary respected |
| W9 | Code review per phase | `code-review` skill (+ `security-review` skill on install/token paths) | — | per-PR | findings applied |
| W10 | Discord API fact-checking during impl | `context7` MCP + `voltagent-research:search-specialist` | read-only | on demand | cited notes in PR descriptions |
| W11 | Live-guild scripted checks (channel/thread/webhook fixtures, message readback) | `discord` MCP server driven by primary agent | MCP | Phase B+ | manual-test transcript attached to PR |

Rejections, per the "name it or say why not" rule: `nextjs-developer` was rejected for
W6 because the shared UI lives in `packages/views`, where `next/*` imports are banned
(CLAUDE.md hard rule) — `react-specialist` fits; the thin `apps/web/app/discord/bind/`
route is a one-file wiring task the same agent handles. `database-administrator` was
rejected for W2 (ops/HA focus) in favor of `sql-pro`. No documentation-specialist or
dedicated test agent exists in this environment: docs (W8) fall to the primary agent,
and tests are written by the same agent as the code, which is also what the repo's
"tests follow the code" table in CLAUDE.md prescribes. No suitable agent is missing
badly enough to justify defining a new one — the gap that *would* justify one (a
Discord-protocol specialist) is better served by W10's doc-lookup path than by a new
static prompt.

### 9.3 Parallelism and handoff contracts

Worktree per concurrently-active workstream (`make worktree-env` + agent worktree
isolation; per-worktree DB names/ports remove migration and dev-server contention —
verified repo capability, not an assumption). Maximum useful width here is three lanes
(backend Go / core TS / frontend views) plus the primary agent integrating — well under
the 6-agent host ceiling. Handoffs are written artifacts, not shared context: W2
publishes the schema note; W3 freezes the handler JSON shapes *before* W5 starts (the
zod schemas are the contract, and the repo's malformed-response-test rule enforces it);
W6 consumes only W5's exported types. Merges land serially through the primary agent in
phase order.

### 9.4 Human-only list

Not delegable to any agent in this environment:

1. **Discord developer-portal work**: creating the application/bot, custody of the bot
   token, any privileged-intent toggle, and any verification submission.
2. **The privileged-intent decision** itself (§7.2 recommends "none", but accepting the
   ambient-listening ceiling is a product call) — OQ-2.
3. **Live Gateway handshake debugging against a real guild** — requires the real token,
   portal access, and judgment over live traffic. (The `discord` MCP server can script
   fixture setup and message readback around it — W11 — but not replace it.)
4. **Rebase conflict arbitration** on weekly syncs when contested files (§8.3) conflict
   non-mechanically.
5. **The support-tier call**: signing up as named maintainer under
   `community-maintained.mdx` terms — OQ-1.
6. **Secret provisioning**: generating and installing `MULTICA_DISCORD_SECRET_KEY` in
   every deployment target.

## 10. Delivery plan

Four PRs on the fork (a single PR is unacceptable internally; the ~9k-line #6944 shape is
reserved for the eventual upstream consolidation, §4.2). The abandoned upstream PR #6009
(21 files, +4,070, "backend inbound core") is the empirically right-sized first slice.

- **Phase A — inbound core** (W2, W3, W7): migration pair (issue-origin widening),
  adapter package with Gateway client (IDENTIFY/HELLO/heartbeat/RESUME cache, close-code
  triage), inbound normalization, resolvers, install service + handler endpoints +
  router.go env-gated block behind `MULTICA_DISCORD_SECRET_KEY`. Includes the
  migration-lint gap-size check (§8.3). Exit: mention/DM to the bot creates a session
  and reaches an agent; replies not yet delivered.
- **Phase B — outbound** (W4, W11): REST sender with chunking + rate-limit header
  handling, markdown converter, streaming edit worker, replier, typing indicator.
  Deliberately its own reviewed PR because outbound delivery is the fragile seam — the
  evidence is #7215 (WeCom replies generated but never pushed, silently) and the test
  weighting in §11. Exit: full round-trip conversation in DM, guild channel, and thread.
- **Phase C — product surface** (W5, W6): core types/schemas/client/queries, realtime
  invalidation, settings tab + icon + integrations-tab section, bind page + web route.
  Exit: install/revoke/bind entirely from the UI.
- **Phase D — polish + docs** (W8): four-locale docs (`channels*`,
  `discord-bot-integration*`, `environment-variables*`, `meta*.json`, locale JSONs),
  daemon execenv constants (§7.6), selfhost env forwarding
  (`docker-compose.selfhost.yml` — the thing Mininglamp only caught in their #58).
  Native slash commands are a separate post-v1 Phase D2 (§7.5).
- **Phase E — upstreaming**: squash A–D into one #6944-shaped PR against
  `multica-ai/multica`, renumber the migration pair, propose the community-maintained
  tier entry, and file the free-standing Telegram execenv gap fix (§7.6) as a
  credibility-building opener.

## 11. Testing strategy

Telegram's measured test share is **2,690 of 6,193 lines (43%)** — the prior "~2.6k of
~6.2k" figure confirmed — and its heaviest single file is `outbound_test.go` at 1,366
lines, i.e. the shipped precedent already concentrates tests on the outbound seam that
#7215 proved fragile. Discord matches that weighting and adds:

- **Gateway lifecycle table tests** (golang-pro, Phase A): HELLO→heartbeat cadence,
  seq tracking, RESUME-vs-IDENTIFY choice per close code, IDENTIFY spacing floor
  (the 1,000/day budget math from §7.1 as an explicit test), lease-loss ctx-cancel
  interrupting a blocked read (the `ws_connector.go:41-53` invariant).
- **Outbound matrices** (Phase B): 2,000-cap chunking incl. code-fence-spanning splits,
  markdown conversion cases, 429/`Retry-After` behavior, edit-throttle coalescing,
  multi-replica safety (outbound must succeed with no Gateway socket present — the
  anti-#7215 test).
- **Handler tests** via `testutil.Call(...).Want(...)` fixtures — CLAUDE.md forbids
  open-coded recorder/INSERT quartets.
- **Frontend**: zod schemas get malformed-response tests (repo rule); the settings tab
  and bind page follow the views-test conventions (no `next/*` mocks; callable-store
  Zustand mocks).
- **Migration tests**: both paths of the pair, and (if the offset-block plan survives the
  lint check) a lint-suite run proving 9NN numbering passes.
- **Live smoke**: human + W11 scripted fixtures against a real guild; kept out of default
  test runs, mirroring the `agentintegration` build-tag pattern for real-agent smokes.

## 12. Effort estimate and staffing

**[inference from measured comparables]** Telegram: 83 files, +9,196, shipped by one
experienced community contributor. Octo IM: ~9,400 lines in an initial ~2-day WBS burst
by essentially one engineer — followed by a month-later convergence rewrite we architect
away. Discord sits between Telegram and Slack in complexity: Telegram-sized surface plus
a Gateway state machine, minus Slack's SDK dependency. Estimate: **adapter ~4.5–6k Go
lines (~45% tests), core+frontend ~1.5–2k TS lines, docs ×4 locales**, total **4–6
calendar weeks** for one engineer directing the §9 agent fleet, with the human-only items
(§9.4) on the critical path at Phase A start (portal/token) and Phase B (live guild).
Ongoing: ~1–2 hours/week of sync maintenance (§8.3) for the life of the fork.

## 13. Risks and mitigations

| Risk | Evidence | Mitigation |
|---|---|---|
| Fork drift kills a long-lived branch | Mininglamp PR #48 dead at 203 commits stale; upstream moves ~150 commits/wk | Weekly syncs; four small phase PRs, none living >2 weeks |
| Migration number collision | Mininglamp's 120→149 renumber: 5 commits + upgrade-path bug risk; upstream burns ~6 numbers/day | 9NN offset block; lint check in Phase A; renumber-hook fallback is a proven pattern |
| Outbound silently drops replies | #7215 open; in-tree single-replica constraint comment in `wecom/outbound.go` | Stateless REST outbound (structurally immune); anti-#7215 test in §11 |
| IDENTIFY budget exhaustion resets the bot token | 1,000/day **[Discord docs]** vs. worst-case ~1,440 reconnects/day at 60s backoff cap | Adapter-level ≥90s IDENTIFY spacing; RESUME-first; close-code triage |
| Upstream ships Discord first | No signal today (verified issue/PR search) | Upstream early (Phase E); #57-style convergence playbook if it happens anyway |
| Community-tier support burden | `channels.mdx` tier = named maintainer, no SLA; WeCom's maintainers now own #7215-class triage | OQ-1 decided *before* Phase E, not after |
| Engine contract refactors under us | UUIDv7 PK migration #7230 touched every adapter | Adapter keeps zero engine patches; weekly sync catches contract moves at ~1-week granularity |

## 14. Open questions (named owners)

- **OQ-1 (owner: project owner / f@freitas.app):** Do we sign up as the named
  community-tier maintainer for Discord upstream, with the support expectations
  `community-maintained.mdx` spells out? Decide before Phase E.
- **OQ-2 (owner: project owner, at the developer portal, Phase A):** Confirm the exact
  current `MESSAGE_CONTENT` verification threshold (100 guilds per the prior analysis;
  the current gateway doc's privileged-intent passage reads as 10,000+ users; the
  authoritative support-portal FAQ blocked automated fetch). Only binding if we ever
  revisit the zero-privileged-intents decision (§7.2).
- **OQ-3 (owner: sql-pro task, Phase A):** Does `migrations_lint_test.go` tolerate the
  9NN offset block? If not, adopt the renumber-hook fallback (§8.3).
- **OQ-4 (owner: golang-pro task, Phase A manual test):** Verify Discord delivers
  unregistered `/new`-style text as a plain message (the v1 directive bridge, §7.5).
- **OQ-5 (owner: primary agent, Phase E):** Whether upstream wants the Telegram execenv
  gap fix (§7.6) folded into the Discord PR or standalone — ask the maintainers.

---

## Appendix: verification notes and corrected reference figures

Discrepancies between the prior analysis' starting figures and what I measured at
`3d37828e9` / fork heads on 2026-08-27 — my measurements govern:

| Claim | Measured |
|---|---|
| Telegram staged as #6009 then #6944 | #6009 **closed unmerged** ("target our fork"); #6944 shipped everything in one commit (`67c6aa05c`, 83 files, +9,196) |
| DingTalk ~125 / WeCom ~80 / Telegram ~62 external refs | 131 / 89 / 66 (methodology in §4.1) |
| Adapter sizes ~6.5k/8.4k/9.9k/18.9k/21.9k; Lark 65 files | 6,193 / 8,284 / 9,582 / 18,638 / 22,137; Lark 66 files |
| Mininglamp "~1,061 commits behind" | 1,065 behind, 133 ahead (merge-base `aecd47b59`, 2026-07-09) |
| Mininglamp "12 migrations" | 12 files = 6 up/down pairs (149–154) |
| Octo IM ~56 files / ~9,414 lines; webhook subsystem ~48 files +6,871 | 56 / +9,414 confirmed exactly; webhook subsystem not measured separately (bundled in the 161-file, +18,752 total) |
| code2rich: deleted desktop 137 files; agentwaker 40 files / 40 migrations | 137 confirmed; agentwaker touches 53 files; 34 migration files = 17 pairs |
| Squads "verify channel-facing surface" | Definitively none (§7.7) |
| `MESSAGE_CONTENT` gated past 100 guilds | Could not re-verify the exact threshold → OQ-2; moot under the §7.2 design |

Sources: local partial clone with full history (5,047 commits) + `mininglamp` and
`code2rich` remotes; GitHub API (authenticated) for PR/issue metadata; docs.discord.com
gateway page fetch (2026-08-27). Note on partial-clone hygiene, per the ancestry
warning: all fork ancestry was established with `git merge-base` / `git rev-list
--count`, never `git cat-file -e`.
