// Package usagehistory collects normalized local usage history from ccusage.
// It does not parse provider logs itself, price tokens, or cache reports.
package usagehistory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hughescr/utraque/internal/leg"
)

const (
	// DefaultExecutable is the package runner used by a zero Options value.
	DefaultExecutable = "bunx"
	// DefaultPackage is the upstream usage-report package.
	DefaultPackage = "ccusage"
	// DefaultVersion follows the most recent ccusage release, as requested by
	// the operator. Pin Version when reproducibility is more important.
	DefaultVersion = "latest"
	// DefaultTimeout bounds one complete collection, including package startup.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxOutputBytes bounds each subprocess's stdout and stderr together.
	DefaultMaxOutputBytes int64 = 32 << 20

	SourceCCUsage     = "ccusage"
	CoverageLocalOnly = "local_only"
	// CostBasisCalculatedAPIReference distinguishes ccusage's calculated USD
	// references from actual subscription charges or prepaid debits.
	CostBasisCalculatedAPIReference = "calculated_api_reference_usd"
	// UnitPriceUnavailableCCUsage states why this collector cannot provide a
	// live per-model unit-price catalog.
	UnitPriceUnavailableCCUsage = "ccusage_does_not_report_unit_prices"
)

// CostStatus describes whether CostUSD can be used in sums. ccusage does not
// expose its missing-pricing flag, so a zero cost with positive token usage is
// conservatively treated as unavailable or unpriced.
type CostStatus string

const (
	CostAvailable             CostStatus = "available"
	CostUnavailableOrUnpriced CostStatus = "unavailable_or_unpriced"
)

// InvocationMode records whether ccusage ran through bunx or as an explicitly
// configured native executable.
type InvocationMode string

const (
	InvocationBunx   InvocationMode = "bunx"
	InvocationNative InvocationMode = "native"
)

// Options configures a Collector. The zero value runs bunx ccusage@latest.
type Options struct {
	// NativeExecutable directly invokes an already installed ccusage native
	// binary. When set it takes precedence over Executable, Package, and
	// Version. The collector never installs, updates, or falls back from it.
	NativeExecutable string
	// Executable is the package runner for bunx mode. Empty means bunx.
	Executable string
	// Package and Version form the package specifier in bunx mode. Empty means
	// ccusage@latest.
	Package        string
	Version        string
	Timeout        time.Duration
	MaxOutputBytes int64

	runner commandRunner
	now    func() time.Time
}

// Collector invokes ccusage with a private controlled configuration.
type Collector struct {
	executable       string
	packageName      string
	requestedVersion string
	packageArg       string
	timeout          time.Duration
	maxOutput        int64
	invocationMode   InvocationMode
	runner           commandRunner
	now              func() time.Time
}

// Report is one collection attempt. SinceDate and UntilDate are inclusive UTC
// dates represented as midnight. A non-nil Collect error can accompany a
// partial report; Issues identifies every omitted section.
type Report struct {
	StartedAt                  time.Time         `json:"started_at"`
	FinishedAt                 time.Time         `json:"finished_at"`
	SinceDate                  time.Time         `json:"since_date"`
	UntilDate                  time.Time         `json:"until_date"`
	ToolVersion                string            `json:"tool_version"`
	InvocationMode             InvocationMode    `json:"invocation_mode"`
	Source                     string            `json:"source"`
	Coverage                   string            `json:"coverage"`
	CostBasis                  string            `json:"cost_basis"`
	UnitPriceUnavailableReason string            `json:"unit_price_unavailable_reason"`
	Daily                      []DailyModelUsage `json:"daily"`
	Blocks                     []BlockSummary    `json:"blocks"`
	Issues                     []Issue           `json:"issues"`
}

// DailyModelUsage is one model, UTC date, and ccusage local-log source. Source
// is an agent label such as "claude", "codex", or "opencode"; it does not
// identify a provider billing account. InferredLeg is classifyLeg's guess
// from the model name alone (leg.Unknown when no prefix matches); it is
// serialised under the key "provider" in both report schemas. Token
// categories are counted exactly once; output tokens already include any
// reasoning-token subset reported by a provider.
type DailyModelUsage struct {
	Date                time.Time  `json:"date"`
	Source              string     `json:"source"`
	Model               string     `json:"model"`
	InferredLeg         leg.ID     `json:"provider"`
	InputTokens         uint64     `json:"input_tokens"`
	OutputTokens        uint64     `json:"output_tokens"`
	CacheCreationTokens uint64     `json:"cache_creation_tokens"`
	CacheReadTokens     uint64     `json:"cache_read_tokens"`
	TotalTokens         uint64     `json:"total_tokens"`
	CostUSD             *float64   `json:"cost_usd"`
	CostStatus          CostStatus `json:"cost_status"`
}

// BlockModel is a raw model name plus the leg classifyLeg infers from it,
// serialised under the key "provider" in both report schemas.
type BlockModel struct {
	Model       string `json:"model"`
	InferredLeg leg.ID `json:"provider"`
}

