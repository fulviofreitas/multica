/** A Discord bot installation bound to a single Multica agent.
 *
 * Wire shape mirrors `DiscordInstallationResponse` in
 * `server/internal/handler/discord.go`. The encrypted bot token in config is
 * INTENTIONALLY absent from this type — it is server-internal and never
 * leaves the backend. New fields the backend adds in the future MUST default
 * to optional so older desktop builds keep parsing the response — see
 * CLAUDE.md → API Compatibility. */
export interface DiscordInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  /** The Discord application's id (surfaced by the Go handler as `app_id`;
   * this is the bot's Discord application/client id, used to build the
   * OAuth2 "add bot to server" invite link). */
  app_id: string;
  /** The bot's Discord username. */
  bot_username: string;
  installer_user_id: string;
  status: "active" | "revoked" | string;
  installed_at: string;
  created_at: string;
  updated_at: string;
  /** The Discord OAuth2 "add bot to server" invite link. Empty when the
   * stored app id is empty (defensive; a real install never produces this). */
  invite_url: string;
}

export interface ListDiscordInstallationsResponse {
  installations: DiscordInstallation[];
  /** Whether the deployment has MULTICA_DISCORD_SECRET_KEY configured. When
   * false the connect entry points are hidden and the panel renders a
   * disabled state instead of erroring. */
  configured: boolean;
}

/** Request body for a bot install: the token pasted from the Discord
 * developer portal's Bot page, and the agent it is installed for. The
 * backend validates it live before persisting. */
export interface RegisterDiscordRequest {
  bot_token: string;
  agent_id: string;
}
