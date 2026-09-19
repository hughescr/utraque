package providerreport

import (
	"context"
	"errors"
	"math"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/referenceprice"
	"github.com/hughescr/utraque/internal/usagehistory"
)

// errCredentialUnavailable and errNotConfigured are the two ways readQuotas
// declines to read a provider without going upstream. safeError classifies
// them with errors.Is into CodeCredentialUnavailable and
// CodeConfigurationError; a reader's own failures arrive as
// *providerquota.Error instead.
var (
	errCredentialUnavailable = errors.New("credential unavailable")
	errNotConfigured         = errors.New("provider not configured")
)

type credentials struct {
	anthropicToken string
	codex          auth.Credential
}

type quotaResult struct {
	obs providerquota.Observation
	err error
}

type priceResult struct {
	snapshot referenceprice.Snapshot
	err      error
}

func (h *Handler) collect(ctx context.Context, creds credentials, since, until time.Time) Report {
	started := h.now().UTC()
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	type historyResult struct {
		report usagehistory.Report
		err    error
	}
	historyDone := make(chan historyResult, 1)
	go func() {
		report, err := h.history.Collect(ctx, since, until)
		historyDone <- historyResult{report: report, err: err}
	}()
	pricesDone := make(chan priceResult, 1)
	go func() {
		if h.prices == nil {
			pricesDone <- priceResult{}
			return
		}
		snapshot, err := h.prices.Read(ctx)
		pricesDone <- priceResult{snapshot: snapshot, err: err}
	}()
	quotas := h.readQuotas(ctx, creds)
	historyResultValue := <-historyDone
	history, historyErr := historyResultValue.report, historyResultValue.err
	prices := <-pricesDone
	ended := h.now().UTC()

	r := Report{SchemaVersion: SchemaVersionV1, GeneratedAt: ended,
		CollectionStartedAt: started, CollectionEndedAt: ended,
		HistoryRange: DateRange{Since: since, Until: until}}
	for _, id := range reportedLegs {
		pr := h.buildProvider(id, ended, quotas[id], prices, history, historyErr, since, until)
		r.Providers = append(r.Providers, pr)
	}
	r.UnattributedHistory = aggregateModels(history.Daily, leg.Unknown, since, until)
	return r
}

// reportedLegs is the order of the report's providers array: one section per
// leg, whether or not that leg is configured.
var reportedLegs = []leg.ID{leg.Anthropic, leg.Codex, leg.DeepSeek}