// BlockSummary is a Claude-log session block. ccusage does not provide
// per-model totals for blocks, so Models is descriptive and all usage remains
// aggregate.
type BlockSummary struct {
	ID                  string       `json:"id"`
	Source              string       `json:"source"`
	StartTime           time.Time    `json:"start_time"`
	EndTime             time.Time    `json:"end_time"`
	ActualEndTime       *time.Time   `json:"actual_end_time"`
	IsActive            bool         `json:"is_active"`
	IsGap               bool         `json:"is_gap"`
	Entries             uint64       `json:"entries"`
	InputTokens         uint64       `json:"input_tokens"`
	OutputTokens        uint64       `json:"output_tokens"`
	CacheCreationTokens uint64       `json:"cache_creation_tokens"`
	CacheReadTokens     uint64       `json:"cache_read_tokens"`
	TotalTokens         uint64       `json:"total_tokens"`
	CostUSD             *float64     `json:"cost_usd"`
	CostStatus          CostStatus   `json:"cost_status"`
	Models              []BlockModel `json:"models"`
	MixedProvider       bool         `json:"mixed_provider"`
}

// Issue records why one report section is missing.
type Issue struct {
	Section string `json:"section"`
	Code    string `json:"code"`
	Missing bool   `json:"missing"`
	Message string `json:"message"`
}

// ErrorKind classifies collection failures without exposing subprocess output.
type ErrorKind string

const (
	ErrorInvalidRequest ErrorKind = "invalid_request"
	ErrorCommand        ErrorKind = "command_failed"
	ErrorTimeout        ErrorKind = "timeout"
	ErrorCanceled       ErrorKind = "canceled"
	ErrorOutputLimit    ErrorKind = "output_limit"
	ErrorIncompatible   ErrorKind = "incompatible_tool_output"
)

// CollectError is returned for a missing report section. Error never includes
// stderr or raw stdout, either of which can contain private local-log data.
type CollectError struct {
	Section string
	Kind    ErrorKind
	detail  string
	cause   error
}

func (e *CollectError) Error() string {
	if e.detail != "" {
		return fmt.Sprintf("usagehistory: %s: %s: %s", e.Section, e.Kind, e.detail)
	}
	return fmt.Sprintf("usagehistory: %s: %s", e.Section, e.Kind)
}

func (e *CollectError) Unwrap() error { return e.cause }

var (
	packagePattern = regexp.MustCompile(`^(?:@[A-Za-z0-9._-]+/)?[A-Za-z0-9._-]+$`)
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

// New validates opts and constructs a collector without starting a process.
func New(opts Options) (*Collector, error) {
	executable := opts.NativeExecutable
	invocationMode := InvocationNative
	pkg, version, packageArg := "", "", ""
	if executable == "" {
		invocationMode = InvocationBunx
		executable = opts.Executable
		if executable == "" {
			executable = DefaultExecutable
		}
		pkg = opts.Package
		if pkg == "" {
			pkg = DefaultPackage
		}
		version = opts.Version
		if version == "" {
			version = DefaultVersion
		}
		if !packagePattern.MatchString(pkg) || strings.HasPrefix(pkg, "-") || strings.HasPrefix(pkg, ".") {
			return nil, fmt.Errorf("usagehistory: invalid package name %q", pkg)
		}
		if !versionPattern.MatchString(version) {
			return nil, fmt.Errorf("usagehistory: invalid package version %q", version)
		}
		packageArg = pkg + "@" + version
	}
	if strings.TrimSpace(executable) != executable || strings.ContainsRune(executable, 0) {
		return nil, fmt.Errorf("usagehistory: executable must not have surrounding whitespace or NUL")
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("usagehistory: timeout must be positive")
	}
	maxOutput := opts.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = DefaultMaxOutputBytes
	}
	if maxOutput < 0 {
		return nil, fmt.Errorf("usagehistory: max output bytes must be positive")
	}
	runner := opts.runner
	if runner == nil {
		runner = processRunner{}
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	return &Collector{
		executable:       executable,
		packageName:      pkg,
		requestedVersion: version,
		packageArg:       packageArg,
		timeout:          timeout,
		maxOutput:        maxOutput,
		invocationMode:   invocationMode,
		runner:           runner,
		now:              now,
	}, nil
}

// classifyLeg is this package's attribution heuristic: it guesses which leg
// a model name belongs to from its prefix alone, and answers leg.Unknown for
// anything else. It is deliberately private and deliberately not the router's
// model grammar (internal/router accepts broader Anthropic prefixes and gives
// registered picker ids and "anthropic-compat." names their own precedence):
// a GPT name in a Claude log establishes an attribution, not a billing
// account, and the two rules are not required to agree.
func classifyLeg(model string) leg.ID {
	name := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(name, "claude-"):
		return leg.Anthropic
	case strings.HasPrefix(name, "gpt-"):
		return leg.Codex
	case strings.HasPrefix(name, "deepseek-"):
		return leg.DeepSeek
	default:
		return leg.Unknown
	}
}

func utcDate(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func classifyError(section string, err error) *CollectError {
	var pe *processError
	if errors.As(err, &pe) {
		kind := ErrorCommand
		switch pe.kind {
		case processErrorOutputLimit:
			kind = ErrorOutputLimit
		case processErrorCanceled:
			if errors.Is(pe.cause, context.DeadlineExceeded) {
				kind = ErrorTimeout
			} else {
				kind = ErrorCanceled
			}
		}
		return &CollectError{Section: section, Kind: kind, detail: pe.safeDetail, cause: pe.cause}
	}
	return &CollectError{Section: section, Kind: ErrorCommand, cause: err}
}
