// Package publicdetail handles credential replacement in decoded upstream error messages.
package publicdetail

import (
	"cmp"
	"slices"
	"strings"
)

// minTrailingFragment is longer than the exact-match threshold: short
// prefixes also occur at the ends of perfectly ordinary messages.
const minTrailingFragment = 16

// Replace masks exact attempt credentials and a substantial credential prefix
// cut off at the end of the input. Scrub before applying any output limit.
func Replace(message, marker string, values ...string) string {
	cut := len(message)
	for _, v := range values {
		cut = min(cut, len(message)-trailingFragment(message, v))
	}
	if cut < len(message) {
		// Preserve leftmost-longest exact matches, including one that straddles
		// the cut. Never re-scan the marker as if it were upstream text.
		var out strings.Builder
		next := make([]int, len(values))
		for j := range next {
			next[j] = -1
		}
		for pos := 0; pos < cut; {
			at, size := cut, 0
			for j, v := range values {
				if len(v) < 4 {
					continue
				}
				if next[j] < pos {
					next[j] = len(message)
					if i := strings.Index(message[pos:], v); i >= 0 {
						next[j] = pos + i
					}
				}
				if i := next[j]; i < at || i == at && len(v) > size {
					at, size = i, len(v)
				}
			}
			out.WriteString(message[pos:at])
			if at == cut {
				break
			}
			out.WriteString(marker)
			pos = at + size
			if pos > cut {
				return out.String()
			}
		}
		out.WriteString(marker)
		return out.String()
	}
	return replaceExact(message, marker, values)
}

// trailingFragment returns the longest proper prefix of secret at EOF. The
// usual case is a single bounded substring search. For repetitive inputs the
// prefix table avoids repeatedly comparing long almost-matching candidates.
func trailingFragment(text, secret string) int {
	n := min(len(text), len(secret)-1)
	if n < minTrailingFragment {
		return 0
	}
	tail := text[len(text)-n:]
	i := strings.Index(tail, secret[:minTrailingFragment])
	if i < 0 {
		return 0
	}
	tail = tail[i:]
	if strings.HasPrefix(secret, tail) {
		return len(tail)
	}
	if !strings.Contains(tail[1:], secret[:minTrailingFragment]) {
		return 0
	}
	border := make([]int, len(tail))
	advance := func(n int, ch byte) int {
		for n > 0 && secret[n] != ch {
			n = border[n-1]
		}
		if secret[n] == ch {
			n++
		}
		return n
	}
	for j := 1; j < len(border); j++ {
		border[j] = advance(border[j-1], secret[j])
	}
	matched := 0
	for j := range len(tail) {
		matched = advance(matched, tail[j])
	}
	if matched < minTrailingFragment {
		return 0
	}
	return matched
}

func replaceExact(message, marker string, values []string) string {
	if len(values) == 1 {
		if len(values[0]) < 4 {
			return message
		}
		return strings.ReplaceAll(message, values[0], marker)
	}
	// Prefer the whole token when another credential is its prefix; replace in
	// one pass so the marker itself is never input to a second replacement.
	ordered := slices.Clone(values)
	slices.SortFunc(ordered, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	pairs := make([]string, 0, 2*len(ordered))
	for _, v := range ordered {
		if len(v) >= 4 {
			pairs = append(pairs, v, marker)
		}
	}
	return strings.NewReplacer(pairs...).Replace(message)
}
