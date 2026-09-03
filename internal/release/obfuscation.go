package release

import (
	"regexp"
	"strings"
)

// reHexLike matches a token that is entirely hex digits (a common obfuscation).
var reHexLike = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)

// IsObfuscated reports whether a release name looks like a random/obfuscated
// identifier (hex or base64-ish with no meaningful words), rather than a
// human-readable scene/release name. It is used both to decide which releases
// are worth probing for a PAR2-recovered real name, and to exclude unusable
// obfuscated releases from default search results.
func IsObfuscated(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	// A pure long hex string is almost certainly obfuscated.
	if reHexLike.MatchString(name) {
		return true
	}

	// Split on common separators and inspect the tokens.
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == ' ' || r == '.' || r == '-' || r == '_'
	})
	if len(fields) == 0 {
		return true
	}

	// A name is considered obfuscated when it has no word-like tokens and at
	// least one long random-looking token.
	hasWord := false
	hasRandom := false
	for _, f := range fields {
		if isWordLike(f) {
			hasWord = true
			continue
		}
		if len(f) >= 8 {
			hasRandom = true
		}
	}
	if hasWord {
		return false
	}
	return hasRandom
}

// isWordLike reports whether a token resembles a real word rather than a
// random/base64 identifier. A real word is a run of >=3 letters that is all
// lowercase, all uppercase, or Title-case (leading capital then lowercase),
// contains no digits, and includes at least one vowel. Mixed-case runs like
// "PERuop" or letter/digit soup like "4Mg2PER" are rejected.
func isWordLike(tok string) bool {
	if len(tok) < 3 {
		return false
	}
	hasDigit := false
	letters := 0
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		}
	}
	if hasDigit || letters < 3 || letters != len([]rune(tok)) {
		return false // real words in release names don't mix digits into a token
	}
	if !isWordCase(tok) {
		return false // reject mixed-case (base64-like) runs
	}
	return hasVowel(tok)
}

// isWordCase reports whether s is all-lower, all-upper, or Title-case.
func isWordCase(s string) bool {
	rs := []rune(s)
	allLower, allUpper := true, true
	for _, r := range rs {
		if r >= 'A' && r <= 'Z' {
			allLower = false
		}
		if r >= 'a' && r <= 'z' {
			allUpper = false
		}
	}
	if allLower || allUpper {
		return true
	}
	// Title-case: first upper, remainder lower.
	if rs[0] >= 'A' && rs[0] <= 'Z' {
		for _, r := range rs[1:] {
			if r >= 'A' && r <= 'Z' {
				return false
			}
		}
		return true
	}
	return false
}

func hasVowel(s string) bool {
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'a', 'e', 'i', 'o', 'u', 'y':
			return true
		}
	}
	return false
}
