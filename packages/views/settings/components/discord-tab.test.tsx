// @vitest-environment jsdom

import { type ReactNode } from "react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

type MemberRole = "owner" | "admin" | "member" | "guest";

const membersRef = vi.hoisted(() => ({
  current: [{ user_id: "user-1", role: "owner" as MemberRole }],
}));
const installationsRef = vi.hoisted(() => ({
  current: {
    installations: [] as unknown[],
    configured: true,
  },
}));
const agentsRef = vi.hoisted(() => ({
  current: [
    { id: "agent-1", name: "Agent One", archived_at: null },
    { id: "agent-2", name: "Agent Two", archived_at: null },
    { id: "agent-3", name: "Archived Agent", archived_at: "2026-01-01T00:00:00Z" },
  ] as unknown[],
}));
const mockRegister = vi.hoisted(() => vi.fn());
const mockDeleteInstallation = vi.hoisted(() => vi.fn());
const mockOpenExternal = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());
const mockToastSuccess = vi.hoisted(() => vi.fn());
const discordQueryErrorRef = vi.hoisted(() => ({ current: false }));
const discordQueryLoadingRef = vi.hoisted(() => ({ current: false }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[]; enabled?: boolean }) => {
    if (opts.enabled === false) return { data: undefined, isLoading: false };
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("members")) return { data: membersRef.current, isLoading: false };
    if (key.includes("agents")) return { data: agentsRef.current, isLoading: false };
    if (key.includes("installations")) {
      return {
        data: discordQueryLoadingRef.current ? undefined : installationsRef.current,
        isLoading: discordQueryLoadingRef.current,
        isError: discordQueryErrorRef.current,
      };
    }
    return { data: undefined, isLoading: false };
  },
  useQueryClient: () => ({ invalidateQueries: mockInvalidate }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
  agentListOptions: () => ({ queryKey: ["agents"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getAgentName: (agentId: string) => `Agent ${agentId}`,
    getMemberName: () => "Unknown",
    getSquadName: () => "Unknown Squad",
    getActorName: () => "Unknown",
    getActorInitials: () => "??",
    getActorAvatarUrl: () => null,
  }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => (
    <span data-testid="actor-avatar" data-actor-id={actorId} />
  ),
}));

vi.mock("@multica/core/discord", () => ({
  discordInstallationsOptions: () => ({
    queryKey: ["discord", "installations"],
    queryFn: vi.fn(),
  }),
  discordKeys: { installations: (wsId: string) => ["discord", "installations", wsId] },
}));

vi.mock("@multica/core/api", () => {
  class ApiError extends Error {
    status: number;
    statusText: string;
    body?: unknown;
    constructor(message: string, status: number, statusText: string, body?: unknown) {
      super(message);
      this.name = "ApiError";
      this.status = status;
      this.statusText = statusText;
      this.body = body;
    }
  }
  return {
    ApiError,
    api: {
      registerDiscordBot: mockRegister,
      deleteDiscordInstallation: mockDeleteInstallation,
    },
  };
});

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (sel?: (s: { user: { id: string } }) => unknown) =>
      sel ? sel({ user: { id: "user-1" } }) : { user: { id: "user-1" } },
    { getState: () => ({ user: { id: "user-1" } }) },
  );
  return { useAuthStore };
});

vi.mock("sonner", () => ({
  toast: { success: mockToastSuccess, error: mockToastError, message: vi.fn() },
}));

vi.mock("../../platform", () => ({ openExternal: mockOpenExternal }));

import { DiscordAgentBindButton, DiscordTab } from "./discord-tab";
import { ApiError } from "@multica/core/api";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };

function renderUI(children: ReactNode) {
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>,
  );
}

function resetFixtures() {
  vi.clearAllMocks();
  membersRef.current = [{ user_id: "user-1", role: "owner" }];
  installationsRef.current = { installations: [], configured: true };
  agentsRef.current = [
    { id: "agent-1", name: "Agent One", archived_at: null },
    { id: "agent-2", name: "Agent Two", archived_at: null },
    { id: "agent-3", name: "Archived Agent", archived_at: "2026-01-01T00:00:00Z" },
  ];
  discordQueryErrorRef.current = false;
  discordQueryLoadingRef.current = false;
}

