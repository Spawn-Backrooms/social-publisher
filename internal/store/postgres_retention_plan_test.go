package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

// A LIMIT bounds deletion only after its input is produced. Unsupported tie
// sorts used to read an entire equal-expiry group before returning a tiny batch.
func TestPostgresRetentionEqualExpiryBacklogsUseBoundedPlans(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	a := fixtureAccount(t, s, model.Principal{TenantID: "tenant", Subject: "subject"}, model.X)
	for _, tc := range []struct {
		table, insert, cleanup string
		args                   []any
	}{
		{"sp_api_admissions", `INSERT INTO sp_api_admissions(token,owner_tenant,owner_subject,scope_kind,scope_key,expires_at)
			SELECT 'expired-'||i,'tenant','subject','request','',statement_timestamp()-interval '10 days' FROM generate_series(1,10000) i`, retentionAdmissionsQuery, nil},
		{"sp_api_owner_budgets", `INSERT INTO sp_api_owner_budgets(owner_tenant,owner_subject,window_started_at)
			SELECT 'tenant','subject-'||i,statement_timestamp()-interval '10 days' FROM generate_series(1,10000) i`, retentionOwnerBudgetsQuery, nil},
		{"sp_oauth_states", `INSERT INTO sp_oauth_states(state_hash,cookie_hash,owner_tenant,owner_subject,platform,expires_at)
			SELECT 'expired-'||i,'binding','tenant','subject','x',statement_timestamp()-interval '10 days' FROM generate_series(1,10000) i`, retentionOAuthQuery, nil},
		{"sp_process_heartbeats", `INSERT INTO sp_process_heartbeats(instance_id,role,build_revision,last_seen_at)
			SELECT 'expired-'||i,'api','test-revision',statement_timestamp()-interval '10 days' FROM generate_series(1,10000) i`, retentionHeartbeatsQuery, nil},
		{"sp_provider_pacing", `INSERT INTO sp_provider_pacing(account_id,scope,next_allowed_at)
			SELECT $1,'scope-'||i,statement_timestamp()-interval '10 days' FROM generate_series(1,10000) i`, retentionPacingQuery, []any{a.ID}},
	} {
		t.Run(tc.table, func(t *testing.T) {
			if _, err := s.pool.Exec(ctx, tc.insert, tc.args...); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, "ANALYZE "+tc.table); err != nil {
				t.Fatal(err)
			}
			var now time.Time
			if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
				t.Fatal(err)
			}
			for _, batchSize := range []int{3, 500, 2000} {
				t.Run(fmt.Sprintf("batch_%d", batchSize), func(t *testing.T) {
					var encoded []byte
					if err := s.pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+tc.cleanup, now, batchSize).Scan(&encoded); err != nil {
						t.Fatal(err)
					}
					var plans []struct {
						Plan map[string]any `json:"Plan"`
					}
					if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
						t.Fatalf("cannot decode retention query plan: %v", err)
					}
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						if node["Relation Name"] == tc.table {
							rows, _ := node["Actual Rows"].(float64)
							removed, _ := node["Rows Removed by Filter"].(float64)
							loops, _ := node["Actual Loops"].(float64)
							work := (rows + removed) * loops
							if work > float64(batchSize+32) {
								t.Errorf("batch of %d examined %.0f %s rows through %v; candidate selection and join-back must both be bounded", batchSize, work, tc.table, node["Node Type"])
							}
						}
						children, _ := node["Plans"].([]any)
						for _, child := range children {
							if child, ok := child.(map[string]any); ok {
								inspect(child)
							}
						}
					}
					inspect(plans[0].Plan)
				})
			}
		})
	}
}

