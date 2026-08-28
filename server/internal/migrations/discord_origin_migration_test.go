package migrations

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const discordOriginMigrationTestSchema = "discord_origin_migration_test"

// preDiscordOriginMembers is the origin_type constraint membership as of
// migration 367 (post-Telegram), i.e. immediately before migration 900 runs.
var preDiscordOriginMembers = []string{
	"autopilot", "quick_create", "lark_chat", "slack_chat", "agent_create",
	"dingtalk_chat", "wecom_chat", "telegram_chat",
}

// postDiscordOriginMembers is the expected membership once migration 900/901
// have widened and validated the constraint to include Discord.
var postDiscordOriginMembers = append(append([]string{}, preDiscordOriginMembers...), "discord_chat")

func TestDiscordOriginMigrationsUpDownAndCatalog(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire Postgres connection: %v", err)
	}
	defer conn.Release()

	cleanup := func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+discordOriginMigrationTestSchema+" CASCADE")
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+discordOriginMigrationTestSchema); err != nil {
		t.Fatalf("create isolated migration schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('search_path', $1, false)`, discordOriginMigrationTestSchema); err != nil {
		t.Fatalf("set isolated migration search path: %v", err)
	}

	// Seed the table at the post-Telegram (366/367) state: the state
	// migrations 900/901 assume they are running against.
	if _, err := conn.Exec(ctx, `
		CREATE TABLE issue (
			id UUID PRIMARY KEY,
			origin_type TEXT NULL
		);
		ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
			CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'telegram_chat'));
	`); err != nil {
		t.Fatalf("create pre-Discord issue table: %v", err)
	}

	assertDiscordOriginConstraint(t, ctx, conn.Conn(), true, preDiscordOriginMembers)
	assertDiscordOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000001")
	assertUnknownOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000002")

	// 900.up: widen but NOT VALID.
	applyMigrationFile(t, ctx, conn.Conn(), "900_issue_origin_discord_chat.up.sql")
	assertDiscordOriginConstraint(t, ctx, conn.Conn(), false, postDiscordOriginMembers)
	if _, err := conn.Exec(ctx, `INSERT INTO issue (id, origin_type) VALUES ($1, 'discord_chat')`, "00000000-0000-4000-8000-000000000003"); err != nil {
		t.Fatalf("insert discord_chat after widening constraint: %v", err)
	}
	assertUnknownOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000004")

	// 901.up: validate. Every pre-existing origin, plus discord_chat, must
	// still be a member and the constraint must now be trusted.
	applyMigrationFile(t, ctx, conn.Conn(), "901_issue_origin_discord_chat_validate.up.sql")
	assertDiscordOriginConstraint(t, ctx, conn.Conn(), true, postDiscordOriginMembers)
	for i, origin := range postDiscordOriginMembers {
		id := fmt.Sprintf("00000000-0000-4000-8000-0000000001%02d", i)
		if _, err := conn.Exec(ctx, `INSERT INTO issue (id, origin_type) VALUES ($1, $2)`, id, origin); err != nil {
			t.Fatalf("insert origin_type=%s under validated widened constraint: %v", origin, err)
		}
	}
	assertUnknownOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000005")

	// 901.down: PostgreSQL cannot flip a validated constraint back to NOT
	// VALID in place, so this recreates the same widened member set as
	// NOT VALID. Membership must be unchanged; only convalidated flips.
	applyMigrationFile(t, ctx, conn.Conn(), "901_issue_origin_discord_chat_validate.down.sql")
	assertDiscordOriginConstraint(t, ctx, conn.Conn(), false, postDiscordOriginMembers)

	// 900.down: narrow back to the pre-Discord member set. This fails
	// closed while discord_chat rows remain, so remove them first.
	if _, err := conn.Exec(ctx, `DELETE FROM issue WHERE origin_type = 'discord_chat'`); err != nil {
		t.Fatalf("remove Discord rows before narrowing rollback: %v", err)
	}
	applyMigrationFile(t, ctx, conn.Conn(), "900_issue_origin_discord_chat.down.sql")
	assertDiscordOriginConstraint(t, ctx, conn.Conn(), true, preDiscordOriginMembers)
	assertDiscordOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000006")
	assertUnknownOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000007")
}

// constraintMemberPattern extracts single-quoted string literals from a
// pg_get_constraintdef() rendering such as:
//
//	CHECK ((origin_type = ANY (ARRAY['autopilot'::text, 'discord_chat'::text])))
var constraintMemberPattern = regexp.MustCompile(`'([a-z_]+)'`)

func assertDiscordOriginConstraint(t *testing.T, ctx context.Context, conn *pgx.Conn, wantValidated bool, wantMembers []string) {
	t.Helper()
	var validated bool
	var definition string
	if err := conn.QueryRow(ctx, `
		SELECT convalidated, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'issue'::regclass AND conname = 'issue_origin_type_check'
	`).Scan(&validated, &definition); err != nil {
		t.Fatalf("inspect issue origin constraint: %v", err)
	}
	if validated != wantValidated {
		t.Fatalf("constraint validated = %t, want %t (definition: %s)", validated, wantValidated, definition)
	}

	gotMembers := constraintMemberPattern.FindAllStringSubmatch(definition, -1)
	got := make([]string, 0, len(gotMembers))
	for _, m := range gotMembers {
		got = append(got, m[1])
	}
	sort.Strings(got)

	want := append([]string{}, wantMembers...)
	sort.Strings(want)

	if !equalStringSlices(got, want) {
		t.Fatalf("constraint members = %v, want %v (definition: %s)", got, want, definition)
	}
}

func assertDiscordOriginRejected(t *testing.T, ctx context.Context, conn *pgx.Conn, id string) {
	t.Helper()
	if _, err := conn.Exec(ctx, `INSERT INTO issue (id, origin_type) VALUES ($1, 'discord_chat')`, id); !isCheckViolation(err) {
		t.Fatalf("insert discord_chat under pre-Discord constraint: got %v, want check violation", err)
	}
}

func assertUnknownOriginRejected(t *testing.T, ctx context.Context, conn *pgx.Conn, id string) {
	t.Helper()
	if _, err := conn.Exec(ctx, `INSERT INTO issue (id, origin_type) VALUES ($1, 'not_a_real_origin')`, id); !isCheckViolation(err) {
		t.Fatalf("insert not_a_real_origin: got %v, want check violation", err)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
