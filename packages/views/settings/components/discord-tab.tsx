"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ChevronRight, ExternalLink, Trash2 } from "lucide-react";
import { DiscordMark } from "./discord-mark";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  agentListOptions,
  memberListOptions,
} from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import { discordInstallationsOptions, discordKeys } from "@multica/core/discord";
import { api, ApiError } from "@multica/core/api";
import type { DiscordInstallation } from "@multica/core/types";
import { ActorAvatar } from "../../common/actor-avatar";
import { openExternal } from "../../platform";
import { useT } from "../../i18n";

// DiscordTab is the workspace settings panel for Discord bot installations.
// Listing is member-visible; connect/disconnect are admin-only (backend-
// enforced; the UI hides the buttons for non-admins to match).
//
// Unlike Lark/Slack/DingTalk/Telegram (whose only install entry point is the
// Agent detail page's bind button, because asking the user to pick an agent
// in Settings would re-create that page's picker), Discord ALSO gets a
// top-level "Connect Discord" button here: the docs
// (discord-bot-integration.mdx) document Settings -> Integrations -> Discord
// -> Connect Discord as the primary flow, and the register endpoint takes
// agent_id in the request body rather than being reached through the agent
// page, so the agent picker naturally lives in the dialog instead of the URL.
// DiscordAgentBindButton below still exists for the agent detail page and
// reuses the same dialog with a fixed agent id (no picker).
export function DiscordTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  const { data, isLoading, isError } = useQuery({
    ...discordInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const installations = data?.installations ?? [];
  const configured = data?.configured === true;

  const [connectOpen, setConnectOpen] = useState(false);
  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleDisconnect() {
    if (!disconnectTarget || disconnecting) return;
    setDisconnecting(true);
    try {
      // Await the server before touching cache/UI (repo rule: no optimistic
      // removal on flows that confirm/destroy).
      await api.deleteDiscordInstallation(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: discordKeys.installations(wsId) });
      toast.success(t(($) => $.discord.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.discord.toast_disconnect_failed),
      );
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div className="space-y-8">
      {isError ? (
        <Card>
          <CardContent>
            <p className="text-body text-muted-foreground">
              {t(($) => $.discord.load_failed)}
            </p>
          </CardContent>
        </Card>
      ) : isLoading ? (
        <Card>
          <CardContent>
            <p className="text-body text-muted-foreground">{t(($) => $.discord.loading)}</p>
          </CardContent>
        </Card>
      ) : !configured ? (
        // MULTICA_DISCORD_SECRET_KEY is unset on this deployment. Rendered as
        // a disabled/informational state, never as an error — the server
        // returns configured:false deliberately, not as a failure.
        <Card>
          <CardContent className="space-y-2">
            <p className="text-body font-medium">{t(($) => $.discord.not_enabled_title)}</p>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.discord.not_enabled_description_prefix)}{" "}
              <code className="rounded bg-muted px-1 py-0.5 text-micro">
                MULTICA_DISCORD_SECRET_KEY
              </code>{" "}
              {t(($) => $.discord.not_enabled_description_suffix)}{" "}
              {t(($) => $.discord.not_enabled_self_host_hint)}
            </p>
          </CardContent>
        </Card>
      ) : (
        <section className="space-y-3">
          <div className="flex items-center justify-between gap-3">
            <h2 className="text-body font-semibold">{t(($) => $.discord.connected_bots)}</h2>
            {canManage && (
              <Button
                variant="outline"
                size="sm"
                onClick={() => setConnectOpen(true)}
                data-testid="discord-connect-open"
              >
                <DiscordMark className="h-3 w-3" />
                {t(($) => $.discord.connect_button)}
              </Button>
            )}
          </div>
          {installations.length === 0 ? (
            <Card>
              <CardContent className="space-y-2">
                <p className="text-body font-medium">{t(($) => $.discord.empty_title)}</p>
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.discord.empty_description_prefix)}{" "}
                  <strong>{t(($) => $.discord.empty_description_cta)}</strong>{" "}
                  {t(($) => $.discord.empty_description_suffix)}
                </p>
              </CardContent>
            </Card>
          ) : (
            <Card>
              <CardContent className="divide-y">
                {installations.map((inst) => (
                  <InstallationRow
                    key={inst.id}
                    installation={inst}
                    canManage={canManage}
                    onDisconnect={() => setDisconnectTarget(inst.id)}
                  />
                ))}
              </CardContent>
            </Card>
          )}
        </section>
      )}

      <DiscordConnectDialog open={connectOpen} onOpenChange={setConnectOpen} />

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.discord.disconnect_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.discord.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.discord.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting
                ? t(($) => $.discord.disconnecting)
                : t(($) => $.discord.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function InstallationRow({
  installation,
  canManage,
  onDisconnect,
}: {
  installation: DiscordInstallation;
  canManage: boolean;
  onDisconnect: () => void;
}) {
  const { t } = useT("settings");
  const { getAgentName } = useActorName();
  const isActive = installation.status === "active";
  const agentName = getAgentName(installation.agent_id);
  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="flex items-start gap-3">
        <ActorAvatar
          actorType="agent"
          actorId={installation.agent_id}
          size="lg"
          enableHoverCard
          profileLink
        />
        <div className="space-y-1">
          <p className="text-body font-medium">
            {agentName}
            {installation.bot_username ? (
              <span className="ml-2 text-caption text-muted-foreground">
                @{installation.bot_username}
              </span>
            ) : null}
            {!isActive && (
              <span className="ml-2 rounded bg-muted px-1.5 py-0.5 text-micro text-muted-foreground">
                {t(($) => $.discord.revoked_badge)}
              </span>
            )}
          </p>
          <p className="text-micro text-muted-foreground">
            {t(($) => $.discord.installed_at_label, {
              when: new Date(installation.installed_at).toLocaleString(),
            })}
          </p>
          {isActive && installation.invite_url && (
            <button
              type="button"
              onClick={() => openExternal(installation.invite_url)}
              className="inline-flex items-center gap-1 text-caption text-muted-foreground underline-offset-2 transition-colors hover:text-foreground hover:underline"
              title={t(($) => $.discord.invite_button_tooltip)}
              data-testid="discord-invite-link"
            >
              <ExternalLink className="h-3 w-3" />
              {t(($) => $.discord.invite_button)}
            </button>
          )}
        </div>
      </div>
      {canManage && isActive && (
        <Button variant="outline" size="sm" onClick={onDisconnect}>
          <Trash2 className="h-3 w-3" />
          {t(($) => $.discord.disconnect)}
        </Button>
      )}
    </div>
  );
}

