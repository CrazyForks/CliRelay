package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const postgresRequestLogHygieneTimeout = 2 * time.Minute

var postgresRequestLogHygieneDone atomic.Bool

var redundantPostgresRequestLogIndexes = []string{
	"idx_logs_api_key_id_chart_cover",
	"idx_request_logs_tenant_auth_subject_time",
	"idx_logs_model",
}

// compactPostgresLogStorage applies low-risk, online maintenance only. It never
// deletes request metadata and never runs VACUUM FULL. Concurrent index drops
// keep reads and writes available while reducing per-insert index maintenance.
func compactPostgresLogStorage(parent context.Context, db *sql.DB) error {
	if db == nil || postgresRequestLogHygieneDone.Load() {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, postgresRequestLogHygieneTimeout)
	defer cancel()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("usage: acquire postgres maintenance connection: %w", err)
	}
	defer conn.Close()

	var locked bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext('clirelay.request_logs.hygiene'))`).Scan(&locked); err != nil {
		return fmt.Errorf("usage: acquire postgres request log maintenance lock: %w", err)
	}
	if !locked {
		return nil
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = conn.ExecContext(cleanupCtx, `SELECT pg_advisory_unlock(hashtext('clirelay.request_logs.hygiene'))`)
		_, _ = conn.ExecContext(cleanupCtx, `RESET lock_timeout`)
		_, _ = conn.ExecContext(cleanupCtx, `RESET statement_timeout`)
	}()

	for _, stmt := range []string{
		`SET lock_timeout = '5s'`,
		`SET statement_timeout = '90s'`,
	} {
		if _, err = conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("usage: configure postgres request log maintenance session: %w", err)
		}
	}

	var errs []error
	if _, err = conn.ExecContext(ctx, `ALTER TABLE public.request_logs SET (
		autovacuum_vacuum_scale_factor = 0.02,
		autovacuum_vacuum_threshold = 1000,
		autovacuum_analyze_scale_factor = 0.01,
		autovacuum_analyze_threshold = 1000,
		autovacuum_vacuum_insert_scale_factor = 0.05,
		autovacuum_vacuum_insert_threshold = 1000
	)`); err != nil {
		errs = append(errs, fmt.Errorf("usage: configure postgres request log autovacuum: %w", err))
	}
	for _, indexName := range redundantPostgresRequestLogIndexes {
		exists, errExists := postgresIndexExists(ctx, conn, indexName)
		if errExists != nil {
			errs = append(errs, errExists)
			continue
		}
		if !exists {
			continue
		}
		if _, errDrop := conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY IF EXISTS public.`+indexName); errDrop != nil {
			errs = append(errs, fmt.Errorf("usage: drop redundant postgres index %s: %w", indexName, errDrop))
			continue
		}
		log.Infof("usage: removed redundant postgres request log index %s", indexName)
	}
	if err = errors.Join(errs...); err != nil {
		return err
	}
	postgresRequestLogHygieneDone.Store(true)
	return nil
}

func postgresIndexExists(ctx context.Context, conn *sql.Conn, indexName string) (bool, error) {
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass(?) IS NOT NULL`, "public."+indexName).Scan(&exists); err != nil {
		return false, fmt.Errorf("usage: inspect postgres index %s: %w", indexName, err)
	}
	return exists, nil
}
