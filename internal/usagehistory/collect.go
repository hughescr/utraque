package usagehistory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/leg"
)

var toolVersionPattern = regexp.MustCompile(`^(?:ccusage(?: version)?[[:space:]]+)?v?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)$`)

// Collect gathers ccusage daily history and Claude-log block summaries for the
// inclusive UTC date range. A partial Report is returned with an error when a
// report section cannot be collected or has an incompatible schema.
func (c *Collector) Collect(ctx context.Context, since, until time.Time) (Report, error) {
	report := Report{
		StartedAt:                  c.now().UTC(),
		SinceDate:                  utcDate(since),
		UntilDate:                  utcDate(until),
		InvocationMode:             c.invocationMode,
		Source:                     SourceCCUsage,
		Coverage:                   CoverageLocalOnly,
		CostBasis:                  CostBasisCalculatedAPIReference,
		UnitPriceUnavailableReason: UnitPriceUnavailableCCUsage,
		Daily:                      []DailyModelUsage{},
		Blocks:                     []BlockSummary{},
		Issues:                     []Issue{},
	}
	finish := func() { report.FinishedAt = c.now().UTC() }
	if report.UntilDate.Before(report.SinceDate) {
		finish()
		err := &CollectError{Section: "request", Kind: ErrorInvalidRequest, detail: "until date precedes since date"}
		report.Issues = append(report.Issues, issueFrom(err))
		return report, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	tempDir, configPath, err := makeControlledConfig()
	if err != nil {
		finish()
		ce := &CollectError{Section: "setup", Kind: ErrorCommand, detail: "could not create private tool configuration", cause: err}
		report.Issues = append(report.Issues, issueFrom(ce))
		return report, ce
	}
	defer os.RemoveAll(tempDir)

	base := commandSpec{
		name:      c.executable,
		dir:       tempDir,
		env:       allowlistedEnvironment(),
		maxOutput: c.maxOutput,
	}
	versionSpec := base
	versionSpec.args = []string{"--version"}
	if c.invocationMode == InvocationBunx {
		versionSpec.args = append([]string{c.packageArg}, versionSpec.args...)
	}
	versionOutput, err := c.runner.Run(ctx, versionSpec)
	if err != nil {
		finish()
		ce := classifyError("version", err)
		report.Issues = append(report.Issues, issueFrom(ce))
		return report, ce
	}
	report.ToolVersion, err = parseToolVersion(versionOutput)
	if err != nil {
		finish()
		ce := incompatibleError("version", err)
		report.Issues = append(report.Issues, issueFrom(ce))
		return report, ce
	}
	// Resolve "latest" once, then pin both reports to the observed semantic
	// version. This keeps a release appearing between subprocesses from mixing
	// schemas within one collection while future collections still refresh it.
	reportPackageArg := c.packageArg
	if c.invocationMode == InvocationBunx && c.requestedVersion == DefaultVersion {
		reportPackageArg = c.packageName + "@" + report.ToolVersion
	}

	dailySpec := base
	dailySpec.args = []string{
		"daily", "--json", "--by-agent", "--timezone", "UTC",
		"--since", report.SinceDate.Format("2006-01-02"),
		"--until", report.UntilDate.Format("2006-01-02"),
		"--no-offline", "--config", configPath,
	}
	if c.invocationMode == InvocationBunx {
		dailySpec.args = append([]string{reportPackageArg}, dailySpec.args...)
	}
	blocksSpec := base
	blocksSpec.args = []string{
		"claude", "blocks", "--json", "--mode", "calculate",
		"--timezone", "UTC", "--session-length", "5",
		"--since", report.SinceDate.Format("20060102"),
		"--until", report.UntilDate.Format("20060102"),
		"--config", configPath,
	}
	if c.invocationMode == InvocationBunx {
		blocksSpec.args = append([]string{reportPackageArg}, blocksSpec.args...)
	}

	var dailyOutput, blocksOutput []byte
	var dailyErr, blocksErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		dailyOutput, dailyErr = c.runner.Run(ctx, dailySpec)
	}()
	go func() {
		defer wg.Done()
		blocksOutput, blocksErr = c.runner.Run(ctx, blocksSpec)
	}()
	wg.Wait()

	var collectionErrors []error
	if dailyErr != nil {
		ce := classifyError("daily", dailyErr)
		report.Issues = append(report.Issues, issueFrom(ce))
		collectionErrors = append(collectionErrors, ce)
	} else if report.Daily, err = parseDaily(dailyOutput, report.SinceDate, report.UntilDate); err != nil {
		ce := incompatibleError("daily", err)
		report.Issues = append(report.Issues, issueFrom(ce))
		collectionErrors = append(collectionErrors, ce)
	}
	if blocksErr != nil {
		ce := classifyError("blocks", blocksErr)
		report.Issues = append(report.Issues, issueFrom(ce))
		collectionErrors = append(collectionErrors, ce)
	} else if report.Blocks, err = parseBlocks(blocksOutput); err != nil {
		ce := incompatibleError("blocks", err)
		report.Issues = append(report.Issues, issueFrom(ce))
		collectionErrors = append(collectionErrors, ce)
	}
	finish()
	return report, errors.Join(collectionErrors...)
}

