package aiaccountstatus

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

const (
	defaultMaxConcurrency  = 4
	jobTTL                 = 30 * time.Minute
	accountRefreshMinGap   = 20 * time.Second
	probeTimeout           = 25 * time.Second
	staleRefreshThreshold  = 15 * time.Minute
	staleNormalizeInterval = 5 * time.Minute // GET stays read-mostly; do not UPDATE every list
)

type CacheInvalidator func(tenantID, authIndex, authSubjectID string)
type APIToolsFactory func(tenantID string) *managementapitools.Service

// ProbeFunc is injectable for tests.
type ProbeFunc func(ctx context.Context, svc *managementapitools.Service, cfg *config.Config, auth *coreauth.Auth) (ProbeResult, error)

type Service struct {
	cfg            *config.Config
	authManager    *coreauth.Manager
	apiToolsFor    APIToolsFactory
	invalidate     CacheInvalidator
	maxConcurrency int
	probeFn        ProbeFunc
	// upsertStatus is nil in production (uses usage.UpsertAIAccountStatus); tests may override.
	upsertStatus func(usage.AIAccountStatusRecord) error
	// reconcileQuota is nil in production (uses authManager.ReconcileQuota); tests may override.
	reconcileQuota func(ctx context.Context, authID string) (bool, error)
	// normalizeStale is nil in production (usage.NormalizeStaleAIAccountRefreshStates).
	normalizeStale func(tenantID string, olderThan time.Duration) (int64, error)

	sf  singleflight.Group
	sem chan struct{} // process-wide probe concurrency across jobs

	mu            sync.Mutex
	jobs          map[string]*job
	inFlight      map[string]string    // tenant|subject -> jobID owning the flight
	lastSuccess   map[string]time.Time // tenant|subject -> last successful probe (closes force=false TOCTOU)
	staleNormMu   sync.Mutex
	lastStaleNorm map[string]time.Time
}

type job struct {
	ID        string
	TenantID  string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Results   map[string]*AccountRefreshResult
	order     []string
}

func New(cfg *config.Config, authManager *coreauth.Manager, apiToolsFor APIToolsFactory, invalidate CacheInvalidator) *Service {
	return &Service{
		cfg:            cfg,
		authManager:    authManager,
		apiToolsFor:    apiToolsFor,
		invalidate:     invalidate,
		maxConcurrency: defaultMaxConcurrency,
		probeFn:        probeAuth,
		sem:            make(chan struct{}, defaultMaxConcurrency),
		jobs:           make(map[string]*job),
		inFlight:       make(map[string]string),
		lastSuccess:    make(map[string]time.Time),
		lastStaleNorm:  make(map[string]time.Time),
	}
}

// SetProbeFunc overrides upstream probing (tests).
func (s *Service) SetProbeFunc(fn ProbeFunc) {
	if s == nil {
		return
	}
	s.probeFn = fn
}

// SetUpsertStatusFunc overrides latest-status persistence (tests).
func (s *Service) SetUpsertStatusFunc(fn func(usage.AIAccountStatusRecord) error) {
	if s == nil {
		return
	}
	s.upsertStatus = fn
}

// SetReconcileQuotaFunc overrides runtime quota reconcile (tests).
func (s *Service) SetReconcileQuotaFunc(fn func(ctx context.Context, authID string) (bool, error)) {
	if s == nil {
		return
	}
	s.reconcileQuota = fn
}

// SetNormalizeStaleFunc overrides stale refresh normalization (tests).
func (s *Service) SetNormalizeStaleFunc(fn func(tenantID string, olderThan time.Duration) (int64, error)) {
	if s == nil {
		return
	}
	s.normalizeStale = fn
}

// SetMaxConcurrency adjusts the global probe semaphore (tests).
func (s *Service) SetMaxConcurrency(n int) {
	if s == nil || n < 1 {
		return
	}
	s.maxConcurrency = n
	s.sem = make(chan struct{}, n)
}

