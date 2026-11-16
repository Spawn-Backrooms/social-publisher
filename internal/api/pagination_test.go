package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"socialpublisher/internal/model"
	"socialpublisher/internal/store"
)

type eventPageFixture struct {
	*apiStore
	gotOwner model.Principal
	gotPost  string
	gotAfter int64
	gotLimit int
}

func (s *eventPageFixture) ListEventsPage(_ context.Context, owner model.Principal, postID string, after int64, limit int) (store.EventPage, error) {
	s.gotOwner, s.gotPost, s.gotAfter, s.gotLimit = owner, postID, after, limit
	return store.EventPage{Events: []model.Event{{ID: 41, Kind: "target.queued"}, {ID: 42, Kind: "target.processing"}}, NextAfterID: 42, HasMore: true}, nil
}

func TestEventPaginationUsesBoundedOwnerScopedCursor(t *testing.T) {
	s, base, _, token := fixtureAPI(t)
	st := &eventPageFixture{apiStore: base}
	s.Store = st
	w := req(s.Handler(), "GET", "/v1/posts/owned-post/events?after_id=40&limit=2", "", token)
	if w.Code != http.StatusOK || st.gotOwner != base.owner || st.gotPost != "owned-post" || st.gotAfter != 40 || st.gotLimit != 2 {
		t.Fatalf("owner/cursor lost: status=%d owner=%+v post=%q after=%d limit=%d", w.Code, st.gotOwner, st.gotPost, st.gotAfter, st.gotLimit)
	}
	var page store.EventPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Events) != 2 || page.NextAfterID != 42 || !page.HasMore {
		t.Fatalf("pagination response=%+v err=%v", page, err)
	}
	w = req(s.Handler(), "GET", "/v1/posts/owned-post/events", "", token)
	if w.Code != http.StatusOK || st.gotLimit != 100 || st.gotAfter != 0 {
		t.Fatalf("unbounded event default: status=%d limit=%d after=%d", w.Code, st.gotLimit, st.gotAfter)
	}
}

func TestEventPaginationRejectsInvalidBounds(t *testing.T) {
	for _, query := range []string{"limit=201", "limit=0", "limit=1&limit=2", "after_id=-1", "after_id=9223372036854775808", "after_id=1&after_id=2"} {
		s, _, _, token := fixtureAPI(t)
		w := req(s.Handler(), "GET", "/v1/posts/owned-post/events?"+query, "", token)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid pagination %q reached store: %d %s", query, w.Code, w.Body.String())
		}
	}
}
