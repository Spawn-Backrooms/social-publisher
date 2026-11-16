// Package ops records database-backed process liveness and actual worker-loop
// progress, and runs bounded operational retention. It never calls providers.
package ops

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

type Backend interface {
	RecordProcessHeartbeat(context.Context, store.ProcessHeartbeat) error
	OperationsSnapshot(context.Context, store.OperationsQuery) (store.OperationsSnapshot, error)
	RetainOperationalRecords(context.Context, store.RetentionPolicy) (store.RetentionResult, error)
}

type Config struct {
	InstanceID          string
	Role                string
	BuildRevision       string
	HeartbeatInterval   time.Duration
	HeartbeatStaleAfter time.Duration
	WorkerStaleAfter    time.Duration
	RetentionInterval   time.Duration
	EventRetention      time.Duration
	RetentionBatchSize  int
	MetricLimit         int
	QueryTimeout        time.Duration
	Logger              *slog.Logger
}

type Snapshot struct {
	Healthy       bool                  `json:"healthy"`
	BuildRevision string                `json:"build_revision"`
	Role          string                `json:"role"`
	InstanceID    string                `json:"instance_id"`
	DatabaseTime  time.Time             `json:"database_time"`
	LocalProcess  *store.ProcessStatus  `json:"local_process"`
	Queue         store.QueueSummary    `json:"queue"`
	Processes     store.ProcessSummary  `json:"processes"`
	Pools         store.PoolSummary     `json:"pools"`
	Retention     store.RetentionResult `json:"retention"`
}

type Manager struct {
	backend  Backend
	config   Config
	progress atomic.Int64
	mu       sync.RWMutex
	last     store.RetentionResult
}

func New(backend Backend, config Config) (*Manager, error) {
	if backend == nil || (config.Role != "api" && config.Role != "worker" && config.Role != "serve") || config.BuildRevision == "" || len(config.BuildRevision) > 200 || len(config.InstanceID) > 200 {
		return nil, errors.New("invalid operations identity or role")
	}
	if config.InstanceID == "" {
		id, err := security.RandomToken(24)
		if err != nil {
			return nil, err
		}
		config.InstanceID = id
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 15 * time.Second
	}
	if config.HeartbeatStaleAfter == 0 {
		config.HeartbeatStaleAfter = time.Minute
	}
	if config.WorkerStaleAfter == 0 {
		config.WorkerStaleAfter = 13 * time.Minute
	}
	if config.RetentionInterval == 0 {
		config.RetentionInterval = time.Hour
	}
	if config.EventRetention == 0 {
		config.EventRetention = 90 * 24 * time.Hour
	}
	if config.RetentionBatchSize == 0 {
		config.RetentionBatchSize = 500
	}
	if config.MetricLimit == 0 {
		config.MetricLimit = 1000
	}
	if config.QueryTimeout == 0 {
		config.QueryTimeout = 5 * time.Second
	}
	if config.HeartbeatInterval <= 0 || config.HeartbeatStaleAfter <= config.HeartbeatInterval || config.WorkerStaleAfter <= config.HeartbeatInterval || config.RetentionInterval <= 0 || config.QueryTimeout <= 0 || config.EventRetention < 7*24*time.Hour || config.RetentionBatchSize < 1 || config.RetentionBatchSize > 2000 || config.MetricLimit < 1 || config.MetricLimit > 10000 {
		return nil, errors.New("invalid operations intervals, retention, or bounds")
	}
	return &Manager{backend: backend, config: config}, nil
}

// WorkerProgress is called only after a successful dispatch iteration, including
// a successful no-work check. A timer, a failed claim, or an in-flight provider
// request must not call it. API-only processes have no worker progress.
func (m *Manager) WorkerProgress() {
	if m.config.Role == "worker" || m.config.Role == "serve" {
		m.progress.Add(1)
	}
}

// Heartbeat can be called synchronously during startup, before opening HTTP
// readiness. Database time records liveness; only a new progress sequence moves
// the separately stored worker timestamp.
func (m *Manager) Heartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, m.config.QueryTimeout)
	defer cancel()
	return m.backend.RecordProcessHeartbeat(ctx, store.ProcessHeartbeat{
		InstanceID: m.config.InstanceID, Role: m.config.Role, BuildRevision: m.config.BuildRevision,
		WorkerProgressSequence: m.progress.Load(),
	})
}

func (m *Manager) Snapshot(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, m.config.QueryTimeout)
	defer cancel()
	value, err := m.backend.OperationsSnapshot(ctx, store.OperationsQuery{
		InstanceID: m.config.InstanceID, HeartbeatStaleAfter: m.config.HeartbeatStaleAfter,
		WorkerStaleAfter: m.config.WorkerStaleAfter, Limit: m.config.MetricLimit,
	})
	if err != nil {
		return Snapshot{}, err
	}
	healthy := value.LocalProcess != nil && value.LocalProcess.InstanceID == m.config.InstanceID &&
		value.LocalProcess.Role == m.config.Role && value.LocalProcess.BuildRevision == m.config.BuildRevision && value.LocalProcess.HeartbeatFresh
	if healthy && m.config.Role != "api" {
		healthy = value.LocalProcess.WorkerProgressFresh || value.LocalProcess.WorkerStarting
	}
	m.mu.RLock()
	retention := m.last
	m.mu.RUnlock()
	return Snapshot{Healthy: healthy, BuildRevision: m.config.BuildRevision, Role: m.config.Role, InstanceID: m.config.InstanceID,
		DatabaseTime: value.DatabaseTime, LocalProcess: value.LocalProcess, Queue: value.Queue, Processes: value.Processes,
		Pools: value.Pools, Retention: retention}, nil
}

func (m *Manager) RetainOnce(ctx context.Context) (store.RetentionResult, error) {
	ctx, cancel := context.WithTimeout(ctx, m.config.QueryTimeout)
	defer cancel()
	result, err := m.backend.RetainOperationalRecords(ctx, store.RetentionPolicy{EventRetention: m.config.EventRetention, BatchSize: m.config.RetentionBatchSize})
	if err == nil {
		m.mu.Lock()
		m.last = result
		m.mu.Unlock()
	}
	return result, err
}

// Run is stopped by process cancellation. Each maintenance iteration has its
// own bounded deadline, so retention cannot keep shutdown waiting indefinitely.
func (m *Manager) Run(ctx context.Context) {
	m.record(ctx)
	m.retain(ctx)
	heartbeats := time.NewTicker(m.config.HeartbeatInterval)
	defer heartbeats.Stop()
	retention := time.NewTicker(m.config.RetentionInterval)
	defer retention.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeats.C:
			m.record(ctx)
		case <-retention.C:
			m.retain(ctx)
		}
	}
}

func (m *Manager) retain(ctx context.Context) {
	if _, err := m.RetainOnce(ctx); err != nil && ctx.Err() == nil {
		m.log("operations_retention_failed")
	}
}

func (m *Manager) record(ctx context.Context) {
	if err := m.Heartbeat(ctx); err != nil && ctx.Err() == nil {
		m.log("operations_heartbeat_failed")
	}
}

func (m *Manager) log(message string) {
	if m.config.Logger != nil {
		m.config.Logger.Warn(message)
	}
}