func (s *Service) StartRefresh(tenantID string, req RefreshRequest) RefreshAccepted {
	s.purgeExpiredJobs()
	tenantID = strings.TrimSpace(tenantID)
	// Refresh boundary: allow stale cleanup without waiting for GET throttle alone.
	s.maybeNormalizeStaleRefresh(tenantID)
	auths := s.listAuths(tenantID)
	byIndex := make(map[string]*coreauth.Auth, len(auths))
	bySubject := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		byIndex[auth.Index] = auth
		if id := usage.ResolveAuthSubjectIdentity(auth); id != nil && id.ID != "" {
			// Prefer enabled/usable credential when multiple aliases share one subject.
			bySubject[id.ID] = preferAuthRepresentative(bySubject[id.ID], auth)
		}
	}

	// subjectID -> best representative auth (prefer usable over disabled).
	picks := make(map[string]*coreauth.Auth)
	add := func(auth *coreauth.Auth) {
		if auth == nil {
			return
		}
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID == "" {
			return
		}
		picks[identity.ID] = preferAuthRepresentative(picks[identity.ID], auth)
	}
	for _, idx := range req.AuthIndexes {
		add(byIndex[strings.TrimSpace(idx)])
	}
	for _, sid := range req.AuthSubjectIDs {
		add(bySubject[strings.TrimSpace(sid)])
	}
	if len(req.AuthIndexes) == 0 && len(req.AuthSubjectIDs) == 0 {
		for _, auth := range auths {
			add(auth)
		}
	}

	selected := make([]*coreauth.Auth, 0, len(picks))
	subjectIDs := make([]string, 0, len(picks))
	for sid, auth := range picks {
		selected = append(selected, auth)
		subjectIDs = append(subjectIDs, sid)
	}

	// Batch-load recent success outside s.mu so GetJob/other refreshes are not blocked
	// by N small-table queries under the service lock.
	recentBySubject := map[string]time.Time{}
	if !req.Force && len(subjectIDs) > 0 {
		if rows, err := usage.ListAIAccountStatusForTenant(tenantID, subjectIDs); err == nil {
			nowCheck := time.Now()
			for _, rec := range rows {
				if rec.RefreshState != string(RefreshSuccess) || rec.UpstreamCheckedAt == nil {
					continue
				}
				if nowCheck.Sub(*rec.UpstreamCheckedAt) < accountRefreshMinGap {
					recentBySubject[rec.AuthSubjectID] = *rec.UpstreamCheckedAt
				}
			}
		}
	}

	now := time.Now().UTC()
	j := &job{
		ID:        uuid.NewString(),
		TenantID:  tenantID,
		State:     "running",
		CreatedAt: now,
		UpdatedAt: now,
		Results:   make(map[string]*AccountRefreshResult),
	}

	accepted := 0
	deduped := 0
	skipped := make([]string, 0)
	workAuths := make([]*coreauth.Auth, 0)

	s.mu.Lock()
	for _, auth := range selected {
		identity := usage.ResolveAuthSubjectIdentity(auth)
		subjectID := identity.ID
		key := flightKey(tenantID, subjectID)

		// Disabled credentials are represented by the auth manager itself; probing
		// them only creates avoidable upstream failures and cannot make them active.
		// (Representative pick already preferred enabled aliases when any exist.)
		if !isAuthUsable(auth) {
			skipped = append(skipped, auth.Index)
			j.Results[subjectID] = &AccountRefreshResult{
				AuthIndex:     auth.Index,
				AuthSubjectID: subjectID,
				State:         RefreshSuccess,
				ErrorCode:     "disabled",
				ErrorMessage:  "skipped: account disabled",
				UpdatedAt:     now,
			}
			j.order = append(j.order, subjectID)
			continue
		}

		// force never bypasses in-flight singleflight/dedupe.
		if existingJob, busy := s.inFlight[key]; busy {
			deduped++
			j.Results[subjectID] = &AccountRefreshResult{
				AuthIndex:     auth.Index,
				AuthSubjectID: subjectID,
				State:         RefreshError,
				ErrorCode:     "deduplicated",
				ErrorMessage:  "refresh already in progress for job " + existingJob,
				UpdatedAt:     now,
			}
			j.order = append(j.order, subjectID)
			continue
		}

		if !req.Force {
			// Combine lock-external DB batch with lock-local success memory so a
			// probe that finished after the batch read still respects min-gap.
			recent, ok := recentBySubject[subjectID]
			if mem, memOK := s.lastSuccess[key]; memOK && (now.Sub(mem) < accountRefreshMinGap) {
				if !ok || mem.After(recent) {
					recent, ok = mem, true
				}
			}
			if ok {
				skipped = append(skipped, auth.Index)
				j.Results[subjectID] = &AccountRefreshResult{
					AuthIndex:     auth.Index,
					AuthSubjectID: subjectID,
					State:         RefreshSuccess,
					ErrorCode:     "fresh",
					ErrorMessage:  "skipped: recently refreshed",
					UpdatedAt:     recent,
				}
				j.order = append(j.order, subjectID)
				continue
			}
		}

		s.inFlight[key] = j.ID
		j.Results[subjectID] = &AccountRefreshResult{
			AuthIndex:     auth.Index,
			AuthSubjectID: subjectID,
			State:         RefreshQueued,
			UpdatedAt:     now,
		}
		j.order = append(j.order, subjectID)
		workAuths = append(workAuths, auth)
		accepted++
	}
	// Job with only terminal items is already completed.
	if accepted == 0 {
		j.State = "completed"
	}
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Persist queued only for truly accepted accounts (partial update).
	for _, auth := range workAuths {
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil {
			continue
		}
		_ = usage.UpdateAIAccountRefreshState(tenantID, identity.ID, auth.Index, auth.Provider, string(RefreshQueued), string(auth.Status), "", "")
	}

	if accepted > 0 {
		go s.runJob(j.ID, tenantID, workAuths)
	}

	return RefreshAccepted{
		JobID:        j.ID,
		Accepted:     accepted,
		Deduplicated: deduped,
		Skipped:      skipped,
	}
}

