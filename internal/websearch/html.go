package websearch

import (
	"regexp"
	"strconv"
	"strings"
)

// A regex HTML stripper rather than a parser dependency. It is doing one job —
// turning a page into something a model can read — and for that, over-stripping
// is a better failure than a third direct dependency. Anything it mangles, the
// model sees as mangled text rather than as confident nonsense.
var (
	// Alternation rather than a backreference: Go's regexp is RE2, which has
	// none. Each element is listed with its own closing tag.
	dropElements = regexp.MustCompile(`(?is)<script\b.*?</script\s*>|<style\b.*?</style\s*>` +
		`|<noscript\b.*?</noscript\s*>|<svg\b.*?</svg\s*>|<head\b.*?</head\s*>`)
	blockEnd   = regexp.MustCompile(`(?i)</(p|div|section|article|li|tr|h[1-6]|blockquote|pre)\s*>`)
	lineBreak  = regexp.MustCompile(`(?i)<(br|hr)\s*/?>`)
	anyTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	titleTag   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	manyBlank  = regexp.MustCompile(`\n{3,}`)
	trailingWS = regexp.MustCompile(`[ \t]+\n`)
	manySpaces = regexp.MustCompile(`[ \t]{2,}`)
	numericEnt = regexp.MustCompile(`&#(x?[0-9a-fA-F]+);`)
)

func htmlTitle(s string) string {
	if m := titleTag.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(decodeEntities(stripTags(m[1])))
	}
	return ""
}

func htmlToText(s string) string {
	s = dropElements.ReplaceAllString(s, "\n")
	s = blockEnd.ReplaceAllString(s, "\n")
	s = lineBreak.ReplaceAllString(s, "\n")
	s = stripTags(s)
	return decodeEntities(s)
}

func stripTags(s string) string { return anyTag.ReplaceAllString(s, " ") }

func collapseBlankLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = manySpaces.ReplaceAllString(s, " ")
	s = trailingWS.ReplaceAllString(s, "\n")
	s = manyBlank.ReplaceAllString(s, "\n\n")

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, strings.TrimSpace(line))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

var namedEntities = strings.NewReplacer(
	"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
	"&apos;", "'", "&#39;", "'", "&mdash;", "—", "&ndash;", "–",
	"&hellip;", "…", "&rsquo;", "'", "&lsquo;", "'", "&ldquo;", `"`, "&rdquo;", `"`,
)

func decodeEntities(s string) string {
	s = namedEntities.Replace(s)
	return numericEnt.ReplaceAllStringFunc(s, func(match string) string {
		m := numericEnt.FindStringSubmatch(match)
		if m == nil {
			return match
		}
		digits, base := m[1], 10
		if strings.HasPrefix(strings.ToLower(digits), "x") {
			digits, base = digits[1:], 16
		}
		code, err := strconv.ParseInt(digits, base, 32)
		if err != nil || code <= 0 || code > 0x10FFFF {
			return match
		}
		return string(rune(code))
	})
}