func makeControlledConfig() (dir, path string, err error) {
	dir, err = os.MkdirTemp("", "utraque-usagehistory-")
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, "ccusage.json")
	const config = `{"defaults":{"mode":"calculate","pricingOverrides":{}}}`
	if err = os.WriteFile(path, []byte(config), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, path, nil
}

func allowlistedEnvironment() []string {
	keys := []string{
		"HOME", "PATH", "TMPDIR", "BUN_INSTALL",
		// ccusage uses these path-only overrides to find redirected local logs.
		// Authentication variables remain excluded by construction.
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR",
	}
	env := make([]string, 0, len(keys)+2)
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "NO_COLOR=1", "FORCE_COLOR=0")
}

func parseToolVersion(output []byte) (string, error) {
	text := strings.TrimSpace(string(output))
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("expected one semantic-version line")
	}
	match := toolVersionPattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return "", fmt.Errorf("expected a semantic version")
	}
	return match[1], nil
}

func incompatibleError(section string, err error) *CollectError {
	return &CollectError{Section: section, Kind: ErrorIncompatible, detail: err.Error(), cause: err}
}

func issueFrom(err *CollectError) Issue {
	message := err.detail
	if message == "" {
		message = string(err.Kind)
	}
	return Issue{Section: err.Section, Code: string(err.Kind), Missing: true, Message: message}
}

type rawTokenTotals struct {
	InputTokens         *uint64  `json:"inputTokens"`
	OutputTokens        *uint64  `json:"outputTokens"`
	CacheCreationTokens *uint64  `json:"cacheCreationTokens"`
	CacheReadTokens     *uint64  `json:"cacheReadTokens"`
	TotalTokens         *uint64  `json:"totalTokens"`
	TotalCost           *float64 `json:"totalCost"`
}

type rawModelBreakdown struct {
	ModelName           *string  `json:"modelName"`
	InputTokens         *uint64  `json:"inputTokens"`
	OutputTokens        *uint64  `json:"outputTokens"`
	CacheCreationTokens *uint64  `json:"cacheCreationTokens"`
	CacheReadTokens     *uint64  `json:"cacheReadTokens"`
	Cost                *float64 `json:"cost"`
}

type rawAgentDaily struct {
	Agent *string `json:"agent"`
	rawTokenTotals
	ModelsUsed      *[]string            `json:"modelsUsed"`
	ModelBreakdowns *[]rawModelBreakdown `json:"modelBreakdowns"`
}

type rawDaily struct {
	Agent  *string `json:"agent"`
	Period *string `json:"period"`
	rawTokenTotals
	ModelsUsed *[]string        `json:"modelsUsed"`
	Agents     *[]rawAgentDaily `json:"agents"`
}

type rawDailyReport struct {
	Daily  *[]rawDaily     `json:"daily"`
	Totals *rawTokenTotals `json:"totals"`
}

type dailyKey struct {
	date, source, model string
}

type dailyAccumulator struct {
	row          DailyModelUsage
	cost         float64
	costComplete bool
}

