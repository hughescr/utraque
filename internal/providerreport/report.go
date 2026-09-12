package providerreport

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/usagehistory"
)

type credentials struct {
	anthropicToken string
	codex          auth.Credential
}

type quotaResult struct {
	obs providerquota.Observation
	err error
}

func (h *Handler) collect(ctx context.Context, creds credentials, since, until time.Time) Report {
	started := h.now().UTC()
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	history, historyErr := h.history.Collect(ctx, since, until)
	quotas := h.readQuotas(ctx, creds)
	ended := h.now().UTC()

	r := Report{SchemaVersion: SchemaVersion, GeneratedAt: ended,
		CollectionStartedAt: started, CollectionEndedAt: ended,
		HistoryRange: DateRange{Since: since, Until: until}}
	providers := []struct {
		name string
		kind usagehistory.Provider
	}{{"anthropic", usagehistory.ProviderAnthropic}, {"codex", usagehistory.ProviderCodex}, {"deepseek", usagehistory.ProviderDeepSeek}}
	for _, p := range providers {
		pr := h.buildProvider(p.name, p.kind, ended, quotas[p.name], history, historyErr, since, until)
		r.Providers = append(r.Providers, pr)
	}
	r.UnattributedHistory = aggregateModels(history.Daily, usagehistory.ProviderUnknown, since, until)
	return r
}

func (h *Handler) readQuotas(ctx context.Context, creds credentials) map[string]quotaResult {
	out := make(map[string]quotaResult, 3)
	var mu sync.Mutex
	var wg sync.WaitGroup
	read := func(name string, fn func() (providerquota.Observation, error)) {
		defer wg.Done()
		o, err := fn()
		mu.Lock()
		out[name] = quotaResult{o, err}
		mu.Unlock()
	}
	wg.Add(3)
	go read("anthropic", func() (providerquota.Observation, error) {
		if h.anthropic == nil || creds.anthropicToken == "" {
			return providerquota.Observation{}, errors.New("credential unavailable")
		}
		return h.anthropic.Read(ctx, creds.anthropicToken)
	})
	go read("deepseek", func() (providerquota.Observation, error) {
		if h.deepseek == nil {
			return providerquota.Observation{}, errors.New("provider not configured")
		}
		return h.deepseek.Read(ctx)
	})
	go read("codex", func() (providerquota.Observation, error) {
		if h.codex == nil || h.codexSource == nil || creds.codex.AccountID == "" {
			return providerquota.Observation{}, errors.New("credential unavailable")
		}
		return h.codex.ReadCredential(ctx, h.codexSource, creds.codex)
	})
	wg.Wait()
	return out
}

func (h *Handler) buildProvider(name string, kind usagehistory.Provider, ended time.Time, quota quotaResult, history usagehistory.Report, historyErr error, since, until time.Time) ProviderReport {
	pr := ProviderReport{Provider: name, LastAttempt: ended, Errors: []ReportError{}}
	var quotaErr *providerquota.Error
	if errors.As(quota.err, &quotaErr) && !quotaErr.AttemptedAt.IsZero() {
		pr.LastAttempt = quotaErr.AttemptedAt
	}
	if quota.err == nil {
		o := quota.obs
		pr.QuotaAfter = &o
	}
	if quota.err != nil {
		pr.Errors = append(pr.Errors, safeError("quota_after", quota.err))
	}

	rows30 := aggregateModels(history.Daily, kind, since, until)
	rows7 := aggregateModels(history.Daily, kind, until.AddDate(0, 0, -6), until)
	blocks := filterBlocks(history.Blocks, kind)
	if len(rows30) > 0 || len(blocks) > 0 || historyErr == nil {
		pr.History = &HistorySummary{StartedAt: history.StartedAt, FinishedAt: history.FinishedAt,
			Source: history.Source, Coverage: history.Coverage, CostBasis: history.CostBasis,
			ToolVersion: history.ToolVersion, InvocationMode: history.InvocationMode, UnitPricesAvailable: false,
			UnitPriceUnavailableReason: history.UnitPriceUnavailableReason,
			SevenDays:                  PeriodSummary{Since: until.AddDate(0, 0, -6), Until: until, Models: rows7},
			ThirtyDays:                 PeriodSummary{Since: since, Until: until, Models: rows30}, Blocks: blocks, Issues: history.Issues}
	}
	if historyErr != nil {
		pr.Errors = append(pr.Errors, safeError("history", historyErr))
	}

	hasQuota := pr.QuotaBefore != nil || pr.QuotaAfter != nil
	hasHistory := pr.History != nil
	switch {
	case hasQuota && hasHistory && len(pr.Errors) == 0:
		pr.Status = "ok"
	case hasQuota || hasHistory:
		pr.Status = "partial"
	default:
		pr.Status = "error"
	}
	if pr.Status != "error" {
		t := ended
		pr.LastSuccess = &t
	}
	if name == "anthropic" {
		pr.Calibration = &Calibration{UnavailableReason: "paired_quota_measurement_unavailable"}
		if h.planLabel != "" || h.planMultiplier != nil {
			pr.ConfiguredPlan = &ConfiguredPlan{Label: h.planLabel, Multiplier: h.planMultiplier, Source: "configured"}
		}
	}
	if name == "deepseek" {
		pr.Remaining = estimateDeepSeek(quota.obs, rows30)
	}
	return pr
}

