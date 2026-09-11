package usagehistory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const representativeDaily = `{
  "daily": [{
    "agent": "all", "period": "2026-09-12",
    "inputTokens": 23, "outputTokens": 11, "cacheCreationTokens": 6, "cacheReadTokens": 9,
    "totalTokens": 49, "totalCost": 1.95,
    "modelsUsed": ["claude-sonnet-4-5", "gpt-5.6-sol", "deepseek-chat", "mystery-model"],
    "modelBreakdowns": [{"modelName":"outer-must-not-be-counted","inputTokens":23,"outputTokens":11,"cacheCreationTokens":6,"cacheReadTokens":9,"cost":1.95}],
    "agents": [
      {"agent":"claude","inputTokens":15,"outputTokens":8,"cacheCreationTokens":3,"cacheReadTokens":4,"totalTokens":30,"totalCost":1.25,
       "modelsUsed":["claude-sonnet-4-5","gpt-5.6-sol"],
       "modelBreakdowns":[
         {"modelName":"claude-sonnet-4-5","inputTokens":10,"outputTokens":2,"cacheCreationTokens":3,"cacheReadTokens":4,"cost":1.25},
         {"modelName":"gpt-5.6-sol","inputTokens":5,"outputTokens":6,"cacheCreationTokens":0,"cacheReadTokens":0,"cost":0}
       ]},
      {"agent":"codex","inputTokens":8,"outputTokens":3,"cacheCreationTokens":3,"cacheReadTokens":5,"totalTokens":19,"totalCost":0.70,
       "modelsUsed":["deepseek-chat","mystery-model"],
       "modelBreakdowns":[
         {"modelName":"deepseek-chat","inputTokens":7,"outputTokens":1,"cacheCreationTokens":2,"cacheReadTokens":3,"cost":0.5},
         {"modelName":"mystery-model","inputTokens":1,"outputTokens":2,"cacheCreationTokens":1,"cacheReadTokens":2,"cost":0.2}
       ]},
      {"agent":"other","inputTokens":0,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":0,"totalCost":0,
       "modelsUsed":[],"modelBreakdowns":[]}
    ]
  }],
  "totals": {"inputTokens":23,"outputTokens":11,"cacheCreationTokens":6,"cacheReadTokens":9,"totalTokens":49,"totalCost":1.95}
}`

const representativeBlocks = `{
  "blocks": [{
    "id":"block-1","startTime":"2026-09-12T10:00:00-04:00","endTime":"2026-09-12T15:00:00-04:00",
    "isActive":true,"isGap":false,"entries":2,
    "tokenCounts":{"inputTokens":1,"outputTokens":2,"cacheCreationInputTokens":3,"cacheReadInputTokens":4},
    "totalTokens":10,"costUSD":0,"models":["gpt-5.6-sol","claude-sonnet-4-5","gpt-5.6-sol"],
    "burnRate":{"tokensPerMinute":999},"projection":{"totalTokens":999999},"usageLimitResetTime":"private-backend-claim"
  }]
}`

type fakeResponse struct {
	output []byte
	err    error
}

type fakeRunner struct {
	mu          sync.Mutex
	responses   map[string]fakeResponse
	specs       []commandSpec
	configs     []string
	configModes []os.FileMode
}

func (f *fakeRunner) Run(_ context.Context, spec commandSpec) ([]byte, error) {
	op := commandOperation(spec.args)
	var config, configMode string
	var mode os.FileMode
	if op == "daily" || op == "blocks" {
		configPath := spec.args[len(spec.args)-1]
		contents, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("fake read config: %w", err)
		}
		info, err := os.Stat(configPath)
		if err != nil {
			return nil, fmt.Errorf("fake stat config: %w", err)
		}
		config = string(contents)
		mode = info.Mode().Perm()
		configMode = configPath
	}
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	if configMode != "" {
		f.configs = append(f.configs, config)
		f.configModes = append(f.configModes, mode)
	}
	response := f.responses[op]
	f.mu.Unlock()
	return slices.Clone(response.output), response.err
}

func commandOperation(args []string) string {
	if len(args) >= 2 && args[1] == "--version" {
		return "version"
	}
	if len(args) >= 2 && args[1] == "daily" {
		return "daily"
	}
	if len(args) >= 3 && args[1] == "claude" && args[2] == "blocks" {
		return "blocks"
	}
	return "unknown"
}