func parseDaily(data []byte, since, until time.Time) ([]DailyModelUsage, error) {
	var raw rawDailyReport
	if err := decodeOne(data, &raw); err != nil {
		return nil, err
	}
	if raw.Daily == nil || raw.Totals == nil {
		return nil, fmt.Errorf("missing daily or totals")
	}
	if err := validateTotals("totals", raw.Totals); err != nil {
		return nil, err
	}

	acc := make(map[dailyKey]*dailyAccumulator)
	var reportDays tokenVector
	for dayIndex, day := range *raw.Daily {
		path := fmt.Sprintf("daily[%d]", dayIndex)
		if day.Agent == nil || *day.Agent != "all" || day.Period == nil || day.Agents == nil || day.ModelsUsed == nil {
			return nil, fmt.Errorf("%s missing required unified fields", path)
		}
		if err := validateTotals(path, &day.rawTokenTotals); err != nil {
			return nil, err
		}
		date, err := time.Parse("2006-01-02", *day.Period)
		if err != nil || date.Format("2006-01-02") != *day.Period {
			return nil, fmt.Errorf("%s.period is not a UTC date", path)
		}
		if date.Before(since) || date.After(until) {
			return nil, fmt.Errorf("%s.period is outside requested range", path)
		}
		var dayAgents tokenVector
		for agentIndex, agent := range *day.Agents {
			agentPath := fmt.Sprintf("%s.agents[%d]", path, agentIndex)
			if agent.Agent == nil || agent.ModelBreakdowns == nil || agent.ModelsUsed == nil {
				return nil, fmt.Errorf("%s missing required fields", agentPath)
			}
			if err := validateTotals(agentPath, &agent.rawTokenTotals); err != nil {
				return nil, err
			}
			source := *agent.Agent
			if source == "" || strings.TrimSpace(source) != source {
				return nil, fmt.Errorf("%s.agent must be a nonempty source label", agentPath)
			}
			var agentModels tokenVector
			for modelIndex, model := range *agent.ModelBreakdowns {
				modelPath := fmt.Sprintf("%s.modelBreakdowns[%d]", agentPath, modelIndex)
				if err := addDailyModel(acc, date, source, model, modelPath); err != nil {
					return nil, err
				}
				if err := agentModels.add(modelTokenVector(model)); err != nil {
					return nil, fmt.Errorf("%s model token totals overflow", agentPath)
				}
			}
			if !agentModels.equal(totalsTokenVector(&agent.rawTokenTotals)) {
				return nil, fmt.Errorf("%s model breakdown tokens do not equal agent totals", agentPath)
			}
			if err := dayAgents.add(totalsTokenVector(&agent.rawTokenTotals)); err != nil {
				return nil, fmt.Errorf("%s agent token totals overflow", path)
			}
		}
		dayTotals := totalsTokenVector(&day.rawTokenTotals)
		if !dayAgents.equal(dayTotals) {
			return nil, fmt.Errorf("%s agent token totals do not equal day totals", path)
		}
		if err := reportDays.add(dayTotals); err != nil {
			return nil, fmt.Errorf("daily token totals overflow")
		}
	}
	if !reportDays.equal(totalsTokenVector(raw.Totals)) {
		return nil, fmt.Errorf("daily token totals do not equal report totals")
	}

	rows := make([]DailyModelUsage, 0, len(acc))
	for _, item := range acc {
		if item.costComplete {
			cost := item.cost
			item.row.CostUSD = &cost
			item.row.CostStatus = CostAvailable
		} else {
			item.row.CostStatus = CostUnavailableOrUnpriced
		}
		rows = append(rows, item.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Date.Equal(rows[j].Date) {
			return rows[i].Date.Before(rows[j].Date)
		}
		if rows[i].Source != rows[j].Source {
			return rows[i].Source < rows[j].Source
		}
		return rows[i].Model < rows[j].Model
	})
	return rows, nil
}

func addDailyModel(acc map[dailyKey]*dailyAccumulator, date time.Time, source string, raw rawModelBreakdown, path string) error {
	if raw.ModelName == nil || strings.TrimSpace(*raw.ModelName) == "" ||
		raw.InputTokens == nil || raw.OutputTokens == nil ||
		raw.CacheCreationTokens == nil || raw.CacheReadTokens == nil || raw.Cost == nil {
		return fmt.Errorf("%s missing required fields", path)
	}
	if !validCost(*raw.Cost) {
		return fmt.Errorf("%s.cost must be finite and non-negative", path)
	}
	total, err := sumTokens(*raw.InputTokens, *raw.OutputTokens, *raw.CacheCreationTokens, *raw.CacheReadTokens)
	if err != nil {
		return fmt.Errorf("%s token total overflows", path)
	}
	key := dailyKey{date: date.Format("2006-01-02"), source: source, model: *raw.ModelName}
	item := acc[key]
	complete := total == 0 || *raw.Cost > 0
	if item == nil {
		item = &dailyAccumulator{
			row:          DailyModelUsage{Date: date, Source: source, Model: *raw.ModelName, InferredLeg: classifyLeg(*raw.ModelName)},
			costComplete: complete,
		}
		acc[key] = item
	} else {
		item.costComplete = item.costComplete && complete
	}
	if item.row.InputTokens, err = addUint(item.row.InputTokens, *raw.InputTokens); err != nil {
		return fmt.Errorf("%s input token sum overflows", path)
	}
	if item.row.OutputTokens, err = addUint(item.row.OutputTokens, *raw.OutputTokens); err != nil {
		return fmt.Errorf("%s output token sum overflows", path)
	}
	if item.row.CacheCreationTokens, err = addUint(item.row.CacheCreationTokens, *raw.CacheCreationTokens); err != nil {
		return fmt.Errorf("%s cache creation token sum overflows", path)
	}
	if item.row.CacheReadTokens, err = addUint(item.row.CacheReadTokens, *raw.CacheReadTokens); err != nil {
		return fmt.Errorf("%s cache read token sum overflows", path)
	}
	if item.row.TotalTokens, err = addUint(item.row.TotalTokens, total); err != nil {
		return fmt.Errorf("%s total token sum overflows", path)
	}
	item.cost += *raw.Cost
	if math.IsInf(item.cost, 0) || math.IsNaN(item.cost) {
		return fmt.Errorf("%s cost sum is not finite", path)
	}
	return nil
}