func isAuthUsable(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return !auth.Disabled && auth.Status != coreauth.StatusDisabled
}

// needsRuntimeQuotaReconcile reports whether local scheduler state has an active
// quota cooldown that still needs ProbeQuotaRecovery. Normal accounts return false
// so status refresh performs only the management probe (one upstream call).
func needsRuntimeQuotaReconcile(auth *coreauth.Auth) bool {
	if auth == nil || auth.Disabled || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	now := time.Now()
	if auth.Unavailable && auth.Quota.Exceeded && auth.NextRetryAfter.After(now) {
		return true
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Unavailable && state.Quota.Exceeded && state.NextRetryAfter.After(now) {
			return true
		}
	}
	return false
}

func (s *Service) maybeNormalizeStaleRefresh(tenantID string) {
	if s == nil {
		return
	}
	tenantID = strings.TrimSpace(tenantID)
	now := time.Now()
	s.staleNormMu.Lock()
	if last, ok := s.lastStaleNorm[tenantID]; ok && now.Sub(last) < staleNormalizeInterval {
		s.staleNormMu.Unlock()
		return
	}
	s.lastStaleNorm[tenantID] = now
	s.staleNormMu.Unlock()

	fn := s.normalizeStale
	if fn == nil {
		fn = usage.NormalizeStaleAIAccountRefreshStates
	}
	_, _ = fn(tenantID, staleRefreshThreshold)
}

// preferAuthRepresentative keeps an enabled credential over a disabled alias for the same subject.
func preferAuthRepresentative(current, candidate *coreauth.Auth) *coreauth.Auth {
	if candidate == nil {
		return current
	}
	if current == nil {
		return candidate
	}
	if !isAuthUsable(current) && isAuthUsable(candidate) {
		return candidate
	}
	return current
}

