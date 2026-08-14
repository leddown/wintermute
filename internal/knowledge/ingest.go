package knowledge

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxDocumentBytes bounds one upload. Large enough for a consolidated
// regulation or a policy set, small enough that a mistaken upload cannot be
// read into memory, chunked and stored before anyone notices.
const MaxDocumentBytes = 25 << 20 // 25 MiB

// Extraction is the text pulled out of an upload.
type Extraction struct {
	Text string
	// Via names what read it, which is the first thing to check when a
	// document searches badly.
	Via string
}

// Extract reads text out of an uploaded file.
//
// PDFs go through pdftotext. There is no pure-Go fallback on purpose: this
// program has two direct dependencies and a PDF parser would be a third, for a
// job poppler already does better — its layout mode preserves the heading
// structure that the chunker splits on. A server without it gets a clear error
// naming the package to install rather than a silently worse extraction.
func Extract(filename, mediaType string, body []byte) (*Extraction, error) {
	if len(body) == 0 {
		return nil, Invalidf("the uploaded file is empty")
	}
	if len(body) > MaxDocumentBytes {
		return nil, Invalidf("the uploaded file is %s; the limit is %s",
			HumanBytes(int64(len(body))), HumanBytes(MaxDocumentBytes))
	}

	switch ext := strings.ToLower(filepath.Ext(filename)); ext {
	case ".pdf":
		return extractPDF(body)
	case ".txt", ".text", ".md", ".markdown", ".csv", ".json", ".yaml", ".yml", "":
		if !utf8.Valid(body) {
			return nil, Invalidf("%s is not valid UTF-8 text", filename)
		}
		return &Extraction{Text: Normalize(string(body)), Via: "direct read"}, nil
	case ".htm", ".html":
		return &Extraction{Text: Normalize(htmlToText(string(body))), Via: "html strip"}, nil
	default:
		return nil, Invalidf("unsupported document type %q (supported: .pdf, .txt, .md, .html, .csv, .json, .yaml)", ext)
	}
}

func extractPDF(body []byte) (*Extraction, error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return nil, Invalidf("reading PDFs needs the pdftotext command (install poppler-utils); " +
			"convert the document to text or markdown to upload it without")
	}

	tmp, err := os.CreateTemp("", "wm-upload-*.pdf")
	if err != nil {
		return nil, fmt.Errorf("create temporary file: %w", err)
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temporary file: %w", err)
	}

	var out, stderr bytes.Buffer
	// #nosec G204 -- fixed binary resolved via LookPath, fixed flags, and a
	// temporary path this function created; no shell is involved.
	cmd := exec.Command(bin, "-layout", "-enc", "UTF-8", path, "-")
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, Invalidf("pdftotext could not read this PDF: %s",
			strings.TrimSpace(firstLine(stderr.String())))
	}

	text := Normalize(out.String())
	if strings.TrimSpace(text) == "" {
		return nil, Invalidf("no text could be extracted — this is probably a scanned PDF and needs OCR first")
	}
	return &Extraction{Text: text, Via: "pdftotext -layout"}, nil
}

var (
	crlf         = regexp.MustCompile(`\r\n?`)
	trailingWS   = regexp.MustCompile(`[ \t]+\n`)
	tooManyLines = regexp.MustCompile(`\n{4,}`)
	// Alternation rather than a backreference: Go's regexp is RE2, which has
	// none. Closing the wrong one of the two would only over-strip.
	scriptStyle = regexp.MustCompile(`(?is)<script\b.*?</script\s*>|<style\b.*?</style\s*>`)
	htmlTag     = regexp.MustCompile(`(?s)<[^>]+>`)
)

// Normalize applies conservative whitespace cleanup only. It does not strip
// page furniture: dropping content silently is worse than a chunk containing a
// page number.
func Normalize(s string) string {
	s = strings.ReplaceAll(s, "\ufeff", "")
	s = strings.ReplaceAll(s, "\f", "\n")
	s = crlf.ReplaceAllString(s, "\n")
	s = trailingWS.ReplaceAllString(s, "\n")
	s = tooManyLines.ReplaceAllString(s, "\n\n\n")
	return strings.TrimSpace(s) + "\n"
}