func validateTotals(path string, raw *rawTokenTotals) error {
	if raw.InputTokens == nil || raw.OutputTokens == nil || raw.CacheCreationTokens == nil ||
		raw.CacheReadTokens == nil || raw.TotalTokens == nil || raw.TotalCost == nil {
		return fmt.Errorf("%s missing required totals", path)
	}
	if !validCost(*raw.TotalCost) {
		return fmt.Errorf("%s.totalCost must be finite and non-negative", path)
	}
	total, err := sumTokens(*raw.InputTokens, *raw.OutputTokens, *raw.CacheCreationTokens, *raw.CacheReadTokens)
	if err != nil || total != *raw.TotalTokens {
		return fmt.Errorf("%s.totalTokens does not equal its token categories", path)
	}
	return nil
}

type tokenVector struct {
	input, output, cacheCreation, cacheRead, total uint64
}

func totalsTokenVector(raw *rawTokenTotals) tokenVector {
	return tokenVector{
		input: *raw.InputTokens, output: *raw.OutputTokens,
		cacheCreation: *raw.CacheCreationTokens, cacheRead: *raw.CacheReadTokens,
		total: *raw.TotalTokens,
	}
}

func modelTokenVector(raw rawModelBreakdown) tokenVector {
	return tokenVector{
		input: *raw.InputTokens, output: *raw.OutputTokens,
		cacheCreation: *raw.CacheCreationTokens, cacheRead: *raw.CacheReadTokens,
		total: *raw.InputTokens + *raw.OutputTokens + *raw.CacheCreationTokens + *raw.CacheReadTokens,
	}
}

func (v *tokenVector) add(other tokenVector) error {
	values := [5]*uint64{&v.input, &v.output, &v.cacheCreation, &v.cacheRead, &v.total}
	addends := [5]uint64{other.input, other.output, other.cacheCreation, other.cacheRead, other.total}
	for index := range values {
		next, err := addUint(*values[index], addends[index])
		if err != nil {
			return err
		}
		*values[index] = next
	}
	return nil
}

func (v tokenVector) equal(other tokenVector) bool {
	return v == other
}