func (s *Service) runJob(jobID, tenantID string, auths []*coreauth.Auth) {
	var wg sync.WaitGroup
	for _, auth := range auths {
		auth := auth
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID == "" {
			continue
		}
		subjectID := identity.ID
		key := flightKey(tenantID, subjectID)

		wg.Add(1)
		go func() {
			defer wg.Done()
			// Global semaphore across all jobs.
			s.sem <- struct{}{}
			defer func() { <-s.sem }()

			_, _, _ = s.sf.Do(key, func() (any, error) {
				s.refreshOne(jobID, tenantID, auth, subjectID)
				return nil, nil
			})

			s.mu.Lock()
			if s.inFlight[key] == jobID {
				delete(s.inFlight, key)
			}
			// Record success under the same lock so force=false cannot TOCTOU-miss
			// a probe that finished between lock-external DB batch and lock acquire.
			if j := s.jobs[jobID]; j != nil {
				if r := j.Results[subjectID]; r != nil && r.State == RefreshSuccess && r.ErrorCode == "" {
					s.lastSuccess[key] = r.UpdatedAt
					if s.lastSuccess[key].IsZero() {
						s.lastSuccess[key] = time.Now().UTC()
					}
				}
			}
			s.mu.Unlock()
		}()
	}
	wg.Wait()

	s.mu.Lock()
	if j := s.jobs[jobID]; j != nil {
		j.State = "completed"
		j.UpdatedAt = time.Now().UTC()
	}
	s.mu.Unlock()
}

func (s *Service) refreshOne(jobID, tenantID string, auth *coreauth.Auth, subjectID string) {
	now := time.Now().UTC()
	s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
		r.State = RefreshRunning
		r.UpdatedAt = now
	})
	_ = usage.UpdateAIAccountRefreshState(tenantID, subjectID, auth.Index, auth.Provider, string(RefreshRunning), string(auth.Status), "", "")

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	// Status probe already hits the provider quota endpoint for codex/gemini-cli/etc.
	// Only run ReconcileQuota when the credential has an active runtime quota cooldown,
	// so normal refreshes do not double-call the same upstream URL.
	if needsRuntimeQuotaReconcile(auth) {
		if s.reconcileQuota != nil {
			_, _ = s.reconcileQuota(ctx, auth.ID)
		} else if s.authManager != nil && strings.TrimSpace(auth.ID) != "" {
			_, _ = s.authManager.ReconcileQuota(ctx, auth.ID)
		}
	}

	var probe ProbeResult
	var probeErr error
	var apiTools *managementapitools.Service
	if s.apiToolsFor != nil {
		apiTools = s.apiToolsFor(tenantID)
	}
	probeFn := s.probeFn
	if probeFn == nil {
		probeFn = probeAuth
	}
	if apiTools != nil {
		probe, probeErr = probeFn(ctx, apiTools, s.cfg, auth)
	} else {
		probeErr = fmt.Errorf("api tools unavailable")
	}

	checked := time.Now().UTC()
	if probeErr != nil {
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"probe_failed", probeErr.Error(), checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "probe_failed"
			r.ErrorMessage = sanitizeMsg(probeErr.Error())
			r.UpdatedAt = checked
		})
		return
	}

	if probe.Unsupported {
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"unsupported_probe", probe.UnsupportedReason, checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "unsupported_probe"
			r.ErrorMessage = sanitizeMsg(probe.UnsupportedReason)
			r.UpdatedAt = checked
		})
		return
	}

	healthStatus := strings.TrimSpace(probe.Health)
	if healthStatus == "" || healthStatus == "ok" {
		healthStatus = string(auth.Status)
	}
	record := usage.AIAccountStatusRecord{
		TenantID:               tenantID,
		AuthSubjectID:          subjectID,
		AuthIndex:              auth.Index,
		Provider:               auth.Provider,
		RefreshState:           string(RefreshSuccess),
		HealthStatus:           healthStatus,
		PlanType:               firstNonEmpty(probe.PlanType, metadataString(auth, "plan_type", "planType")),
		Quotas:                 probe.Quotas,
		ResetCreditCount:       probe.ResetCreditCount,
		ResetCreditExpirations: probe.ResetCreditExpirations,
		UpstreamCheckedAt:      &checked,
		UpdatedAt:              checked,
	}
	if len(probe.Quotas) > 0 {
		daily := make(map[string]*float64, len(probe.Quotas))
		points := make([]usage.QuotaSnapshotPoint, 0, len(probe.Quotas))
		for _, q := range probe.Quotas {
			if q.Percent != nil {
				p := *q.Percent
				daily[q.QuotaKey] = &p
			}
			points = append(points, usage.QuotaSnapshotPoint{
				RecordedAt:    checked,
				AuthIndex:     auth.Index,
				Provider:      auth.Provider,
				QuotaKey:      q.QuotaKey,
				QuotaLabel:    q.QuotaLabel,
				Percent:       q.Percent,
				ResetAt:       q.ResetAt,
				WindowSeconds: q.WindowSeconds,
			})
		}
		_ = usage.RecordDailyQuotaSnapshotIdentityForTenant(tenantID, auth.Index, subjectID, auth.Provider, daily)
		_ = usage.RecordQuotaSnapshotPointsIdentityForTenant(tenantID, auth.Index, subjectID, auth.Provider, points)
	}

	// Latest-status persistence is the trust boundary: write failure must not be reported as success.
	upsert := s.upsertStatus
	if upsert == nil {
		upsert = usage.UpsertAIAccountStatus
	}
	if err := upsert(record); err != nil {
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"persist_failed", err.Error(), checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "persist_failed"
			r.ErrorMessage = sanitizeMsg(err.Error())
			r.UpdatedAt = checked
			r.Result = nil
		})
		return
	}
	// DB assigns version = previous+1 on conflict; never invent version from the in-memory draft.
	persisted, err := s.loadPersistedStatus(tenantID, subjectID)
	if err != nil || persisted == nil {
		msg := "persisted status reload failed"
		if err != nil {
			msg = err.Error()
		}
		_ = usage.UpdateAIAccountRefreshFailure(
			tenantID, subjectID, auth.Index, auth.Provider, string(auth.Status),
			"persist_reload_failed", msg, checked,
		)
		s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
			r.State = RefreshError
			r.ErrorCode = "persist_reload_failed"
			r.ErrorMessage = sanitizeMsg(msg)
			r.UpdatedAt = checked
			r.Result = nil
		})
		return
	}
	if s.invalidate != nil {
		s.invalidate(tenantID, auth.Index, subjectID)
	}

	// Progressive view uses DB-truth version/updated fields for frontend monotonic merge.
	view := s.viewFromPersistedRecord(tenantID, auth, *persisted)
	s.setResult(jobID, subjectID, func(r *AccountRefreshResult) {
		r.State = RefreshSuccess
		r.ErrorCode = ""
		r.ErrorMessage = ""
		r.UpdatedAt = checked
		r.Result = view
	})
}

