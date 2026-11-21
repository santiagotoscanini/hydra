// Package glob matches slash-separated paths against patterns that support
// `*` (within one segment), `**` (any number of segments), `?` and
// `{a,b}` alternatives (not nested).
package glob

import (
	"regexp"
	"strings"
	"sync"
)

var cache sync.Map // pattern -> *regexp.Regexp

// Match reports whether path matches pattern. A pattern ending in `/` is
// treated as `dir/**`.
func Match(pattern, path string) bool {
	re, ok := cache.Load(pattern)
	if !ok {
		re = compile(pattern)
		cache.Store(pattern, re)
	}
	return re.(*regexp.Regexp).MatchString(path)
}

func compile(pattern string) *regexp.Regexp {
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	var b strings.Builder
	b.WriteString("^")
	inBrace := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '{' && !inBrace:
			inBrace = true
			b.WriteString("(?:")
		case c == '}' && inBrace:
			inBrace = false
			b.WriteString(")")
		case c == ',' && inBrace:
			b.WriteString("|")
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			i++
			// `**/` also matches zero directories.
			if i+1 < len(pattern) && pattern[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
