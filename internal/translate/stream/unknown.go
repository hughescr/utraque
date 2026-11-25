package stream

const (
	maxUnknownBuckets = 64 // includes both reserved buckets
	malformedBucket   = "(malformed)"
	otherBucket       = "(other)"
)

// Unknown event telemetry makes a bounded protocol-identifier exception for
// names matching [a-z][a-z0-9_.]{0,63}. This is not a secrecy guarantee.
func validEventName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// unknownBucket classifies a wire name, reserving both synthetic slots even
// before they first appear. Already-admitted names keep their attribution.
func unknownBucket(counts map[string]int, name string) string {
	if !validEventName(name) {
		return malformedBucket
	}
	if _, ok := counts[name]; ok {
		return name
	}
	used := len(counts)
	for _, reserved := range [...]string{malformedBucket, otherBucket} {
		if _, present := counts[reserved]; !present {
			used++
		}
	}
	if used >= maxUnknownBuckets {
		return otherBucket
	}
	return name
}

// MergeUnknownEvents adds a stream's counts to a lifetime map. Carry forward
// the source's synthetic buckets, but reclassify all names against dst's budget.
func MergeUnknownEvents(dst, src map[string]int) {
	for name, n := range src {
		if name != malformedBucket && name != otherBucket {
			name = unknownBucket(dst, name)
		}
		dst[name] += n
	}
}
