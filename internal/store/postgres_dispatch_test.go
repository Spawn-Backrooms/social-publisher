package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresBusyAccountBacklogDoesNotHideOtherAccount(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	b := fixtureAccount(t, s, owner, model.X)
	for range 80 {
		fixturePost(t, s, owner, time.Now().Add(-time.Hour), a)
	}
	other := fixturePost(t, s, owner, time.Now().Add(-time.Minute), b)
	busy, err := s.Claim(ctx, time.Minute)
	if err != nil || busy.AccountID != a.ID {
		t.Fatalf("first account was not admitted: %+v %v", busy, err)
	}
	next, err := s.Claim(ctx, time.Minute)
	if err != nil || next.ID != other.Targets[0].ID {
		t.Fatalf("busy account backlog hid unrelated work: account=%s target=%s error=%v", next.AccountID, next.ID, err)
	}
	if err := s.Heartbeat(ctx, busy.ID, busy.LeaseToken, time.Minute); err != nil {
		t.Fatal("other account only ran after the first account stopped:", err)
	}
	if err := s.FinishTarget(ctx, next.ID, next.LeaseToken, Finish{Status: "published", RemoteID: "other-account-receipt"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Claim(ctx, time.Minute)
	requireError(t, err, ErrNoWork)
}

func TestPostgresDispatchFindsMixedParentAndDisabledExpiredLease(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	b := fixtureAccount(t, s, owner, model.Instagram)
	post := fixturePost(t, s, owner, time.Now().Add(-time.Minute), a, b)
	first, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTarget(ctx, first.ID, first.LeaseToken, Finish{Status: "needs_attention", Progress: model.Progress{Unsafe: true}}); err != nil {
		t.Fatal(err)
	}
	second, err := s.Claim(ctx, time.Minute)
	if err != nil || second.PostID != post.ID || second.ID == first.ID {
		t.Fatalf("parent aggregate status hid its eligible target: %+v %v", second, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sp_accounts SET status='reauth_required' WHERE id=$1`, second.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sp_targets SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Claim(ctx, time.Minute)
	requireError(t, err, ErrNoWork)
	saved, err := s.GetPost(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range saved.Targets {
		if target.ID == second.ID && target.Status != "failed" {
			t.Fatalf("disabled expired lease was never recovered: %s", target.Status)
		}
	}
}

func TestPostgresDispatchDoesNotScanTerminalHistory(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	// A large retained history must not turn an idle poll into a posts-table
	// walk. Real persisted rows and EXPLAIN ANALYZE catch planner regressions.
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_posts(id,owner_tenant,owner_subject,status,publish_at,idempotency_key,request_hash)
		SELECT 'history-post-'||i,'tenant','subject','published',clock_timestamp()-interval '7 days','history-key-'||i,'hash'
		FROM generate_series(1,20000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_targets(id,post_id,account_id,owner_tenant,owner_subject,platform,position,content,status,next_attempt_at,remote_id)
		SELECT 'history-target-'||i,'history-post-'||i,$1,'tenant','subject','x',0,'{}','published',clock_timestamp()-interval '7 days','receipt-'||i
		FROM generate_series(1,20000) i`, a.ID); err != nil {
		t.Fatal(err)
	}
	fixturePost(t, s, owner, time.Now().Add(-time.Minute), a)
	if _, err := s.pool.Exec(ctx, `ANALYZE sp_posts; ANALYZE sp_targets; ANALYZE sp_accounts`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var raw []byte
	if err := tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+dispatchPostQuery).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []map[string]any
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	seenDue, seenExpired := false, false
	var inspect func(map[string]any)
	inspect = func(node map[string]any) {
		if node["Relation Name"] == "sp_posts" {
			kind, _ := node["Node Type"].(string)
			rows, _ := node["Actual Rows"].(float64)
			removed, _ := node["Rows Removed by Filter"].(float64)
			if !strings.Contains(kind, "Index") || rows+removed > 2 {
				t.Errorf("terminal posts were scanned: %s", raw)
			}
		}
		if node["Relation Name"] == "sp_targets" {
			kind, _ := node["Node Type"].(string)
			if !strings.Contains(kind, "Index") {
				t.Errorf("target history was scanned without an eligibility index: %s", raw)
			}
			condition, _ := node["Index Cond"].(string)
			seenDue = seenDue || strings.Contains(condition, "next_attempt_at <=")
			seenExpired = seenExpired || strings.Contains(condition, "lease_until <=")
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			inspect(child.(map[string]any))
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
	if !seenDue || !seenExpired {
		t.Fatalf("dispatch did not use both eligibility indexes: %s", raw)
	}
	t.Logf("dispatch with 20000 terminal posts: execution_ms=%v shared_hit_blocks=%v; both due/expired bounds use indexes; one primary-key parent lookup",
		plans[0]["Execution Time"], plans[0]["Plan"].(map[string]any)["Shared Hit Blocks"])
}