func TestCollectNormalizesHistoryAndUsesControlledCommands(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-child")
	t.Setenv("OPENAI_API_KEY", "must-not-reach-child")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-reach-child")
	t.Setenv("CODEX_HOME", "/private/redirected-codex-logs")
	t.Setenv("CLAUDE_CONFIG_DIR", "/private/redirected-claude-logs")
	runner := &fakeRunner{responses: map[string]fakeResponse{
		"version": {output: []byte("ccusage version 20.0.20\n")},
		"daily":   {output: []byte(representativeDaily)},
		"blocks":  {output: []byte(representativeBlocks)},
	}}
	times := []time.Time{
		time.Date(2026, 9, 13, 1, 2, 3, 0, time.FixedZone("local", -7*60*60)),
		time.Date(2026, 9, 13, 1, 2, 4, 0, time.FixedZone("local", -7*60*60)),
	}
	var timeIndex int
	collector, err := New(Options{runner: runner, now: func() time.Time {
		value := times[timeIndex]
		timeIndex++
		return value
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Both instants fall on September 12 in UTC even though their local dates
	// differ, proving that the CLI range and report dates use UTC.
	since := time.Date(2026, 9, 11, 22, 0, 0, 0, time.FixedZone("west", -7*60*60))
	until := time.Date(2026, 9, 13, 1, 0, 0, 0, time.FixedZone("east", 3*60*60))
	report, err := collector.Collect(context.Background(), since, until)
	if err != nil {
		t.Fatal(err)
	}

	if report.ToolVersion != "20.0.20" || report.Source != SourceCCUsage || report.Coverage != CoverageLocalOnly ||
		report.CostBasis != CostBasisCalculatedAPIReference || report.UnitPriceUnavailableReason != UnitPriceUnavailableCCUsage {
		t.Fatalf("report provenance = version %q source %q coverage %q", report.ToolVersion, report.Source, report.Coverage)
	}
	wantDate := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	if !report.SinceDate.Equal(wantDate) || !report.UntilDate.Equal(wantDate) {
		t.Fatalf("UTC range = %s..%s, want %s", report.SinceDate, report.UntilDate, wantDate)
	}
	if len(report.Daily) != 4 {
		t.Fatalf("daily rows = %d, want 4: %#v", len(report.Daily), report.Daily)
	}
	if slices.ContainsFunc(report.Daily, func(row DailyModelUsage) bool { return row.Model == "outer-must-not-be-counted" }) {
		t.Fatal("outer unified breakdown was double-counted")
	}
	rows := make(map[string]DailyModelUsage)
	for _, row := range report.Daily {
		rows[row.Model] = row
	}
	if rows["claude-sonnet-4-5"].Provider != ProviderAnthropic || rows["gpt-5.6-sol"].Provider != ProviderCodex ||
		rows["deepseek-chat"].Provider != ProviderDeepSeek || rows["mystery-model"].Provider != ProviderUnknown {
		t.Fatalf("provider classification = %#v", rows)
	}
	if row := rows["gpt-5.6-sol"]; row.TotalTokens != 11 || row.CostUSD != nil || row.CostStatus != CostUnavailableOrUnpriced {
		t.Fatalf("zero-priced positive usage = %#v", row)
	}
	if row := rows["claude-sonnet-4-5"]; row.CostUSD == nil || *row.CostUSD != 1.25 || row.CostStatus != CostAvailable {
		t.Fatalf("priced usage = %#v", row)
	}
	if len(report.Blocks) != 1 {
		t.Fatalf("blocks = %#v", report.Blocks)
	}
	block := report.Blocks[0]
	if !block.MixedProvider || len(block.Models) != 2 || block.CostUSD != nil || block.CostStatus != CostUnavailableOrUnpriced {
		t.Fatalf("mixed block = %#v", block)
	}
	if block.StartTime.Location() != time.UTC || block.ActualEndTime != nil {
		t.Fatalf("block times = %#v", block)
	}

	runner.mu.Lock()
	specs := slices.Clone(runner.specs)
	configs := slices.Clone(runner.configs)
	modes := slices.Clone(runner.configModes)
	runner.mu.Unlock()
	if len(specs) != 3 || len(configs) != 2 {
		t.Fatalf("commands/configs = %d/%d", len(specs), len(configs))
	}
	byOp := make(map[string]commandSpec)
	for _, spec := range specs {
		byOp[commandOperation(spec.args)] = spec
		if spec.name != DefaultExecutable || spec.dir == "" || spec.maxOutput != DefaultMaxOutputBytes {
			t.Fatalf("command envelope = %#v", spec)
		}
		for _, env := range spec.env {
			if strings.Contains(env, "must-not-reach-child") {
				t.Fatalf("credential inherited by child: %q", env)
			}
		}
		if !slices.Contains(spec.env, "CODEX_HOME=/private/redirected-codex-logs") ||
			!slices.Contains(spec.env, "CLAUDE_CONFIG_DIR=/private/redirected-claude-logs") {
			t.Fatalf("redirected local log paths missing from environment: %#v", spec.env)
		}
		if _, statErr := os.Stat(spec.dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("private working directory was not removed: %v", statErr)
		}
	}
	if !reflect.DeepEqual(byOp["version"].args, []string{"ccusage@latest", "--version"}) {
		t.Fatalf("version argv = %#v", byOp["version"].args)
	}
	wantDaily := []string{"ccusage@20.0.20", "daily", "--json", "--by-agent", "--timezone", "UTC", "--since", "2026-09-12", "--until", "2026-09-12", "--no-offline", "--config", byOp["daily"].args[len(byOp["daily"].args)-1]}
	if !reflect.DeepEqual(byOp["daily"].args, wantDaily) {
		t.Fatalf("daily argv = %#v", byOp["daily"].args)
	}
	wantBlocks := []string{"ccusage@20.0.20", "claude", "blocks", "--json", "--mode", "calculate", "--timezone", "UTC", "--session-length", "5", "--since", "20260912", "--until", "20260912", "--config", byOp["blocks"].args[len(byOp["blocks"].args)-1]}
	if !reflect.DeepEqual(byOp["blocks"].args, wantBlocks) {
		t.Fatalf("blocks argv = %#v", byOp["blocks"].args)
	}
	for index, config := range configs {
		if config != `{"defaults":{"mode":"calculate","pricingOverrides":{}}}` || modes[index] != 0o600 {
			t.Fatalf("controlled config = %q mode %o", config, modes[index])
		}
	}
	if len(report.Issues) != 0 || !report.StartedAt.Equal(times[0].UTC()) || !report.FinishedAt.Equal(times[1].UTC()) {
		t.Fatalf("report envelope = %#v", report)
	}
}

func TestCollectSupportsEmptyReports(t *testing.T) {
	runner := &fakeRunner{responses: map[string]fakeResponse{
		"version": {output: []byte("20.0.20")},
		"daily":   {output: []byte(`{"daily":[],"totals":{"inputTokens":0,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":0,"totalCost":0}}`)},
		"blocks":  {output: []byte(`{"blocks":[]}`)},
	}}
	collector, err := New(Options{runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.Collect(context.Background(), time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Daily == nil || report.Blocks == nil || len(report.Daily) != 0 || len(report.Blocks) != 0 {
		t.Fatalf("empty report lost empty slices: %#v", report)
	}
}

func TestCollectReturnsUsablePartialReportAndSanitizedIssue(t *testing.T) {
	secret := "SECRET_FROM_STDERR"
	runner := &fakeRunner{responses: map[string]fakeResponse{
		"version": {output: []byte("20.0.20")},
		"daily":   {err: &processError{kind: processErrorExit, safeDetail: "tool exited with status 7", cause: errors.New(secret)}},
		"blocks":  {output: []byte(`{"blocks":[]}`)},
	}}
	collector, err := New(Options{runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.Collect(context.Background(), time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected partial collection error")
	}
	if strings.Contains(err.Error(), secret) || len(report.Issues) != 1 || strings.Contains(report.Issues[0].Message, secret) {
		t.Fatalf("secret exposed: err=%q issues=%#v", err, report.Issues)
	}
	if report.Issues[0].Section != "daily" || report.Issues[0].Code != string(ErrorCommand) || !report.Issues[0].Missing || report.Blocks == nil {
		t.Fatalf("partial metadata = %#v", report)
	}
}

func TestCollectMarksIncompatibleSectionWithoutDiscardingOtherSection(t *testing.T) {
	runner := &fakeRunner{responses: map[string]fakeResponse{
		"version": {output: []byte("20.0.20")},
		"daily":   {output: []byte(`{"daily":[]}`)},
		"blocks":  {output: []byte(`{"blocks":[]}`)},
	}}
	collector, err := New(Options{runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.Collect(context.Background(), time.Now(), time.Now())
	var collectionErr *CollectError
	if !errors.As(err, &collectionErr) || collectionErr.Section != "daily" || collectionErr.Kind != ErrorIncompatible {
		t.Fatalf("schema error = %#v", err)
	}
	if len(report.Blocks) != 0 || report.Blocks == nil || len(report.Issues) != 1 || report.Issues[0].Code != string(ErrorIncompatible) {
		t.Fatalf("partial incompatible report = %#v", report)
	}
}

func TestDailySchemaAndNumericValidation(t *testing.T) {
	date := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	tests := map[string]string{
		"absent cost is not zero": strings.Replace(representativeDaily, `,"cost":0}`, `}`, 1),
		"negative unsigned token": strings.Replace(representativeDaily, `"inputTokens":10`, `"inputTokens":-1`, 1),
		"inconsistent total":      strings.Replace(representativeDaily, `"totalTokens":49`, `"totalTokens":50`, 1),
		"trailing JSON":           representativeDaily + ` {}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDaily([]byte(input), date, date); err == nil {
				t.Fatal("expected incompatible data error")
			}
		})
	}
}

func TestNewRejectsUnsafeOrUnboundedOptions(t *testing.T) {
	tests := []Options{
		{Package: "--package"},
		{Version: "latest --silent"},
		{Executable: " bunx"},
		{Timeout: -time.Second},
		{MaxOutputBytes: -1},
	}
	for _, opts := range tests {
		if _, err := New(opts); err == nil {
			t.Fatalf("New(%#v) succeeded", opts)
		}
	}
}

func TestProcessRunnerCancelsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err := (processRunner{}).Run(ctx, commandSpec{
		name: os.Args[0],
		args: []string{"-test.run=TestUsageHistoryHelperProcess", "--", "sleep-with-child", marker},
		dir:  t.TempDir(), env: []string{"PATH=" + os.Getenv("PATH")}, maxOutput: 1 << 20,
	})
	var pe *processError
	if !errors.As(err, &pe) || pe.kind != processErrorCanceled || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %#v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant survived process-group cancellation: %v", statErr)
	}
}

func TestProcessRunnerEnforcesCombinedOutputLimitWithoutLeakingStderr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := (processRunner{}).Run(ctx, commandSpec{
		name: os.Args[0], args: []string{"-test.run=TestUsageHistoryHelperProcess", "--", "output"},
		dir: t.TempDir(), env: []string{"PATH=" + os.Getenv("PATH")}, maxOutput: 1024,
	})
	var pe *processError
	if !errors.As(err, &pe) || pe.kind != processErrorOutputLimit {
		t.Fatalf("output-limit error = %#v", err)
	}
	if strings.Contains(err.Error(), "SECRET_STDERR") {
		t.Fatalf("stderr leaked in error: %q", err)
	}
	_, err = (processRunner{}).Run(context.Background(), commandSpec{
		name: os.Args[0], args: []string{"-test.run=TestUsageHistoryHelperProcess", "--", "exit"},
		dir: t.TempDir(), env: []string{"PATH=" + os.Getenv("PATH")}, maxOutput: 1024,
	})
	if !errors.As(err, &pe) || pe.kind != processErrorExit || strings.Contains(err.Error(), "SECRET_STDERR") {
		t.Fatalf("exit error exposed stderr: %#v", err)
	}
}

func TestUsageHistoryHelperProcess(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	switch os.Args[separator+1] {
	case "sleep-with-child":
		marker := os.Args[separator+2]
		child := exec.Command(os.Args[0], "-test.run=TestUsageHistoryHelperProcess", "--", "mark", marker)
		if err := child.Start(); err != nil {
			os.Exit(90)
		}
		time.Sleep(30 * time.Second)
	case "mark":
		time.Sleep(400 * time.Millisecond)
		if err := os.WriteFile(os.Args[separator+2], []byte("alive"), 0o600); err != nil {
			os.Exit(91)
		}
	case "output":
		_, _ = fmt.Fprint(os.Stderr, "SECRET_STDERR")
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 1<<20))
	case "exit":
		_, _ = fmt.Fprint(os.Stderr, "SECRET_STDERR")
		os.Exit(7)
	default:
		os.Exit(92)
	}
	os.Exit(0)
}
