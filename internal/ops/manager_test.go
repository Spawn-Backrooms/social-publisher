package ops

import (
	"context"
	"errors"
	"testing"
	"time"

	"socialpublisher/internal/store"
)

type snapshotBackend struct {
	value          store.OperationsSnapshot
	err            error
	retention      store.RetentionResult
	afterRetention context.CancelFunc
}

func (b *snapshotBackend) RecordProcessHeartbeat(context.Context, store.ProcessHeartbeat) error {
	return b.err
}
func (b *snapshotBackend) OperationsSnapshot(context.Context, store.OperationsQuery) (store.OperationsSnapshot, error) {
	return b.value, b.err
}
func (b *snapshotBackend) RetainOperationalRecords(context.Context, store.RetentionPolicy) (store.RetentionResult, error) {
	if b.afterRetention != nil {
		b.afterRetention()
	}
	return b.retention, b.err
}

func TestSnapshotSeparatesAPIHeartbeatFromWorkerProgress(t *testing.T) {
	for _, tc := range []struct {
		name, role               string
		seen, progress, starting bool
		wantHealthy              bool
	}{
		{"api is live without a dispatcher", "api", true, false, false, true},
		{"worker timer after startup grace is insufficient", "worker", true, false, false, false},
		{"serve timer after startup grace is insufficient", "serve", true, false, false, false},
		{"worker dispatch loop progressed", "worker", true, true, false, true},
		{"serve dispatch loop progressed", "serve", true, true, false, true},
		{"stale process is unhealthy despite old progress", "serve", false, true, false, false},
		{"long first worker iteration is within startup grace", "worker", true, false, true, true},
		{"long first serve iteration is within startup grace", "serve", true, false, true, true},
		{"startup grace does not excuse stale heartbeat", "serve", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &snapshotBackend{value: store.OperationsSnapshot{LocalProcess: &store.ProcessStatus{
				InstanceID: "instance", Role: tc.role, BuildRevision: "built-revision", HeartbeatFresh: tc.seen, WorkerProgressFresh: tc.progress, WorkerStarting: tc.starting,
			}}}
			manager, err := New(backend, Config{InstanceID: "instance", Role: tc.role, BuildRevision: "built-revision"})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := manager.Snapshot(t.Context())
			if err != nil || snapshot.Healthy != tc.wantHealthy || snapshot.BuildRevision != "built-revision" || snapshot.Role != tc.role {
				t.Fatalf("snapshot: %+v err=%v", snapshot, err)
			}
		})
	}
}

func TestSnapshotRequiresMatchingProcessIdentityAndDatabase(t *testing.T) {
	for _, row := range []*store.ProcessStatus{
		nil,
		{InstanceID: "wrong", Role: "api", BuildRevision: "release", HeartbeatFresh: true},
		{InstanceID: "instance", Role: "worker", BuildRevision: "release", HeartbeatFresh: true, WorkerProgressFresh: true},
		{InstanceID: "instance", Role: "api", BuildRevision: "previous-release", HeartbeatFresh: true},
	} {
		backend := &snapshotBackend{value: store.OperationsSnapshot{LocalProcess: row}}
		manager, err := New(backend, Config{InstanceID: "instance", Role: "api", BuildRevision: "release"})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := manager.Snapshot(t.Context())
		if err != nil || snapshot.Healthy {
			t.Fatalf("missing or mismatched process reported healthy: %+v %v", snapshot, err)
		}
		backend.err = errors.New("database unavailable")
		if _, err = manager.Snapshot(t.Context()); err == nil {
			t.Fatal("database failure was hidden by cached health")
		}
	}
}

func TestConfigRejectsRetentionBelowSevenDays(t *testing.T) {
	_, err := New(&snapshotBackend{}, Config{Role: "serve", BuildRevision: "test", EventRetention: 7*24*time.Hour - time.Second})
	if err == nil {
		t.Fatal("unsafe event retention was accepted")
	}
}

func TestConfigRejectsHeartbeatFasterThanItsStalenessThreshold(t *testing.T) {
	_, err := New(&snapshotBackend{}, Config{Role: "serve", BuildRevision: "test", HeartbeatInterval: time.Minute, HeartbeatStaleAfter: time.Second})
	if err == nil {
		t.Fatal("heartbeat configuration would mark a normally operating process stale")
	}
}

func TestRunPerformsRetentionBeforeItsFirstHourlyTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	completedAt := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	backend := &snapshotBackend{retention: store.RetentionResult{At: completedAt, EventsDeleted: 3}, afterRetention: cancel}
	manager, err := New(backend, Config{Role: "api", BuildRevision: "release", RetentionInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	manager.Run(ctx)
	snapshot, err := manager.Snapshot(t.Context())
	if err != nil || !snapshot.Retention.At.Equal(completedAt) || snapshot.Retention.EventsDeleted != 3 {
		t.Fatalf("a process restarted before the hourly tick never cleaned up: %+v err=%v", snapshot.Retention, err)
	}
}