describe("DiscordTab", () => {
  beforeEach(resetFixtures);

  it("shows a loading state without claiming Discord is disabled", () => {
    discordQueryLoadingRef.current = true;
    renderUI(<DiscordTab />);
    expect(screen.getByText("Loading…")).toBeTruthy();
    expect(screen.queryByText(/Discord integration not enabled/i)).toBeNull();
  });

  it("shows a load error instead of pretending Discord is disabled", () => {
    discordQueryErrorRef.current = true;
    renderUI(<DiscordTab />);
    expect(screen.getByText(/Failed to load Discord installations/i)).toBeTruthy();
    expect(screen.queryByText(/Discord integration not enabled/i)).toBeNull();
  });

  // Canonical: configured:false is a deliberate disabled state (the server
  // returns it when MULTICA_DISCORD_SECRET_KEY is unset), never an error.
  it("renders the disabled state, not an error, when the deployment has no Discord key", () => {
    installationsRef.current = { installations: [], configured: false };
    renderUI(<DiscordTab />);
    expect(screen.getByText(/Discord integration not enabled/i)).toBeTruthy();
    expect(screen.getByText(/MULTICA_DISCORD_SECRET_KEY/)).toBeTruthy();
    expect(screen.queryByTestId("discord-connect-open")).toBeNull();
  });

  it("shows the empty state when configured but nothing is connected", () => {
    renderUI(<DiscordTab />);
    expect(screen.getByText(/No bots connected yet/i)).toBeTruthy();
  });

  it("renders the installations list with agent name, bot username, and an invite link", () => {
    installationsRef.current = {
      installations: [
        {
          id: "inst-1",
          agent_id: "agent-7",
          status: "active",
          bot_username: "my_bot",
          installed_at: "2026-01-01T00:00:00Z",
          invite_url: "https://discord.com/oauth2/authorize?client_id=123",
        },
      ],
      configured: true,
    };
    renderUI(<DiscordTab />);
    expect(screen.getByText("Agent agent-7")).toBeTruthy();
    expect(screen.getByText("@my_bot")).toBeTruthy();
    expect(screen.getByText(/Disconnect/i)).toBeTruthy();
    expect(screen.getByTestId("discord-invite-link")).toBeTruthy();
  });

  it("marks a revoked installation instead of offering to disconnect it again", () => {
    installationsRef.current = {
      installations: [
        {
          id: "inst-1",
          agent_id: "agent-7",
          status: "revoked",
          bot_username: "my_bot",
          installed_at: "2026-01-01T00:00:00Z",
          invite_url: "",
        },
      ],
      configured: true,
    };
    renderUI(<DiscordTab />);
    expect(screen.getByText("revoked")).toBeTruthy();
    expect(screen.queryByText("Disconnect")).toBeNull();
  });

  it("hides Connect Discord for a plain member (backend is admin-only)", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    renderUI(<DiscordTab />);
    expect(screen.queryByTestId("discord-connect-open")).toBeNull();
  });

  it("hides Disconnect for a plain member even when a bot is connected", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    installationsRef.current = {
      installations: [
        { id: "inst-1", agent_id: "agent-7", status: "active", bot_username: "my_bot" },
      ],
      configured: true,
    };
    renderUI(<DiscordTab />);
    expect(screen.queryByText("Disconnect")).toBeNull();
  });

  it("submits the install form with the picked agent and pasted token", async () => {
    // Base UI's Select needs a real pointer/timer-driven user-event instance
    // to open its portal reliably in jsdom — the bare `userEvent.click`
    // module API hangs here, matching preferences-tab.test.tsx's precedent
    // for the same Select primitive. pointerEventsCheck is disabled because
    // the popup's open transition briefly leaves `pointer-events: none` on
    // an ancestor, which user-event's strict pointer check otherwise trips
    // on even though the click lands on the visible, interactive option.
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    mockRegister.mockResolvedValue({ id: "inst-1", status: "active" });
    renderUI(<DiscordTab />);

    await user.click(screen.getByTestId("discord-connect-open"));
    await screen.findByTestId("discord-connect-dialog");

    await user.click(screen.getByTestId("discord-agent-select"));
    const option = await screen.findByRole("option", { name: "Agent One" });
    await user.click(option);

    // Archived agents never reach the picker.
    expect(screen.queryByRole("option", { name: "Archived Agent" })).toBeNull();

    await user.type(screen.getByTestId("discord-bot-token"), "a-bot-token");
    await user.click(screen.getByTestId("discord-connect-submit"));

    await waitFor(() =>
      expect(mockRegister).toHaveBeenCalledWith("workspace-1", {
        bot_token: "a-bot-token",
        agent_id: "agent-1",
      }),
    );
    expect(mockInvalidate).toHaveBeenCalled();
    expect(mockToastSuccess).toHaveBeenCalled();
  }, 15000);

  it.each([
    { status: 400, expectedTitle: "Invalid bot token", message: "Discord rejected this bot token — generate a current token in the Discord developer portal and try again" },
    { status: 409, expectedTitle: "Bot already connected elsewhere", message: "this Discord bot is already connected to a different Multica workspace — disconnect it there before connecting it here" },
    { status: 503, expectedTitle: "Could not verify this bot", message: "could not reach Discord to verify this bot — check the server network or proxy and try again; the token was not saved" },
    { status: 500, expectedTitle: "Connection failed", message: "could not save this Discord bot — something went wrong on the server; the token was not saved" },
  ])(
    "renders a distinct error panel for a $status server response",
    async ({ status, expectedTitle, message }) => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      mockRegister.mockRejectedValue(new ApiError(message, status, "error"));
      renderUI(<DiscordTab />);

      await user.click(screen.getByTestId("discord-connect-open"));
      await user.click(screen.getByTestId("discord-agent-select"));
      await user.click(await screen.findByRole("option", { name: "Agent One" }));
      await user.type(screen.getByTestId("discord-bot-token"), "a-bot-token");
      await user.click(screen.getByTestId("discord-connect-submit"));

      const panel = await screen.findByTestId("discord-connect-error");
      expect(panel.textContent).toContain(expectedTitle);
      expect(panel.textContent).toContain(message);
      // The dialog stays open on failure so the admin can fix the token
      // instead of losing their place.
      expect(screen.getByTestId("discord-connect-dialog")).toBeTruthy();
    },
    15000,
  );

  it("disconnects only after confirmation and refreshes the installation list", async () => {
    mockDeleteInstallation.mockResolvedValue(undefined);
    installationsRef.current = {
      installations: [
        { id: "inst-1", agent_id: "agent-7", status: "active", bot_username: "my_bot" },
      ],
      configured: true,
    };
    renderUI(<DiscordTab />);
    await userEvent.click(screen.getByRole("button", { name: /Disconnect/i }));
    expect(mockDeleteInstallation).not.toHaveBeenCalled();
    const actions = await screen.findAllByRole("button", { name: /^Disconnect$/i });
    await userEvent.click(actions.at(-1)!);
    await waitFor(() =>
      expect(mockDeleteInstallation).toHaveBeenCalledWith("workspace-1", "inst-1"),
    );
    expect(mockInvalidate).toHaveBeenCalled();
  });

  it("keeps the installation visible when disconnect fails", async () => {
    mockDeleteInstallation.mockRejectedValue(new Error("network failed"));
    installationsRef.current = {
      installations: [
        { id: "inst-1", agent_id: "agent-7", status: "active", bot_username: "my_bot" },
      ],
      configured: true,
    };
    renderUI(<DiscordTab />);
    await userEvent.click(screen.getByRole("button", { name: /Disconnect/i }));
    const actions = await screen.findAllByRole("button", { name: /^Disconnect$/i });
    await userEvent.click(actions.at(-1)!);
    await waitFor(() => expect(mockToastError).toHaveBeenCalledWith("network failed"));
    expect(screen.getByText("@my_bot")).toBeTruthy();
    expect(mockInvalidate).not.toHaveBeenCalled();
  });

  // Malformed-response defense (CLAUDE.md -> API Compatibility): a response
  // missing `installations` must not crash the panel.
  it("tolerates a malformed installations response", () => {
    installationsRef.current = { configured: true } as never;
    renderUI(<DiscordTab />);
    expect(screen.getByText(/No bots connected yet/i)).toBeTruthy();
  });
});

