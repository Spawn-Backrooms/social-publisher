ALTER TABLE sp_targets ADD COLUMN processing_started_at timestamptz;

-- Existing operations keep their original age; only an explicit retry or
-- processing resumption grants a new window. started_at remains first-start
-- history and is never reset by those recovery actions.
UPDATE sp_targets SET processing_started_at=COALESCE(started_at,clock_timestamp())
WHERE COALESCE(progress->>'stage','')<>''
    OR (progress ? 'data' AND progress->'data'<>'null'::jsonb)
    OR COALESCE(progress->'unsafe','false'::jsonb)='true'::jsonb;
