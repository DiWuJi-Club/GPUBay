package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00025（计划 12：AI 管家）：带存量数据 Up → Down → Up 往返。
// 基线取 UpTo(24)：计划 9～11（00022～00024）合入前后都成立（goose 停在 ≤ 24 的最高版本），版本断言相对基线。
func TestMigration00025RoundTrip(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", dbtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	scalar := func(q string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s.String
	}
	version := func() int64 {
		t.Helper()
		v, err := p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const tables = `SELECT count(*) FROM information_schema.tables WHERE table_name IN
		('butler_settings', 'butler_conversations', 'butler_messages', 'butler_tool_calls', 'butler_usage')`

	if _, err := p.UpTo(ctx, 24); err != nil {
		t.Fatal(err)
	}
	base := version()
	if base < 21 || base > 24 {
		t.Fatalf("baseline version = %d", base)
	}
	exec(`INSERT INTO users (phone, kind, status, created_at, updated_at) VALUES ('13800000001', 'personal', 'active', now(), now())`)

	if _, err := p.UpByOne(ctx); err != nil {
		t.Fatalf("up 25: %v", err)
	}
	if v := version(); v != 25 {
		t.Fatalf("version after up = %d, want 25", v)
	}
	if got := scalar(tables); got != "5" {
		t.Fatalf("tables after up = %s", got)
	}
	// 迁移写入唯一的设置行（默认开启、30 万 token、60 轮）；id 只能是 1
	if got := scalar(`SELECT enabled::text || '/' || daily_token_limit || '/' || max_turns || '/' || notice || '/' || (updated_by IS NULL)::text FROM butler_settings WHERE id = 1`); got != "true/300000/60//true" {
		t.Fatalf("settings row = %s", got)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO butler_settings (id, enabled, updated_at) VALUES (2, true, now())`); err == nil {
		t.Fatal("second settings row accepted")
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE butler_settings SET daily_token_limit = 100 WHERE id = 1`); err == nil {
		t.Fatal("daily_token_limit below 10000 accepted")
	}

	exec(`INSERT INTO butler_conversations (id, user_id, provider, model, created_at, updated_at)
		VALUES ('cv-aaaaaaaaaaaa', 1, 'mock', 'mock', now(), now())`)
	if got := scalar(`SELECT status || '/' || run_token || '/' || context::text || '/' || user_turns FROM butler_conversations`); got != "idle//{}/0" {
		t.Fatalf("conversation defaults = %s", got)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE butler_conversations SET status = 'bogus'`); err == nil {
		t.Fatal("bogus conversation status accepted")
	}
	exec(`INSERT INTO butler_messages (conversation_id, seq, role, content, created_at)
		VALUES ('cv-aaaaaaaaaaaa', 1, 'user', '[{"type":"text","text":"你好"}]', now())`)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO butler_messages (conversation_id, seq, role, content, created_at)
		VALUES ('cv-aaaaaaaaaaaa', 1, 'assistant', '[]', now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("duplicate seq: err = %v, want unique violation", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO butler_messages (conversation_id, seq, role, content, created_at)
		VALUES ('cv-aaaaaaaaaaaa', 2, 'system', '[]', now())`); err == nil {
		t.Fatal("role system accepted")
	}
	exec(`INSERT INTO butler_messages (conversation_id, seq, role, content, created_at)
		VALUES ('cv-aaaaaaaaaaaa', 2, 'assistant', '[{"type":"tool_use","name":"release_instance"}]', now())`)
	exec(`INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, status, secret_env_enc, expires_at, created_at)
		VALUES ('tc-aaaaaaaaaaaa', 'cv-aaaaaaaaaaaa', (SELECT id FROM butler_messages WHERE seq = 2), 1, 'call_1', 'deploy_app', 'write', '{"env":{"API_KEY":"***"}}', 'pending', '\x0102'::bytea, now() + interval '10 minutes', now())`)
	// 同一对话只能有一条 pending
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, status, created_at)
		VALUES ('tc-bbbbbbbbbbbb', 'cv-aaaaaaaaaaaa', (SELECT id FROM butler_messages WHERE seq = 2), 1, 'call_2', 'release_instance', 'write', '{}', 'pending', now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("second pending: err = %v, want unique violation (butler_tool_calls_pending_uniq)", err)
	}
	exec(`INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, status, created_at)
		VALUES ('tc-cccccccccccc', 'cv-aaaaaaaaaaaa', (SELECT id FROM butler_messages WHERE seq = 2), 1, 'call_3', 'list_instances', 'read', '{}', 'executed', now())`)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, status, created_at)
		VALUES ('tc-dddddddddddd', 'cv-aaaaaaaaaaaa', (SELECT id FROM butler_messages WHERE seq = 2), 1, 'call_4', 'list_instances', 'other', '{}', 'executed', now())`); err == nil {
		t.Fatal("bogus kind accepted")
	}
	if got := scalar(`SELECT length(secret_env_enc) || '/' || summary || '/' || result_ref || '/' || (quote_amount IS NULL)::text FROM butler_tool_calls WHERE id = 'tc-aaaaaaaaaaaa'`); got != "2///true" {
		t.Fatalf("tool call cols = %s", got)
	}
	exec(`INSERT INTO butler_usage (user_id, day, requests, input_tokens, output_tokens) VALUES (1, '2026-10-24', 3, 100, 50)`)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO butler_usage (user_id, day) VALUES (1, '2026-10-24')`); !db.IsUniqueViolation(err) {
		t.Fatalf("duplicate usage day: err = %v, want unique violation", err)
	}
	// 删对话级联删除消息与工具调用
	exec(`INSERT INTO butler_conversations (id, user_id, provider, model, created_at, updated_at)
		VALUES ('cv-bbbbbbbbbbbb', 1, 'mock', 'mock', now(), now())`)
	exec(`INSERT INTO butler_messages (conversation_id, seq, role, content, created_at) VALUES ('cv-bbbbbbbbbbbb', 1, 'user', '[]', now())`)
	exec(`INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, status, created_at)
		VALUES ('tc-eeeeeeeeeeee', 'cv-bbbbbbbbbbbb', (SELECT id FROM butler_messages WHERE conversation_id = 'cv-bbbbbbbbbbbb'), 1, 'c', 'get_wallet', 'read', '{}', 'executed', now())`)
	exec(`DELETE FROM butler_conversations WHERE id = 'cv-bbbbbbbbbbbb'`)
	if got := scalar(`SELECT (SELECT count(*) FROM butler_messages WHERE conversation_id = 'cv-bbbbbbbbbbbb') + (SELECT count(*) FROM butler_tool_calls WHERE conversation_id = 'cv-bbbbbbbbbbbb')`); got != "0" {
		t.Fatalf("cascade left rows = %s", got)
	}

	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v := version(); v != base {
		t.Fatalf("version after down = %d, want %d", v, base)
	}
	if got := scalar(tables); got != "0" {
		t.Fatalf("down left tables: %s", got)
	}
	if got := scalar(`SELECT count(*) FROM users`); got != "1" {
		t.Fatalf("users after down = %s", got)
	}
	if _, err := p.UpByOne(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := scalar(`SELECT count(*) FROM butler_settings`); got != "1" {
		t.Fatalf("settings rows after round trip = %s", got)
	}
}
