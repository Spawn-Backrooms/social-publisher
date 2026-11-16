package store

import (
	"time"

	"socialpublisher/internal/model"
)

// EventPage uses an exclusive, ascending ID cursor. Empty pages retain the
// supplied cursor, so callers may also use it to poll for subsequent events.
type EventPage struct {
	Events      []model.Event `json:"events"`
	NextAfterID int64         `json:"next_after_id"`
	HasMore     bool          `json:"has_more"`
}

type ProcessHeartbeat struct {
	InstanceID             string
	Role                   string
	BuildRevision          string
	WorkerProgressSequence int64
}

type OperationsQuery struct {
	InstanceID          string
	HeartbeatStaleAfter time.Duration
	WorkerStaleAfter    time.Duration
	Limit               int
}

// BoundedCount is exact unless Capped is true; a capped value is a lower bound.
type BoundedCount struct {
	Value  int64 `json:"value"`
	Capped bool  `json:"capped"`
}

type QueueSummary struct {
	DueTargets          BoundedCount `json:"due_targets"`
	OldestDueAt         *time.Time   `json:"oldest_due_at"`
	OldestDueAgeSeconds float64      `json:"oldest_due_age_seconds"`
	NeedsAttention      BoundedCount `json:"needs_attention"`
	ReauthRequired      BoundedCount `json:"reauth_required"`
	ProviderCooldowns   BoundedCount `json:"provider_cooldowns"`
}

type ProcessStatus struct {
	InstanceID             string     `json:"instance_id"`
	Role                   string     `json:"role"`
	BuildRevision          string     `json:"build_revision"`
	StartedAt              time.Time  `json:"started_at"`
	LastSeenAt             time.Time  `json:"last_seen_at"`
	WorkerProgressAt       *time.Time `json:"worker_progress_at"`
	WorkerProgressSequence int64      `json:"worker_progress_sequence"`
	HeartbeatFresh         bool       `json:"heartbeat_fresh"`
	WorkerProgressFresh    bool       `json:"worker_progress_fresh"`
	WorkerStarting         bool       `json:"worker_starting"`
}

// Process counts cover at most the most recently seen Limit instances. When
// Truncated is true, counts are lower bounds; LocalProcess is always read
// independently and is never omitted because of this global bound.
type ProcessSummary struct {
	Observed        int  `json:"observed"`
	FreshInstances  int  `json:"fresh_instances"`
	StaleInstances  int  `json:"stale_instances"`
	FreshWorkers    int  `json:"fresh_workers"`
	StaleWorkers    int  `json:"stale_workers"`
	StartingWorkers int  `json:"starting_workers"`
	Truncated       bool `json:"truncated"`
}

type ConnectionPool struct {
	MaxConns                int32   `json:"max_conns"`
	TotalConns              int32   `json:"total_conns"`
	AcquiredConns           int32   `json:"acquired_conns"`
	IdleConns               int32   `json:"idle_conns"`
	ConstructingConns       int32   `json:"constructing_conns"`
	AcquireCount            int64   `json:"acquire_count"`
	EmptyAcquireCount       int64   `json:"empty_acquire_count"`
	CanceledAcquireCount    int64   `json:"canceled_acquire_count"`
	AcquireDurationSeconds  float64 `json:"acquire_duration_seconds"`
	EmptyAcquireWaitSeconds float64 `json:"empty_acquire_wait_seconds"`
}

type PoolSummary struct {
	Runtime      ConnectionPool `json:"runtime"`
	AccountLocks ConnectionPool `json:"account_locks"`
}

type OperationsSnapshot struct {
	DatabaseTime time.Time      `json:"database_time"`
	LocalProcess *ProcessStatus `json:"local_process"`
	Queue        QueueSummary   `json:"queue"`
	Processes    ProcessSummary `json:"processes"`
	Pools        PoolSummary    `json:"pools"`
}

type RetentionPolicy struct {
	EventRetention time.Duration
	BatchSize      int
}

type RetentionResult struct {
	At                     time.Time `json:"at"`
	EventCandidatesScanned int64     `json:"event_candidates_scanned"`
	EventsDeleted          int64     `json:"events_deleted"`
	EventSweepComplete     bool      `json:"event_sweep_complete"`
	AdmissionsDeleted      int64     `json:"admissions_deleted"`
	OwnerBudgetsDeleted    int64     `json:"owner_budgets_deleted"`
	OAuthStatesDeleted     int64     `json:"oauth_states_deleted"`
	ProviderPacingDeleted  int64     `json:"provider_pacing_deleted"`
	HeartbeatsDeleted      int64     `json:"heartbeats_deleted"`
}