describe("DiscordAgentBindButton", () => {
  beforeEach(resetFixtures);

  it("opens the connect dialog with no agent picker and submits the pasted bot token", async () => {
    mockRegister.mockResolvedValue({ id: "inst-1", status: "active" });
    renderUI(<DiscordAgentBindButton agentId="agent-1" agentName="Bot" />);
    await userEvent.click(screen.getByTestId("discord-agent-connect"));
    await screen.findByTestId("discord-connect-dialog");
    expect(screen.queryByTestId("discord-agent-select")).toBeNull();
    const tokenInput = screen.getByTestId("discord-bot-token");
    expect(tokenInput).toHaveAttribute("type", "password");
    await userEvent.type(tokenInput, "a-bot-token");
    await userEvent.click(screen.getByTestId("discord-connect-submit"));
    await waitFor(() =>
      expect(mockRegister).toHaveBeenCalledWith("workspace-1", {
        bot_token: "a-bot-token",
        agent_id: "agent-1",
      }),
    );
  });

  it("shows the connected badge (not the CTA) when the agent already has an active install", () => {
    installationsRef.current = {
      installations: [
        { id: "inst-1", agent_id: "agent-1", status: "active", bot_username: "my_bot" },
      ],
      configured: true,
    };
    renderUI(<DiscordAgentBindButton agentId="agent-1" />);
    expect(screen.getByTestId("discord-agent-bot-connected")).toBeTruthy();
    expect(screen.getByTestId("discord-agent-bot-disconnect")).toBeTruthy();
    expect(screen.queryByTestId("discord-agent-connect")).toBeNull();
  });

  it("disconnects an agent bot only after confirmation", async () => {
    mockDeleteInstallation.mockResolvedValue(undefined);
    installationsRef.current = {
      installations: [
        { id: "inst-1", agent_id: "agent-1", status: "active", bot_username: "my_bot" },
      ],
      configured: true,
    };
    renderUI(<DiscordAgentBindButton agentId="agent-1" />);

    await userEvent.click(screen.getByTestId("discord-agent-bot-disconnect"));
    expect(mockDeleteInstallation).not.toHaveBeenCalled();
    const actions = await screen.findAllByRole("button", { name: /^Disconnect$/i });
    await userEvent.click(actions.at(-1)!);

    await waitFor(() =>
      expect(mockDeleteInstallation).toHaveBeenCalledWith("workspace-1", "inst-1"),
    );
  });

  it("renders nothing for a non-manager", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    const { container } = renderUI(<DiscordAgentBindButton agentId="agent-1" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when Discord is not configured and the agent is unbound", () => {
    installationsRef.current = { installations: [], configured: false };
    const { container } = renderUI(<DiscordAgentBindButton agentId="agent-1" />);
    expect(container).toBeEmptyDOMElement();
  });
});