func (s *Service) loadPersistedStatus(tenantID, subjectID string) (*usage.AIAccountStatusRecord, error) {
	rows, err := usage.ListAIAccountStatusForTenant(tenantID, []string{subjectID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("status row missing after upsert")
	}
	rec := rows[0]
	return &rec, nil
}

func (s *Service) viewFromPersistedRecord(tenantID string, auth *coreauth.Auth, record usage.AIAccountStatusRecord) *AccountStatusView {
	cycleStarts, _ := usage.QueryLatestWeeklyQuotaCyclesBatch(tenantID, []string{record.AuthSubjectID}, primaryWeeklyKeys(auth.Provider))
	summaries, _ := usage.QueryAuthSubjectUsageSummaries(tenantID, []string{record.AuthSubjectID}, cycleStarts)
	view := statusViewFromRecord(record, auth, summaries[record.AuthSubjectID])
	return &view
}

func (s *Service) setResult(jobID, subjectID string, fn func(*AccountRefreshResult)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil {
		return
	}
	r := j.Results[subjectID]
	if r == nil {
		r = &AccountRefreshResult{AuthSubjectID: subjectID}
		j.Results[subjectID] = r
	}
	fn(r)
	j.UpdatedAt = time.Now().UTC()
}

func (s *Service) GetJob(tenantID, jobID string) (JobSnapshot, bool) {
	s.purgeExpiredJobs()
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil || j.TenantID != strings.TrimSpace(tenantID) {
		return JobSnapshot{}, false
	}
	snap := JobSnapshot{
		JobID:     j.ID,
		TenantID:  j.TenantID,
		State:     j.State,
		CreatedAt: j.CreatedAt,
		UpdatedAt: j.UpdatedAt,
		Results:   make([]AccountRefreshResult, 0, len(j.order)),
	}
	for _, sid := range j.order {
		r := j.Results[sid]
		if r == nil {
			continue
		}
		snap.Results = append(snap.Results, *r)
		snap.Total++
		switch {
		case r.ErrorCode == "deduplicated" || r.ErrorCode == "fresh":
			snap.Completed++
		case r.State == RefreshSuccess:
			snap.Completed++
		case r.State == RefreshError:
			snap.Failed++
			snap.Completed++
		}
	}
	return snap, true
}

func (s *Service) ListStatus(tenantID string, authIndexes, authSubjectIDs []string) (StatusListResponse, error) {
	tenantID = strings.TrimSpace(tenantID)
	// Best-effort stale cleanup: throttled per tenant so GET stays read-mostly.
	s.maybeNormalizeStaleRefresh(tenantID)

	auths := s.listAuths(tenantID)
	indexFilter := make(map[string]struct{})
	for _, idx := range authIndexes {
		if v := strings.TrimSpace(idx); v != "" {
			indexFilter[v] = struct{}{}
		}
	}
	subjectFilter := make(map[string]struct{})
	for _, sid := range authSubjectIDs {
		if v := strings.TrimSpace(sid); v != "" {
			subjectFilter[v] = struct{}{}
		}
	}
	filterOn := len(indexFilter) > 0 || len(subjectFilter) > 0

	wanted := make(map[string]*coreauth.Auth)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID == "" {
			continue
		}
		if filterOn {
			_, okIdx := indexFilter[auth.Index]
			_, okSub := subjectFilter[identity.ID]
			if !okIdx && !okSub {
				continue
			}
		}
		// Prefer enabled/usable alias when multiple credentials share one subject.
		wanted[identity.ID] = preferAuthRepresentative(wanted[identity.ID], auth)
	}
	subjectIDs := make([]string, 0, len(wanted))
	for sid := range wanted {
		subjectIDs = append(subjectIDs, sid)
	}
	// Empty filter match / empty tenant catalog: do not call store with empty IDs
	// (empty means "all subjects for tenant" and would scan the whole status table).
	if len(subjectIDs) == 0 {
		return StatusListResponse{Items: []AccountStatusView{}}, nil
	}

	statusRows, err := usage.ListAIAccountStatusForTenant(tenantID, subjectIDs)
	if err != nil {
		return StatusListResponse{}, err
	}
	statusBySubject := make(map[string]usage.AIAccountStatusRecord, len(statusRows))
	for _, row := range statusRows {
		statusBySubject[row.AuthSubjectID] = row
	}

	// One batch cycle query for all preferred weekly keys used by present providers.
	prefKeys := make([]string, 0)
	prefSeen := map[string]struct{}{}
	for _, auth := range wanted {
		for _, k := range primaryWeeklyKeys(auth.Provider) {
			if _, ok := prefSeen[k]; ok {
				continue
			}
			prefSeen[k] = struct{}{}
			prefKeys = append(prefKeys, k)
		}
	}
	cycleStart, err := usage.QueryLatestWeeklyQuotaCyclesBatch(tenantID, subjectIDs, prefKeys)
	if err != nil {
		return StatusListResponse{}, err
	}
	summaries, err := usage.QueryAuthSubjectUsageSummaries(tenantID, subjectIDs, cycleStart)
	if err != nil {
		return StatusListResponse{}, err
	}

	items := make([]AccountStatusView, 0, len(wanted))
	for _, sid := range subjectIDs {
		auth := wanted[sid]
		row, has := statusBySubject[sid]
		if !has {
			items = append(items, AccountStatusView{
				AuthSubjectID: sid,
				AuthIndex:     auth.Index,
				Provider:      auth.Provider,
				RefreshState:  string(RefreshIdle),
				HealthStatus:  string(auth.Status),
				PlanType:      metadataString(auth, "plan_type", "planType"),
				Quotas:        []usage.QuotaWindowDTO{},
				Usage:         summaries[sid],
			})
			continue
		}
		items = append(items, statusViewFromRecord(row, auth, summaries[sid]))
	}
	return StatusListResponse{Items: items}, nil
}