// discordDocsUrl points at the Discord integration guide on the docs site,
// localized like the Telegram/Slack docs links.
function discordDocsUrl(lang: string | undefined): string {
  const prefix = lang?.startsWith("zh")
    ? "/zh"
    : lang?.startsWith("ja")
      ? "/ja"
      : lang?.startsWith("ko")
        ? "/ko"
        : "";
  return `https://multica.ai/docs${prefix}/discord-bot-integration`;
}

// discordInstallFailureClass classifies a failed install by the server's HTTP
// status (discordInstallErrorResponse in server/internal/handler/discord.go),
// not by pattern-matching the message text: 400 covers both "malformed
// token" and "Discord rejected this token", 409 covers all three
// ownership-conflict cases (same workspace/different agent, archived agent,
// different workspace), and 503 means Discord could not be reached to
// verify. Each class renders under its own title so a bad token and an
// already-connected-elsewhere conflict read as clearly different failures,
// not just different sentences.
type DiscordInstallFailureClass = "bad_token" | "conflict" | "unavailable" | "unknown";

function discordInstallFailureClass(err: unknown): DiscordInstallFailureClass {
  if (!(err instanceof ApiError)) return "unknown";
  switch (err.status) {
    case 400:
      return "bad_token";
    case 409:
      return "conflict";
    case 503:
      return "unavailable";
    default:
      return "unknown";
  }
}