func (h *Handler) readQuotas(ctx context.Context, creds credentials) map[leg.ID]quotaResult {
	out := make(map[leg.ID]quotaResult, len(reportedLegs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	read := func(id leg.ID, fn func() (providerquota.Observation, error)) {
		defer wg.Done()
		o, err := fn()
		mu.Lock()
		out[id] = quotaResult{o, err}
		mu.Unlock()
	}
	wg.Add(len(reportedLegs))
	go read(leg.Anthropic, func() (providerquota.Observation, error) {
		if h.anthropic == nil || creds.anthropicToken == "" {
			return providerquota.Observation{}, errCredentialUnavailable
		}
		return h.anthropic.Read(ctx, creds.anthropicToken)
	})
	go read(leg.DeepSeek, func() (providerquota.Observation, error) {
		if h.deepseek == nil {
			return providerquota.Observation{}, errNotConfigured
		}
		return h.deepseek.Read(ctx)
	})
	go read(leg.Codex, func() (providerquota.Observation, error) {
		if h.codex == nil || h.codexSource == nil || creds.codex.AccountID == "" {
			return providerquota.Observation{}, errCredentialUnavailable
		}
		return h.codex.ReadCredential(ctx, h.codexSource, creds.codex)
	})
	wg.Wait()
	return out
}

// buildProvider assembles one leg's report section: its quota observation or
// error, the history rows usagehistory attributed to the same leg, and the
// reference prices filed under it.
func (h *Handler) buildProvider(id leg.ID, ended time.Time, quota quotaResult, prices priceResult, history usagehistory.Report, historyErr error, since, until time.Time) ProviderReport {
	pr := ProviderReport{Provider: id, LastAttempt: ended, Errors: []ReportError{}}
	var quotaErr *providerquota.Error
	if errors.As(quota.err, &quotaErr) && !quotaErr.AttemptedAt.IsZero() {
		pr.LastAttempt = quotaErr.AttemptedAt
	}
	if quota.err == nil {
		o := quota.obs
		pr.Quota = &o
	}
	if quota.err != nil {
		pr.Errors = append(pr.Errors, safeError(SectionQuota, quota.err))
	}

	rows30 := aggregateModels(history.Daily, id, since, until)
	rows7 := aggregateModels(history.Daily, id, until.AddDate(0, 0, -6), until)
	blocks := filterBlocks(history.Blocks, id)
	if len(rows30) > 0 || len(blocks) > 0 || historyErr == nil {
		pr.History = &HistorySummary{StartedAt: history.StartedAt, FinishedAt: history.FinishedAt,
			Source: history.Source, Coverage: history.Coverage, CostBasis: history.CostBasis,
			ToolVersion: history.ToolVersion, InvocationMode: history.InvocationMode, UnitPricesAvailable: false,
			UnitPriceUnavailableReason: history.UnitPriceUnavailableReason,
			SevenDays:                  PeriodSummary{Since: until.AddDate(0, 0, -6), Until: until, Models: rows7},
			ThirtyDays:                 PeriodSummary{Since: since, Until: until, Models: rows30}, Blocks: blocks, Issues: history.Issues}
	}
	if historyErr != nil {
		pr.Errors = append(pr.Errors, safeError(SectionHistory, historyErr))
	}
	if reference := h.referencePrices(id, rows30, prices.snapshot); reference != nil {
		pr.ReferencePrices = reference
	}
	if prices.err != nil {
		pr.Errors = append(pr.Errors, safeReferencePriceError(prices.err))
	}

	hasQuota := pr.Quota != nil
	hasHistory := pr.History != nil
	switch {
	case hasQuota && hasHistory && len(pr.Errors) == 0:
		pr.Status = StatusOK
	case hasQuota || hasHistory:
		pr.Status = StatusPartial
	default:
		pr.Status = StatusError
	}
	if pr.Status != StatusError {
		t := ended
		pr.LastSuccess = &t
	}
	if id == leg.Anthropic {
		pr.Calibration = &Calibration{UnavailableReason: ReasonPairedMeasurementUnavailable}
		if h.planLabel != "" || h.planMultiplier != nil {
			pr.ConfiguredPlan = &ConfiguredPlan{Label: h.planLabel, Multiplier: h.planMultiplier, Source: "configured"}
		}
	}
	if id == leg.DeepSeek {
		pr.Remaining = estimateDeepSeek(quota.obs, rows30)
	}
	return pr
}

func (h *Handler) referencePrices(provider leg.ID, history []ModelStats, snapshot referenceprice.Snapshot) *PriceSnapshot {
	if len(snapshot.Models) == 0 {
		return nil
	}
	observed := make(map[string]bool, len(history))
	for _, row := range history {
		model := strings.ToLower(strings.TrimSpace(row.Model))
		if h.normalizePrice != nil {
			model = strings.ToLower(strings.TrimSpace(h.normalizePrice(provider, row.Model)))
		}
		if model != "" {
			observed[model] = true
		}
	}
	eligibleModels := map[string]bool{}
	if h.eligiblePrices != nil {
		for _, model := range h.eligiblePrices(provider) {
			if model = strings.ToLower(strings.TrimSpace(model)); model != "" {
				eligibleModels[model] = true
			}
		}
	}
	out := PriceSnapshot{Source: snapshot.Source, ObservedAt: snapshot.ObservedAt,
		Stale: snapshot.Stale, Unit: snapshot.Unit, Models: []PriceRow{}}
	baseTier := false
	for _, price := range snapshot.Models {
		if price.Provider != provider {
			continue
		}
		eligible := eligibleModels[strings.ToLower(price.Model)]
		if !eligible && !observed[strings.ToLower(price.Model)] {
			continue
		}
		baseTier = baseTier || price.HasHigherTier
		out.Models = append(out.Models, PriceRow{ModelPrice: price, Eligible: eligible})
	}
	if len(out.Models) == 0 {
		return nil
	}
	if baseTier {
		out.Assumptions = append(out.Assumptions, "base_tier")
	}
	if provider == leg.Anthropic && slices.ContainsFunc(out.Models, func(row PriceRow) bool { return row.CacheWrite != nil }) {
		out.Assumptions = append(out.Assumptions, "cache_write_5m")
	}
	return &out
}

func safeReferencePriceError(err error) ReportError {
	var priceErr *referenceprice.Error
	if errors.As(err, &priceErr) {
		return ReportError{Section: SectionReferencePrices, Code: ErrorCodeFromPrice(priceErr.Code), Retryable: priceErr.Retryable, Message: "reference prices unavailable"}
	}
	return ReportError{Section: SectionReferencePrices, Code: CodeUnavailable, Retryable: true, Message: "reference prices unavailable"}
}

// safeError classifies a quota or history failure without exposing its text.
// The quota error's AttemptedAt travels with the report error (and also
// overrides ProviderReport.LastAttempt in buildProvider).
func safeError(section Section, err error) ReportError {
	var pe *providerquota.Error
	if errors.As(err, &pe) {
		re := ReportError{Section: section, Code: ErrorCodeFromQuota(pe.Code), Retryable: pe.Retryable, RetryAt: pe.RetryAt, Message: "provider reading unavailable"}
		if !pe.AttemptedAt.IsZero() {
			attempted := pe.AttemptedAt
			re.AttemptedAt = &attempted
		}
		return re
	}
	var ce *usagehistory.CollectError
	if errors.As(err, &ce) {
		return ReportError{Section: section, Code: ErrorCodeFromHistory(ce.Kind), Retryable: ce.Kind == usagehistory.ErrorTimeout || ce.Kind == usagehistory.ErrorCanceled || ce.Kind == usagehistory.ErrorCommand, Message: "local usage history unavailable"}
	}
	code := CodeUnavailable
	switch {
	case errors.Is(err, errCredentialUnavailable):
		code = CodeCredentialUnavailable
	case errors.Is(err, errNotConfigured):
		code = CodeConfigurationError
	}
	return ReportError{Section: section, Code: code, Message: "report source unavailable"}
}

type modelKey struct{ source, model string }
type modelAccum struct {
	row    ModelStats
	cost   float64
	priced bool
}

func aggregateModels(rows []usagehistory.DailyModelUsage, id leg.ID, since, until time.Time) []ModelStats {
	m := map[modelKey]*modelAccum{}
	for _, row := range rows {
		if row.InferredLeg != id || row.Date.Before(since) || row.Date.After(until) {
			continue
		}
		k := modelKey{row.Source, row.Model}
		a := m[k]
		if a == nil {
			a = &modelAccum{row: ModelStats{Source: row.Source, Model: row.Model, Provider: row.InferredLeg}, priced: true}
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
			a.row.CostStatus = usagehistory.CostAvailable
		} else {
			a.row.CostStatus = usagehistory.CostUnavailableOrUnpriced
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

func filterBlocks(blocks []usagehistory.BlockSummary, id leg.ID) []usagehistory.BlockSummary {
	out := []usagehistory.BlockSummary{}
	for _, b := range blocks {
		matched := false
		for _, m := range b.Models {
			if m.InferredLeg == id {
				matched = true
			}
		}
		if matched {
			out = append(out, b)
		}
	}
	return out
}

func estimateDeepSeek(obs providerquota.Observation, models []ModelStats) []RemainingEstimate {
	if obs.Available == nil || !*obs.Available {
		return nil
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
				e.UnavailableReason = ReasonUnsupportedBalanceCurrency
			} else if rate == nil || *rate <= 0 {
				e.UnavailableReason = ReasonHistoricalRateUnavailable
			} else if amount, ok := decimalFloat(b.Total); ok {
				t := amount / *rate
				e.Tokens = &t
			} else {
				e.UnavailableReason = ReasonInvalidBalanceDecimal
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
