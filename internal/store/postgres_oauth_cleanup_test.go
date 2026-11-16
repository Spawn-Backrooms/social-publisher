package store

import (
	"encoding/json"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestPostgresOAuthRequestsBoundExpiredStateCleanup(t *testing.T) {
	for _, operation := range []string{"put", "consume"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := postgresFixture(t, 1)
			ctx := t.Context()
			state := model.OAuthState{StateHash: "active", CookieHash: "browser", Owner: model.Principal{TenantID: "tenant", Subject: "subject"}, Platform: model.X, ExpiresAt: time.Now().Add(time.Hour)}
			if operation == "consume" {
				if err := s.PutOAuthState(ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.pool.Exec(ctx, `INSERT INTO sp_oauth_states(state_hash,cookie_hash,owner_tenant,owner_subject,platform,encrypted_verifier,issued_at,expires_at)
				SELECT 'expired-' || i,'browser','tenant','subject','x','',clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour'
				FROM generate_series(1,1200) AS i`); err != nil {
				t.Fatal(err)
			}
			if operation == "put" {
				if err := s.PutOAuthState(ctx, state); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.ConsumeOAuthState(ctx, state.StateHash, state.CookieHash, state.Platform); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM sp_oauth_states WHERE expires_at<=clock_timestamp()").Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining < 1000 {
				t.Fatalf("one OAuth request removed %d expired states; maximum is 200", 1200-remaining)
			}
		})
	}
}

func TestPostgresOAuthCleanupPlanBoundsLiveAndEqualExpiryBacklogs(t *testing.T) {
	for _, backlog := range []struct{ name, offset string }{
		{"live with nothing expired", "+ interval '1 hour'"},
		{"expired with tied timestamps", "- interval '1 hour'"},
	} {
		t.Run(backlog.name, func(t *testing.T) {
			s, _ := postgresFixture(t, 1)
			ctx := t.Context()
			if _, err := s.pool.Exec(ctx, `INSERT INTO sp_oauth_states(state_hash,cookie_hash,owner_tenant,owner_subject,platform,issued_at,expires_at)
				SELECT 'state-' || i,'cookie','tenant','subject','x',statement_timestamp()-interval '2 hours',statement_timestamp()`+backlog.offset+`
				FROM generate_series(1,20000) AS i`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, "ANALYZE sp_oauth_states"); err != nil {
				t.Fatal(err)
			}
			var encoded []byte
			if err := s.pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+pruneExpiredOAuthStates).Scan(&encoded); err != nil {
				t.Fatal(err)
			}
			var plan []map[string]any
			if err := json.Unmarshal(encoded, &plan); err != nil || len(plan) != 1 {
				t.Fatalf("invalid EXPLAIN output: %v", err)
			}
			var examined float64
			var visit func(map[string]any)
			visit = func(node map[string]any) {
				if node["Relation Name"] == "sp_oauth_states" {
					rows, _ := node["Actual Rows"].(float64)
					filtered, _ := node["Rows Removed by Filter"].(float64)
					loops, _ := node["Actual Loops"].(float64)
					examined += (rows + filtered) * loops
				}
				children, _ := node["Plans"].([]any)
				for _, child := range children {
					visit(child.(map[string]any))
				}
			}
			visit(plan[0]["Plan"].(map[string]any))
			// Allow planner variation around the 200 selected and 200 deleted
			// tuples, while rejecting either full-table/tie-group scan.
			if examined > 1000 {
				t.Fatalf("one OAuth cleanup examined %.0f table tuples; budget 1000 for at most 200 deletions; plan=%s", examined, encoded)
			}
			t.Logf("20,000-state backlog: %.0f table tuples examined", examined)
		})
	}
}
