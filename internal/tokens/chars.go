package tokens

import (
	"strconv"

	"github.com/hughescr/utraque/internal/codex/schema"
)

// Defaults for the fallback heuristic. All are overridable per instance.
const (
	// DefaultDivisor is the UTF-8 bytes-per-token ratio. Four is the usual
	// rule of thumb for English text under o200k. Counting BYTES rather than
	// characters is deliberate: for Latin text the two are identical, and for
	// CJK — where o200k spends roughly one token per character, and each
	// character costs three UTF-8 bytes — bytes/4 lands far closer than
	// characters/4 would.
	DefaultDivisor = 4

	// DefaultPerItemOverhead is the fixed cost of the role and turn framing
	// wrapped around each input item.
	DefaultPerItemOverhead = 4

	// DefaultPerToolOverhead is the fixed framing cost of one tool definition.
	DefaultPerToolOverhead = 8

	// DefaultImageTokens is the flat cost charged for one image part. Real
	// image cost is a function of tile geometry, which cannot be computed
	// without decoding the payload; charging base64 length through the text
	// divisor would overestimate by roughly an order of magnitude, so a flat
	// plausible figure is the more honest wrong answer.
	DefaultImageTokens = 1024
)

// CharsPerToken is the heuristic: a bytes-per-token ratio with flat overheads
// for item framing, tool definitions and images. It implements both views. As
// an Estimator it is what Default returns, for a leg with no exact tokenizer.
// As a RequestEstimator it is the FALLBACK Codex returns only when the exact
// tokenizer cannot initialise, kept because a request that fails for want of a
// token count is worse than a rough count. It is NOT a lower bound — the
// overheads and the divisor both round up — so while it is in force on the
// Codex leg the message_start seed can win ccusage's dedup (see the package
// comment); its Name on the log line is how to tell. The zero value is usable
// and applies every Default* constant.
type CharsPerToken struct {
	// Divisor is UTF-8 bytes per token; <= 0 uses DefaultDivisor.
	Divisor int
	// PerItemOverhead is tokens of framing per input item; < 0 disables it,
	// 0 uses DefaultPerItemOverhead.
	PerItemOverhead int
	// PerToolOverhead is tokens of framing per tool definition; < 0 disables
	// it, 0 uses DefaultPerToolOverhead.
	PerToolOverhead int
	// ImageTokens is the flat cost of one image part; < 0 disables it, 0 uses
	// DefaultImageTokens.
	ImageTokens int
}

var (
	_ Estimator        = CharsPerToken{}
	_ RequestEstimator = CharsPerToken{}
)

func (e CharsPerToken) divisor() int {
	if e.Divisor <= 0 {
		return DefaultDivisor
	}
	return e.Divisor
}

func settingOr(v, dflt int) int {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return dflt
	default:
		return v
	}
}

// Name reports the heuristic and its divisor, e.g. "chars/4".
func (e CharsPerToken) Name() string { return "chars/" + strconv.Itoa(e.divisor()) }

// EstimateString estimates the tokens in one string, rounding up so a non-empty
// string never estimates to zero tokens.
func (e CharsPerToken) EstimateString(s string) int {
	return ceilDiv(len(s), e.divisor())
}

// EstimateRequest estimates a translated request. Byte costs across every field are
// summed first and divided ONCE, so the estimate does not inflate with the
// number of fields the same text is split across; discrete framing costs are
// added as whole tokens on top. A tool's schema is charged as its raw JSON
// bytes: this heuristic makes no claim to be a lower bound.
func (e CharsPerToken) EstimateRequest(req *cschema.ResponsesRequest) int {
	v := charsVisitor{
		perItem:     settingOr(e.PerItemOverhead, DefaultPerItemOverhead),
		perTool:     settingOr(e.PerToolOverhead, DefaultPerToolOverhead),
		imageTokens: settingOr(e.ImageTokens, DefaultImageTokens),
	}
	walkFields(req, &v)
	return v.tokens + ceilDiv(v.bytes, e.divisor())
}

// charsVisitor accumulates the two cost kinds separately: bytes are divided
// once at the end, tokens are already whole.
type charsVisitor struct {
	bytes, tokens                 int
	perItem, perTool, imageTokens int
}

func (v *charsVisitor) Text(s string) { v.bytes += len(s) }
func (v *charsVisitor) Image()        { v.tokens += v.imageTokens }
func (v *charsVisitor) Item()         { v.tokens += v.perItem }
func (v *charsVisitor) Tool(t *cschema.Tool) {
	v.tokens += v.perTool
	v.bytes += len(t.Name) + len(t.Description) + len(t.Parameters)
}

// ceilDiv divides rounding up, for non-negative n and positive d.
func ceilDiv(n, d int) int {
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}
