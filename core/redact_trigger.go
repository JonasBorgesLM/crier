package core

// triggerScanner answers "could any rule possibly match this text?" much
// faster than asking the rules themselves.
//
// It exists because the obvious implementation — one case-insensitive regex
// alternation of the trigger literals — measured slower than the five body
// patterns it was meant to skip. Wrapping a literal in (?i) expands it into
// character classes, which defeats the literal-prefix optimisation that makes
// RE2 fast on plain strings, so the "cheap" pre-check cost more than the
// expensive work.
//
// This scans the text once, and at each byte consults a 256-entry table keyed
// by the lowercased byte. Only the handful of letters that actually begin a
// trigger lead to any comparison at all.
type triggerScanner struct {
	// byFirstByte[c] holds the triggers starting with c, which is always a
	// lowercase ASCII byte or punctuation.
	byFirstByte [256][]string
	maxLen      int
}

func newTriggerScanner(triggers []string) *triggerScanner {
	s := &triggerScanner{}
	for _, t := range triggers {
		if t == "" {
			continue
		}
		lowered := lowerASCII(t)
		c := lowered[0]
		s.byFirstByte[c] = append(s.byFirstByte[c], lowered)
		if len(lowered) > s.maxLen {
			s.maxLen = len(lowered)
		}
	}
	return s
}

// matches reports whether text contains any trigger, compared without regard
// to ASCII case.
func (s *triggerScanner) matches(text string) bool {
	for i := range len(text) {
		candidates := s.byFirstByte[lowerByte(text[i])]
		if len(candidates) == 0 {
			continue
		}
		rest := text[i:]
		for _, t := range candidates {
			if hasPrefixFold(rest, t) {
				return true
			}
		}
	}
	return false
}

// hasPrefixFold reports whether s starts with the lowercase ASCII prefix,
// ignoring case. prefix must already be lowercase.
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := range len(prefix) {
		if lowerByte(s[i]) != prefix[i] {
			return false
		}
	}
	return true
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// lowerASCII lowercases the ASCII letters in s. Non-ASCII bytes are left
// alone: triggers are literals chosen by this package and by operators, and
// full Unicode case folding would cost more than the scan it feeds.
func lowerASCII(s string) string {
	out := []byte(s)
	for i := range out {
		out[i] = lowerByte(out[i])
	}
	return string(out)
}