func htmlToText(s string) string {
	s = scriptStyle.ReplaceAllString(s, " ")
	s = htmlTag.ReplaceAllString(s, "\n")
	replacer := strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
	return replacer.Replace(s)
}

// Chunking bounds. A chunk is a passage under one heading; oversized passages
// are split, and tiny ones are folded into the previous chunk so a heading with
// one line under it does not become its own retrieval unit.
const (
	maxChunkChars = 4000
	minChunkChars = 200
)

var (
	markdownHeading = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(\S.*)$`)
	numberedHeading = regexp.MustCompile(`^\s{0,4}((?:\d+\.){0,5}\d+|[A-Z]{1,3}\.)\s+(\S.{0,90})$`)
	articleHeading  = regexp.MustCompile(`^\s{0,4}((?:Article|Section|Clause|Annex|Appendix|Chapter|Part)\s+[0-9IVXLC]+[A-Za-z]?)\b\s*(.*)$`)
)

// ChunkText splits text into section-aware chunks.
//
// Boundaries follow headings — markdown, numbered clauses, and the
// "Article 17" style that regulations and policies use — rather than a fixed
// width, so a retrieved chunk is a passage somebody wrote and carries the
// heading that says what it is about. That heading is scored along with the
// body, which is what lets a search for "incident reporting" find a section
// titled that even when the body says "notify the authority".
func ChunkText(text string) []Chunk {
	lines := strings.Split(Normalize(text), "\n")

	var (
		chunks  []Chunk
		heading string
		body    []string
	)

	flush := func() {
		joined := strings.TrimSpace(strings.Join(body, "\n"))
		body = body[:0]
		if joined == "" && heading == "" {
			return
		}
		for _, part := range splitOversized(joined) {
			// A very short passage is appended to the previous chunk rather
			// than standing alone: on its own it retrieves poorly and reads as
			// a fragment.
			if len(chunks) > 0 && len(part) < minChunkChars && heading == "" {
				chunks[len(chunks)-1].Body += "\n\n" + part
				continue
			}
			chunks = append(chunks, Chunk{
				Ordinal: len(chunks),
				Heading: heading,
				Body:    part,
			})
		}
	}

	for _, line := range lines {
		if h, ok := headingOf(line); ok {
			flush()
			heading = h
			continue
		}
		body = append(body, line)
	}
	flush()

	// Re-number after any folding above.
	for i := range chunks {
		chunks[i].Ordinal = i
	}
	return chunks
}

func headingOf(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || len(trimmed) > 140 {
		return "", false
	}
	if m := markdownHeading.FindStringSubmatch(line); m != nil {
		return strings.TrimSpace(m[2]), true
	}
	if m := articleHeading.FindStringSubmatch(line); m != nil {
		return strings.TrimSpace(m[1] + " " + m[2]), true
	}
	// A numbered heading has to look like a heading rather than a numbered
	// sentence: short, and not ending in prose punctuation.
	if m := numberedHeading.FindStringSubmatch(line); m != nil {
		rest := strings.TrimSpace(m[2])
		if len(rest) <= 90 && !strings.HasSuffix(rest, ".") && !strings.HasSuffix(rest, ";") &&
			len(strings.Fields(rest)) <= 12 {
			return strings.TrimSpace(m[1] + " " + rest), true
		}
	}
	return "", false
}

// splitOversized breaks a passage longer than maxChunkChars at paragraph
// boundaries, falling back to a hard split only when a single paragraph is
// itself too long.
func splitOversized(text string) []string {
	if len(text) <= maxChunkChars {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	}

	var out []string
	var current strings.Builder
	for _, para := range strings.Split(text, "\n\n") {
		if current.Len() > 0 && current.Len()+len(para) > maxChunkChars {
			out = append(out, strings.TrimSpace(current.String()))
			current.Reset()
		}
		for len(para) > maxChunkChars {
			out = append(out, strings.TrimSpace(para[:maxChunkChars]))
			para = para[maxChunkChars:]
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}
	if strings.TrimSpace(current.String()) != "" {
		out = append(out, strings.TrimSpace(current.String()))
	}
	return out
}

// HumanBytes renders a byte count for an error message.
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no detail"
	}
	return s
}
