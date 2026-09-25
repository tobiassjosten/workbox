package compute

import (
	"strings"
	"unicode"
)

// The bounds this package puts on untrusted text — server-supplied, or taken from
// the config by WithDefaults — before it reaches a terminal:
//
//   - maxRenderedValue bounds a value rendered inside a single-line message: an
//     untrusted guest attribute, and a capacity error's zone, machine type and
//     each alternative zone.
//   - maxMessageValue bounds the server's own explanation, which the remedy block
//     prints on a line of its own, so it may run longer than the rest.
//   - maxListEntries bounds how many alternative zones are kept: they are joined
//     onto one line, so the count needs a bound as much as each entry does.
const (
	maxRenderedValue = 200
	maxMessageValue  = 500
	maxListEntries   = 10
)

// truncate bounds a value at maxRenderedValue runes plus an ellipsis marker; it
// is what lastActiveFromResp applies to the guest attribute.
func truncate(s string) string { return bound(s, maxRenderedValue) }

// bound cuts s to limit runes, appending an ellipsis to mark that it did.
func bound(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

// sanitize prepares an untrusted value — server- or config-supplied — for a
// single-line message, bounded at maxRenderedValue runes plus an ellipsis where
// it was cut.
func sanitize(s string) string {
	return bound(normalize(s), maxRenderedValue)
}

// sanitizeMessage is sanitize for the server's own explanation, which the remedy
// block prints on a line of its own and so may be longer.
func sanitizeMessage(s string) string {
	return bound(normalize(s), maxMessageValue)
}

// normalize keeps graphic runes, turns whitespace — newlines and tabs included —
// into a single space so that nothing runs together and nothing breaks the
// one-line Error(), and drops everything else: control characters, the ESC that
// introduces an escape sequence among them (so what follows stays inert text
// instead of repainting the screen), and format runes such as bidi overrides.
// Leading and trailing whitespace goes entirely.
func normalize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsGraphic(r):
			return r
		case unicode.IsSpace(r):
			return ' '
		default:
			return -1
		}
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