func statusViewFromRecord(row usage.AIAccountStatusRecord, auth *coreauth.Auth, summary usage.AuthSubjectUsageSummary) AccountStatusView {
	view := AccountStatusView{
		AuthSubjectID:          row.AuthSubjectID,
		AuthIndex:              row.AuthIndex,
		Provider:               row.Provider,
		RefreshState:           row.RefreshState,
		HealthStatus:           row.HealthStatus,
		PlanType:               row.PlanType,
		RestrictionSummary:     row.RestrictionSummary,
		ErrorSummary:           row.ErrorSummary,
		ErrorCode:              row.ErrorCode,
		ErrorMessage:           row.ErrorMessage,
		Quotas:                 row.Quotas,
		ResetCreditCount:       row.ResetCreditCount,
		ResetCreditExpirations: row.ResetCreditExpirations,
		Usage:                  summary,
		UpstreamCheckedAt:      row.UpstreamCheckedAt,
		UsageUpdatedAt:         row.UsageUpdatedAt,
		ExpiresAt:              row.ExpiresAt,
		Version:                row.Version,
		UpdatedAt:              timePointer(row.UpdatedAt),
	}
	if auth != nil {
		if view.AuthIndex == "" {
			view.AuthIndex = auth.Index
		}
		if view.Provider == "" {
			view.Provider = auth.Provider
		}
		if view.HealthStatus == "" {
			view.HealthStatus = string(auth.Status)
		}
		if view.PlanType == "" {
			view.PlanType = metadataString(auth, "plan_type", "planType")
		}
	}
	if view.Quotas == nil {
		view.Quotas = []usage.QuotaWindowDTO{}
	}
	if view.Usage.AuthSubjectID == "" {
		view.Usage.AuthSubjectID = row.AuthSubjectID
	}
	if view.UsageUpdatedAt == nil && !summary.UpdatedAt.IsZero() {
		view.UsageUpdatedAt = timePointer(summary.UpdatedAt)
	}
	if view.Usage.WeeklyQuotaUsed == nil && auth != nil {
		view.Usage.WeeklyQuotaUsed = weeklyUsedFromQuotas(row.Quotas, primaryWeeklyKeys(auth.Provider)...)
	}
	return view
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func weeklyUsedFromQuotas(quotas []usage.QuotaWindowDTO, preferred ...string) *float64 {
	pref := make(map[string]struct{}, len(preferred))
	for _, k := range preferred {
		pref[k] = struct{}{}
	}
	for i := range quotas {
		q := &quotas[i]
		if q.Percent == nil {
			continue
		}
		if len(pref) > 0 {
			if _, ok := pref[q.QuotaKey]; !ok {
				continue
			}
		}
		used := 100 - *q.Percent
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		return &used
	}
	return nil
}

func primaryWeeklyKeys(provider string) []string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return []string{"seven_day"}
	case "codex", "kimi":
		return []string{"code_week"}
	case "xai", "grok":
		return []string{"weekly_limit"}
	default:
		return nil
	}
}

func (s *Service) listAuths(tenantID string) []*coreauth.Auth {
	if s == nil || s.authManager == nil {
		return nil
	}
	return s.authManager.ListForTenant(tenantID)
}

func (s *Service) purgeExpiredJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Drop stale success memory outside min-gap so the map stays bounded.
	for key, at := range s.lastSuccess {
		if now.Sub(at) >= accountRefreshMinGap {
			delete(s.lastSuccess, key)
		}
	}
	for id, j := range s.jobs {
		if now.Sub(j.UpdatedAt) > jobTTL {
			delete(s.jobs, id)
		}
	}
	for key, jobID := range s.inFlight {
		if _, ok := s.jobs[jobID]; !ok {
			delete(s.inFlight, key)
		}
	}
}

func flightKey(tenantID, subjectID string) string {
	return strings.TrimSpace(tenantID) + "|" + strings.TrimSpace(subjectID)
}

func sanitizeMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization:") {
		return "upstream request failed"
	}
	if len(msg) > 200 {
		return msg[:200]
	}
	return msg
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func metadataString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := auth.Metadata[key]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}
