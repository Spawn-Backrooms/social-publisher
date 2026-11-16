package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"socialpublisher/internal/ops"
)

type diagnosticFixture struct {
	snapshot ops.Snapshot
	err      error
}

func (f diagnosticFixture) Snapshot(context.Context) (ops.Snapshot, error) {
	return f.snapshot, f.err
}

func TestOperationsDiagnosticsRequireSeparateToken(t *testing.T) {
	s, st, _, bearer := fixtureAPI(t)
	s.OperationsToken = "fixture-only-operations-token"
	s.Operations = diagnosticFixture{snapshot: ops.Snapshot{Healthy: true, BuildRevision: "fixture-build", Role: "api"}}
	// Operations must still work when the owner admission store is unavailable.
	st.requestAdmissionError = errors.New("owner admission unavailable")
	for _, headers := range [][]string{nil, {"wrong-token"}, {s.OperationsToken, s.OperationsToken}} {
		r := httptest.NewRequest("GET", "/internal/ops", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		for _, value := range headers {
			r.Header.Add("X-Operations-Token", value)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "fixture-build") {
			t.Fatalf("invalid operations credential returned diagnostics: %d %s", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/internal/ops", nil)
	r.Header.Set("X-Operations-Token", s.OperationsToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized operations: %d %s", w.Code, w.Body.String())
	}
	var snapshot ops.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || !snapshot.Healthy || snapshot.BuildRevision != "fixture-build" {
		t.Fatalf("diagnostics contract lost: %+v %v", snapshot, err)
	}
}

func TestOperationsDiagnosticsFailClosedWhenUnavailableOrUnhealthy(t *testing.T) {
	for _, backend := range []diagnosticFixture{
		{snapshot: ops.Snapshot{Healthy: false, BuildRevision: "fixture-build"}},
		{err: errors.New("sensitive database connection details")},
	} {
		s, _, _, _ := fixtureAPI(t)
		s.OperationsToken = "fixture-only-operations-token"
		s.Operations = backend
		r := httptest.NewRequest("GET", "/internal/ops", nil)
		r.Header.Set("X-Operations-Token", s.OperationsToken)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "sensitive") {
			t.Fatalf("unavailable diagnostics: %d %s", w.Code, w.Body.String())
		}
	}
}