func safeError(section string, err error) ReportError {
	var pe *providerquota.Error
	if errors.As(err, &pe) {
		return ReportError{Section: section, Code: string(pe.Code), Retryable: pe.Retryable, RetryAt: pe.RetryAt, Message: "provider reading unavailable"}
	}
	var ce *usagehistory.CollectError
	if errors.As(err, &ce) {
		return ReportError{Section: section, Code: string(ce.Kind), Retryable: ce.Kind == usagehistory.ErrorTimeout || ce.Kind == usagehistory.ErrorCanceled || ce.Kind == usagehistory.ErrorCommand, Message: "local usage history unavailable"}
	}
	code := "unavailable"
	if strings.Contains(err.Error(), "credential") {
		code = "credential_unavailable"
	}
	if strings.Contains(err.Error(), "configured") {
		code = "configuration_error"
	}
	return ReportError{Section: section, Code: code, Message: "report source unavailable"}
}

type modelKey struct{ source, model string }
type modelAccum struct {
	row    ModelStats
	cost   float64
	priced bool
}

func aggregateModels(rows []usagehistory.DailyModelUsage, provider usagehistory.Provider, since, until time.Time) []ModelStats {
	m := map[modelKey]*modelAccum{}
	for _, row := range rows {
		if row.Provider != provider || row.Date.Before(since) || row.Date.After(until) {
			continue
		}
		k := modelKey{row.Source, row.Model}
		a := m[k]
		if a == nil {
			a = &modelAccum{row: ModelStats{Source: row.Source, Model: row.Model, Provider: row.Provider}, priced: true}
			m[k] = a
		}
		a.row.InputTokens += row.InputTokens
		a.row.OutputTokens += row.OutputTokens
		a.row.CacheCreationTokens += row.CacheCreationTokens
		a.row.CacheReadTokens += row.CacheReadTokens
		a.row.TotalTokens += row.TotalTokens
		if row.CostStatus != usagehistory.CostAvailable || row.CostUSD == nil {
			a.priced = false
		} else {
			a.cost += *row.CostUSD
		}
	}
	out := make([]ModelStats, 0, len(m))
	for _, a := range m {
		if a.priced && a.row.TotalTokens > 0 {
			cost := a.cost
			rate := cost / float64(a.row.TotalTokens)
			a.row.CostUSD, a.row.HistoricalEffectiveUSDToken = &cost, &rate
			a.row.CostStatus = "available"
		} else {
			a.row.CostStatus = "unavailable_or_unpriced"
			a.row.UnitPriceUnavailableReason = usagehistory.UnitPriceUnavailableCCUsage
		}
		out = append(out, a.row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model == out[j].Model {
			return out[i].Source < out[j].Source
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func filterBlocks(blocks []usagehistory.BlockSummary, provider usagehistory.Provider) []usagehistory.BlockSummary {
	out := []usagehistory.BlockSummary{}
	for _, b := range blocks {
		matched := false
		for _, m := range b.Models {
			if m.Provider == provider {
				matched = true
			}
		}
		if matched {
			out = append(out, b)
		}
	}
	return out
}

func calibrateAnthropic(pair *PairedMeasurement, history *HistorySummary) *Calibration {
	c := &Calibration{UnavailableReason: "no_contemporaneous_exact_five_hour_block"}
	if pair == nil || history == nil {
		return c
	}
	var qb, qa *providerquota.Quota
	for i := range pair.Before.Quotas {
		q := &pair.Before.Quotas[i]
		if q.DurationSeconds != nil && *q.DurationSeconds == 5*60*60 && q.Scope == nil && (q.Active == nil || *q.Active) {
			qb = q
			break
		}
	}
	for i := range pair.After.Quotas {
		q := &pair.After.Quotas[i]
		if q.DurationSeconds != nil && *q.DurationSeconds == 5*60*60 && q.Scope == nil && (q.Active == nil || *q.Active) && qb != nil && q.ID == qb.ID && q.Slot == qb.Slot && q.Kind == qb.Kind && q.Group == qb.Group {
			qa = q
			break
		}
	}
	if qb == nil || qa == nil {
		c.UnavailableReason = "five_hour_quota_unavailable"
		return c
	}
	if qa.UsedPercent <= 0 || qa.UsedPercent > 100 {
		c.UnavailableReason = "quota_percent_not_calibratable"
		return c
	}
	delta := qa.UsedPercent - qb.UsedPercent
	if delta < 0 {
		c.UnavailableReason = "quota_percent_decreased_during_collection"
		return c
	}
	if delta > 2 {
		c.UnavailableReason = "quota_changed_materially_during_collection"
		return c
	}
	if qb.ResetsAt == nil || qa.ResetsAt == nil || !qb.ResetsAt.Equal(*qa.ResetsAt) {
		c.UnavailableReason = "quota_reset_during_collection"
		return c
	}
	if !qa.ResetsAt.After(pair.EndedAt) {
		c.UnavailableReason = "quota_window_already_reset"
		return c
	}
	for i := range history.Blocks {
		b := &history.Blocks[i]
		if b.IsGap || b.TotalTokens == 0 || !b.EndTime.Equal(*qa.ResetsAt) || !b.StartTime.Equal(qa.ResetsAt.Add(-5*time.Hour)) {
			continue
		}
		allAnthropic := len(b.Models) > 0
		for _, m := range b.Models {
			if m.Provider != usagehistory.ProviderAnthropic {
				allAnthropic = false
				break
			}
		}
		if !allAnthropic {
			c.UnavailableReason = "block_contains_non_anthropic_models"
			continue
		}
		if history.StartedAt.Before(pair.StartedAt) || history.FinishedAt.After(pair.EndedAt) {
			c.UnavailableReason = "block_not_collected_within_quota_bracket"
			continue
		}
		last := b.EndTime
		if b.ActualEndTime != nil {
			last = *b.ActualEndTime
		}
		if last.After(pair.After.CollectedAt) {
			c.UnavailableReason = "block_activity_after_quota_reading"
			continue
		}
		q := qa.UsedPercent
		tokens := b.TotalTokens
		estimate := float64(tokens) * (100 - q) / q
		c = &Calibration{QuotaID: qa.ID, Block: b, UsedPercent: &q, ObservedTokens: &tokens, ConditionalRemaining: &estimate,
			Assumptions: []string{"local_logs_represent_quota_usage", "stable_quota_consumption_per_token_for_observed_workload"}}
		if len(b.Models) == 1 {
			c.Model = b.Models[0].Model
		}
		return c
	}
	return c
}

func estimateDeepSeek(obs providerquota.Observation, models []ModelStats) []RemainingEstimate {
	if obs.Available == nil || !*obs.Available {
		return nil
	}
	for _, restriction := range obs.SpendControls {
		if restriction.Reached {
			return []RemainingEstimate{{UnavailableReason: "spend_control_reached"}}
		}
	}
	type rateAccum struct {
		cost   float64
		tokens uint64
		valid  bool
	}
	rates := map[string]*rateAccum{}
	for _, m := range models {
		a := rates[m.Model]
		if a == nil {
			a = &rateAccum{valid: true}
			rates[m.Model] = a
		}
		if m.CostUSD == nil || m.HistoricalEffectiveUSDToken == nil {
			a.valid = false
			continue
		}
		a.cost += *m.CostUSD
		a.tokens += m.TotalTokens
	}
	modelNames := make([]string, 0, len(rates))
	for name := range rates {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	var out []RemainingEstimate
	for _, b := range obs.Balances {
		for _, model := range modelNames {
			a := rates[model]
			var rate *float64
			if a.valid && a.tokens > 0 {
				value := a.cost / float64(a.tokens)
				if value > 0 {
					rate = &value
				}
			}
			e := RemainingEstimate{Model: model, Currency: b.Currency, Balance: b.Total,
				HistoricalUSDToken: rate,
				Assumptions:        []string{"historical_api_reference_rate_matches_future_workload", "future_provider_prices_do_not_change"}}
			if b.Currency != "USD" {
				e.UnavailableReason = "unsupported_balance_currency"
			} else if rate == nil || *rate <= 0 {
				e.UnavailableReason = "historical_effective_rate_unavailable"
			} else if amount, ok := decimalFloat(b.Total); ok {
				t := amount / *rate
				e.Tokens = &t
			} else {
				e.UnavailableReason = "invalid_balance_decimal"
			}
			out = append(out, e)
		}
	}
	return out
}

func decimalFloat(s string) (float64, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, false
	}
	f, _ := r.Float64()
	return f, !math.IsInf(f, 0) && !math.IsNaN(f)
}