type rawBlockTokenCounts struct {
	InputTokens              *uint64 `json:"inputTokens"`
	OutputTokens             *uint64 `json:"outputTokens"`
	CacheCreationInputTokens *uint64 `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     *uint64 `json:"cacheReadInputTokens"`
}

type rawBlock struct {
	ID            *string              `json:"id"`
	StartTime     *string              `json:"startTime"`
	EndTime       *string              `json:"endTime"`
	ActualEndTime *string              `json:"actualEndTime"`
	IsActive      *bool                `json:"isActive"`
	IsGap         *bool                `json:"isGap"`
	Entries       *uint64              `json:"entries"`
	TokenCounts   *rawBlockTokenCounts `json:"tokenCounts"`
	TotalTokens   *uint64              `json:"totalTokens"`
	CostUSD       *float64             `json:"costUSD"`
	Models        *[]string            `json:"models"`
}

type rawBlocksReport struct {
	Blocks *[]rawBlock `json:"blocks"`
}

func parseBlocks(data []byte) ([]BlockSummary, error) {
	var raw rawBlocksReport
	if err := decodeOne(data, &raw); err != nil {
		return nil, err
	}
	if raw.Blocks == nil {
		return nil, fmt.Errorf("missing blocks")
	}
	blocks := make([]BlockSummary, 0, len(*raw.Blocks))
	for index, block := range *raw.Blocks {
		path := fmt.Sprintf("blocks[%d]", index)
		normalized, err := normalizeBlock(block, path)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, normalized)
	}
	sort.Slice(blocks, func(i, j int) bool {
		if !blocks[i].StartTime.Equal(blocks[j].StartTime) {
			return blocks[i].StartTime.Before(blocks[j].StartTime)
		}
		return blocks[i].ID < blocks[j].ID
	})
	return blocks, nil
}

func normalizeBlock(raw rawBlock, path string) (BlockSummary, error) {
	if raw.ID == nil || strings.TrimSpace(*raw.ID) == "" || raw.StartTime == nil || raw.EndTime == nil ||
		raw.IsActive == nil || raw.IsGap == nil || raw.Entries == nil || raw.TokenCounts == nil ||
		raw.TotalTokens == nil || raw.CostUSD == nil || raw.Models == nil {
		return BlockSummary{}, fmt.Errorf("%s missing required fields", path)
	}
	counts := raw.TokenCounts
	if counts.InputTokens == nil || counts.OutputTokens == nil || counts.CacheCreationInputTokens == nil || counts.CacheReadInputTokens == nil {
		return BlockSummary{}, fmt.Errorf("%s.tokenCounts missing required fields", path)
	}
	start, err := time.Parse(time.RFC3339Nano, *raw.StartTime)
	if err != nil {
		return BlockSummary{}, fmt.Errorf("%s.startTime is not RFC3339", path)
	}
	end, err := time.Parse(time.RFC3339Nano, *raw.EndTime)
	if err != nil || end.Before(start) {
		return BlockSummary{}, fmt.Errorf("%s.endTime is invalid", path)
	}
	var actualEnd *time.Time
	if raw.ActualEndTime != nil && *raw.ActualEndTime != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, *raw.ActualEndTime)
		if parseErr != nil {
			return BlockSummary{}, fmt.Errorf("%s.actualEndTime is not RFC3339", path)
		}
		parsed = parsed.UTC()
		actualEnd = &parsed
	}
	total, err := sumTokens(*counts.InputTokens, *counts.OutputTokens, *counts.CacheCreationInputTokens, *counts.CacheReadInputTokens)
	if err != nil || total != *raw.TotalTokens {
		return BlockSummary{}, fmt.Errorf("%s.totalTokens does not equal its token categories", path)
	}
	if !validCost(*raw.CostUSD) {
		return BlockSummary{}, fmt.Errorf("%s.costUSD must be finite and non-negative", path)
	}

	models := make([]BlockModel, 0, len(*raw.Models))
	seenModels := make(map[string]struct{})
	legs := make(map[leg.ID]struct{})
	for modelIndex, model := range *raw.Models {
		if strings.TrimSpace(model) == "" {
			return BlockSummary{}, fmt.Errorf("%s.models[%d] is empty", path, modelIndex)
		}
		if _, exists := seenModels[model]; exists {
			continue
		}
		seenModels[model] = struct{}{}
		inferred := classifyLeg(model)
		legs[inferred] = struct{}{}
		models = append(models, BlockModel{Model: model, InferredLeg: inferred})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })

	result := BlockSummary{
		ID: *raw.ID, Source: "claude", StartTime: start.UTC(), EndTime: end.UTC(), ActualEndTime: actualEnd,
		IsActive: *raw.IsActive, IsGap: *raw.IsGap, Entries: *raw.Entries,
		InputTokens: *counts.InputTokens, OutputTokens: *counts.OutputTokens,
		CacheCreationTokens: *counts.CacheCreationInputTokens, CacheReadTokens: *counts.CacheReadInputTokens,
		TotalTokens: *raw.TotalTokens, Models: models, MixedProvider: len(legs) > 1,
	}
	if result.TotalTokens > 0 && *raw.CostUSD == 0 {
		result.CostStatus = CostUnavailableOrUnpriced
	} else {
		cost := *raw.CostUSD
		result.CostUSD = &cost
		result.CostStatus = CostAvailable
	}
	return result, nil
}

func decodeOne(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid trailing JSON")
	}
	return nil
}

func sumTokens(values ...uint64) (uint64, error) {
	var total uint64
	var err error
	for _, value := range values {
		total, err = addUint(total, value)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func addUint(left, right uint64) (uint64, error) {
	if math.MaxUint64-left < right {
		return 0, fmt.Errorf("uint64 overflow")
	}
	return left + right, nil
}

func validCost(cost float64) bool {
	return cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0)
}