// DiscordConnectDialog is the shared paste-token install form. When
// `fixedAgentId` is set (the agent detail page's bind button) the agent is
// already known and no picker renders — mirrors TelegramAgentBindButton's
// dialog. When omitted (the Settings tab's top-level "Connect Discord"
// button) the admin must pick which agent the bot is for, since
// RegisterDiscordRequest carries agent_id in the body instead of being
// reached through the agent page URL.
function DiscordConnectDialog({
  open,
  onOpenChange,
  fixedAgentId,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  fixedAgentId?: string;
}) {
  const { t, i18n } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();

  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [botToken, setBotToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [errorClass, setErrorClass] = useState<DiscordInstallFailureClass | null>(null);
  const [errorMessage, setErrorMessage] = useState("");

  const { data: agents = [] } = useQuery({
    ...agentListOptions(wsId),
    enabled: !!wsId && open && !fixedAgentId,
  });
  // Archived agents cannot run, so offering them as an install target would
  // only produce a dead bot; the picker only lists agents someone could
  // actually message.
  const availableAgents = agents.filter((a) => !a.archived_at);

  const agentId = fixedAgentId ?? selectedAgentId ?? "";

  function closeDialog() {
    if (submitting) return;
    onOpenChange(false);
    setBotToken("");
    setSelectedAgentId(null);
    setErrorClass(null);
    setErrorMessage("");
  }

  async function handleSubmit() {
    const bot_token = botToken.trim();
    if (submitting || !agentId || !bot_token) return;
    setSubmitting(true);
    setErrorClass(null);
    setErrorMessage("");
    try {
      const installation = await api.registerDiscordBot(wsId, {
        bot_token,
        agent_id: agentId,
      });
      if (!installation.id || installation.status !== "active") {
        throw new Error("Discord connection returned an invalid installation");
      }
      // The discord_installation realtime event also refreshes this list,
      // but invalidate explicitly so the new row appears immediately.
      await qc.invalidateQueries({ queryKey: discordKeys.installations(wsId) });
      toast.success(t(($) => $.discord.connect_success_toast));
      closeDialog();
    } catch (e) {
      // A toast alone would collapse "bad token" and "already connected to
      // another workspace" into the same shape; classify by HTTP status and
      // keep the dialog open with an inline panel titled per class, with the
      // server's already-specific message underneath (see
      // discordInstallFailureClass above).
      setErrorClass(discordInstallFailureClass(e));
      setErrorMessage(
        e instanceof Error ? e.message : t(($) => $.discord.connect_failed_toast),
      );
    } finally {
      setSubmitting(false);
    }
  }

  const errorTitle =
    errorClass === "bad_token"
      ? t(($) => $.discord.connect_error_bad_token_title)
      : errorClass === "conflict"
        ? t(($) => $.discord.connect_error_conflict_title)
        : errorClass === "unavailable"
          ? t(($) => $.discord.connect_error_unavailable_title)
          : t(($) => $.discord.connect_error_unknown_title);

  const canSubmit = agentId !== "" && botToken.trim() !== "" && !submitting;

  return (
    <Dialog
      open={open}
      onOpenChange={(v) => (v ? onOpenChange(true) : closeDialog())}
    >
      <DialogContent className="sm:max-w-lg" data-testid="discord-connect-dialog">
        <DialogHeader>
          <DialogTitle>{t(($) => $.discord.connect_dialog_title)}</DialogTitle>
        </DialogHeader>

        <p className="text-caption text-muted-foreground">
          {t(($) => $.discord.connect_dialog_description)}
        </p>

        <button
          type="button"
          onClick={() => openExternal(discordDocsUrl(i18n.language))}
          className="inline-flex w-fit items-center gap-2 text-body font-medium text-primary underline-offset-2 hover:underline"
          data-testid="discord-docs-link"
        >
          <ExternalLink className="h-4 w-4" />
          {t(($) => $.discord.connect_docs_link)}
        </button>

        {!fixedAgentId && (
          <div className="space-y-1.5">
            <Label htmlFor="discord-agent-select">
              {t(($) => $.discord.agent_select_label)}
            </Label>
            <Select
              items={availableAgents.map((a) => ({ value: a.id, label: a.name }))}
              value={selectedAgentId}
              onValueChange={(v) => setSelectedAgentId(v as string | null)}
              disabled={submitting}
            >
              <SelectTrigger id="discord-agent-select" data-testid="discord-agent-select">
                <SelectValue
                  placeholder={t(($) => $.discord.agent_select_placeholder)}
                />
              </SelectTrigger>
              <SelectContent>
                {availableAgents.map((a) => (
                  <SelectItem key={a.id} value={a.id}>
                    {a.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        )}

        <div className="space-y-1.5">
          <Label htmlFor="discord-bot-token">{t(($) => $.discord.bot_token_label)}</Label>
          <Input
            id="discord-bot-token"
            data-testid="discord-bot-token"
            type="password"
            value={botToken}
            onChange={(e) => setBotToken(e.target.value)}
            // Discord bot-token prefix: a format hint, not copy.
            // eslint-disable-next-line no-restricted-syntax
            placeholder="MTA1M...jY2"
            autoComplete="off"
            spellCheck={false}
            disabled={submitting}
          />
        </div>

        {errorClass && (
          <div
            role="alert"
            className="space-y-0.5 rounded-md border border-destructive/30 bg-destructive/5 p-3"
            data-testid="discord-connect-error"
          >
            <p className="text-caption font-medium text-destructive">{errorTitle}</p>
            <p className="text-caption text-muted-foreground">{errorMessage}</p>
          </div>
        )}

        <DialogFooter>
          <Button
            variant="outline"
            size="sm"
            onClick={closeDialog}
            disabled={submitting}
          >
            {t(($) => $.discord.connect_cancel)}
          </Button>
          <Button
            size="sm"
            onClick={handleSubmit}
            disabled={!canSubmit}
            data-testid="discord-connect-submit"
          >
            {submitting
              ? t(($) => $.discord.connect_submitting)
              : t(($) => $.discord.connect_submit)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// DiscordAgentBindButton is the per-agent CTA on the agent detail page,
// mirroring TelegramAgentBindButton/SlackAgentBindButton. Discord uses the
// paste-a-token model: the admin creates a bot application in the Discord
// developer portal and pastes its token; the backend validates it live via
// the Discord API before persisting.
export function DiscordAgentBindButton({
  agentId,
  agentName,
  className,
  onShowConnectedDetails,
}: {
  agentId: string;
  agentName?: string;
  className?: string;
  /** Compact read-only connected row that invokes this instead of the full
   * badge — the agent inspector passes a "jump to the Integrations tab"
   * handler so management actions live in one place. */
  onShowConnectedDetails?: () => void;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);

  const [dialogOpen, setDialogOpen] = useState(false);

  const { data: listing } = useQuery({
    ...discordInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const configured = listing?.configured === true;

  const { data: members = [] } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  if (!canManage) return null;

  const existing = listing?.installations.find(
    (inst) => inst.agent_id === agentId && inst.status === "active",
  );
  if (existing) {
    return onShowConnectedDetails ? (
      <DiscordAgentBotStatusRow
        onClick={onShowConnectedDetails}
        className={className}
      />
    ) : (
      <DiscordAgentBotConnectedBadge installation={existing} className={className} />
    );
  }

  if (!configured) return null;

  return (
    <div
      className={cn("flex flex-wrap items-center gap-2", className)}
      data-testid="discord-agent-bind-buttons"
    >
      <Button
        variant="outline"
        size="sm"
        onClick={() => setDialogOpen(true)}
        disabled={!agentId}
        title={
          agentName
            ? t(($) => $.discord.bind_button_title, { agent: agentName })
            : undefined
        }
        data-testid="discord-agent-connect"
      >
        <DiscordMark className="h-3 w-3" />
        {t(($) => $.discord.bind_button)}
      </Button>

      <DiscordConnectDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        fixedAgentId={agentId}
      />
    </div>
  );
}

// DiscordAgentBotStatusRow is the compact, read-only connected affordance the
// agent inspector renders; it deep-links into the Integrations tab.
function DiscordAgentBotStatusRow({
  onClick,
  className,
}: {
  onClick: () => void;
  className?: string;
}) {
  const { t } = useT("settings");
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-caption text-muted-foreground transition-colors hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
        className,
      )}
      data-testid="discord-agent-bot-status"
    >
      <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
      <span className="truncate">{t(($) => $.discord.agent_bot_connected_label)}</span>
      <ChevronRight className="ml-auto h-3.5 w-3.5 shrink-0" />
    </button>
  );
}

// DiscordAgentBotConnectedBadge is the full "already connected" affordance:
// status + Disconnect, then an "Add to server" link to the invite URL.
function DiscordAgentBotConnectedBadge({
  installation,
  className,
}: {
  installation: DiscordInstallation;
  className?: string;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();

  const [confirmOpen, setConfirmOpen] = useState(false);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleDisconnect() {
    if (disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteDiscordInstallation(wsId, installation.id);
      await qc.invalidateQueries({ queryKey: discordKeys.installations(wsId) });
      toast.success(t(($) => $.discord.toast_disconnected));
      setConfirmOpen(false);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.discord.toast_disconnect_failed),
      );
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div
      className={cn("space-y-2", className)}
      data-testid="discord-agent-bot-connected"
    >
      <div className="flex items-center justify-between gap-3">
        <span className="inline-flex min-w-0 items-center gap-2 text-caption text-muted-foreground">
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
          <span className="truncate">
            {t(($) => $.discord.agent_bot_connected_label)}
            {installation.bot_username ? ` · @${installation.bot_username}` : ""}
          </span>
        </span>
        <Button
          variant="destructive"
          size="sm"
          onClick={() => setConfirmOpen(true)}
          disabled={disconnecting}
          title={t(($) => $.discord.agent_bot_disconnect_tooltip)}
          aria-label={t(($) => $.discord.disconnect)}
          data-testid="discord-agent-bot-disconnect"
        >
          <Trash2 className="h-3 w-3" />
          {disconnecting
            ? t(($) => $.discord.disconnecting)
            : t(($) => $.discord.disconnect)}
        </Button>
      </div>

      {installation.invite_url && (
        <button
          type="button"
          onClick={() => openExternal(installation.invite_url)}
          className="inline-flex items-center gap-1 text-caption text-muted-foreground underline-offset-2 transition-colors hover:text-foreground hover:underline"
          title={t(($) => $.discord.invite_button_tooltip)}
        >
          <ExternalLink className="h-3 w-3" />
          {t(($) => $.discord.invite_button)}
        </button>
      )}

      <AlertDialog
        open={confirmOpen}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setConfirmOpen(false);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.discord.disconnect_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.discord.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.discord.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting
                ? t(($) => $.discord.disconnecting)
                : t(($) => $.discord.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
