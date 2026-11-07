// Package textcut cuts text to a byte budget without splitting a UTF-8 rune.
package textcut

import "unicode/utf8"

// Truncate cuts s to at most maxBytes, backing off to a rune boundary.
func Truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 { // no rune boundary in reach (invalid UTF-8): cut hard
		cut = maxBytes
	}
	return s[:cut]
}
