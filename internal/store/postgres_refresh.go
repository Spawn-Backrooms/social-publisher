package store

import "context"

// BeginTokenRefresh must commit before the provider receives the stored grant.
// Callers hold the account guard throughout begin, remote refresh and save.
// An unresolved intent is never overwritten or timed out: another guard owner
// must quarantine that credential version before allowing further token use.
func (s *Postgres) BeginTokenRefresh(ctx context.Context, id string, version int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sp_accounts SET refresh_token_version=$2,
        refresh_started_at=clock_timestamp(),updated_at=clock_timestamp()
        WHERE id=$1 AND token_version=$2 AND status='active' AND refresh_token_version IS NULL`, id, version)
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// ClearTokenRefresh is only valid when the caller knows no remote rotation was
// applied. Advancing the version also fences late completion of this attempt,
// even though the encrypted credential itself did not change.
func (s *Postgres) ClearTokenRefresh(ctx context.Context, id string, version int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sp_accounts SET refresh_token_version=NULL,refresh_started_at=NULL,
        token_version=token_version+1,updated_at=clock_timestamp()
        WHERE id=$1 AND token_version=$2 AND refresh_token_version=$2 AND status='active'`, id, version)
	if err != nil {
		return storeError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