func TestPostgresRetentionEventAndParentLookupsStayBounded(t *testing.T) {
	s, _ := postgresFixture(t, 4)
	ctx := t.Context()
	owner := model.Principal{TenantID: "tenant", Subject: "subject"}
	a := fixtureAccount(t, s, owner, model.X)
	p := fixturePost(t, s, owner, time.Now().Add(time.Hour), a)
	if _, err := s.pool.Exec(ctx, `UPDATE sp_targets SET status='published',remote_id='confirmed' WHERE post_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_events(post_id,target_id,kind,created_at)
		SELECT $1,$2,'target.processing',statement_timestamp()-interval '120 days' FROM generate_series(1,10000)`, p.ID, p.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_posts(id,owner_tenant,owner_subject,status,publish_at,timezone,idempotency_key,request_hash)
		SELECT 'planned-post-'||lpad(i::text,5,'0'),'tenant','subject','published',statement_timestamp(),'UTC','planned-key-'||i,'hash'
		FROM generate_series(1,10000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_targets(id,post_id,account_id,owner_tenant,owner_subject,platform,position,content,status,next_attempt_at,remote_id)
		SELECT 'planned-target-'||lpad(i::text,5,'0'),'planned-post-'||lpad(i::text,5,'0'),$1,'tenant','subject','x',0,'{}','published',statement_timestamp(),'confirmed-'||i
		FROM generate_series(1,10000) i`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sp_events(post_id,target_id,kind,created_at)
		SELECT 'planned-post-'||lpad(i::text,5,'0'),'planned-target-'||lpad(i::text,5,'0'),'target.processing',statement_timestamp()-interval '120 days'
		FROM generate_series(1,10000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "ANALYZE sp_posts; ANALYZE sp_events; ANALYZE sp_targets"); err != nil {
		t.Fatal(err)
	}
	for _, batchSize := range []int{3, 500, 2000} {
		t.Run(fmt.Sprintf("parents_%d", batchSize), func(t *testing.T) {
			parents := make([]string, batchSize)
			for i := range parents {
				parents[i] = fmt.Sprintf("planned-post-%05d", i+1)
			}
			assertRetentionRelationWork(t, s, retentionLockParentsQuery, map[string]int{"sp_posts": batchSize}, parents)
		})
		t.Run(fmt.Sprintf("events_%d", batchSize), func(t *testing.T) {
			rows, err := s.pool.Query(ctx, "SELECT id FROM sp_events WHERE post_id=$1 AND kind='target.processing' ORDER BY id LIMIT $2", p.ID, batchSize)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]int64, 0, batchSize)
			for rows.Next() {
				var id int64
				if err = rows.Scan(&id); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			assertRetentionRelationWork(t, s, retentionDeleteEventsQuery, map[string]int{"sp_events": batchSize, "sp_targets": batchSize}, ids, []string{p.ID})
		})
		t.Run(fmt.Sprintf("distinct_parent_events_%d", batchSize), func(t *testing.T) {
			rows, err := s.pool.Query(ctx, "SELECT id,post_id FROM sp_events WHERE post_id LIKE 'planned-post-%' ORDER BY id LIMIT $1", batchSize)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			var parents []string
			for rows.Next() {
				var id int64
				var parent string
				if err := rows.Scan(&id, &parent); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				ids, parents = append(ids, id), append(parents, parent)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			// Inspect every relation in the deletion plan. Limiting event rows
			// alone misses an anti-join that scans all retained target history.
			assertRetentionRelationWork(t, s, retentionDeleteEventsQuery, map[string]int{"sp_events": batchSize, "sp_targets": batchSize}, ids, parents)
		})
	}
}

func assertRetentionRelationWork(t *testing.T, s *Postgres, query string, bounds map[string]int, args ...any) {
	t.Helper()
	var encoded []byte
	if err := s.pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query, args...).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("cannot decode retention query plan: %v", err)
	}
	work := make(map[string]float64)
	var inspect func(map[string]any)
	inspect = func(node map[string]any) {
		if relation, ok := node["Relation Name"].(string); ok {
			rows, _ := node["Actual Rows"].(float64)
			removed, _ := node["Rows Removed by Filter"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			work[relation] += (rows + removed) * loops
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if child, ok := child.(map[string]any); ok {
				inspect(child)
			}
		}
	}
	inspect(plans[0].Plan)
	for relation, examined := range work {
		batchSize, expected := bounds[relation]
		if !expected {
			t.Errorf("retention unexpectedly examined relation %s", relation)
		} else if examined > float64(batchSize+32) {
			t.Errorf("batch of %d examined %.0f %s rows; every table in the plan must be bounded", batchSize, examined, relation)
		}
	}
}
