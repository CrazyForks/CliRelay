package postgres

const requestLogsAuthLookupIndexesSQL = `
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_index_time
  ON request_logs(tenant_id, auth_index, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_subject_time_cost
  ON request_logs(tenant_id, auth_subject_id, timestamp DESC)
  INCLUDE (cost);
`

const requestLogThinkingLevelSQL = `
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS thinking_level TEXT NOT NULL DEFAULT '';
`
