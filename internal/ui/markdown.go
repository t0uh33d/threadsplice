package ui

import (
	"net/url"
	"regexp"
	"strings"
)

// Markdown in message bubbles: the subset agents actually write, parsed into
// a tree that templates render. html/template escapes every piece of peer
// text on the way out and nothing here assembles HTML, so a message can't
// add markup, scripts or attributes. Raw HTML shows as text, an image becomes
// a link (the page fetches nothing a peer names), and only absolute http(s)
// URLs become links.
// ponytail: no reference links, setext headings, nested quotes or lazy list
// continuation; they show as text. Add them if agents start relying on them.

// mdBlock is one block of a message.
type mdBlock struct {
	Kind  string     // p, h, list, quote, hr, table or code
	Level int        // h: 1-6
	Spans []span     // p, h, quote
	Items []mdItem   // list
	Head  [][]span   // table: header cells
	Rows  [][][]span // table: body cells
	Code  block      // code: the fenced block, fences included
}

// mdItem is a list item: Depth is its nesting (0-3) from indentation, Num an
// ordered item's marker ("2."), empty for a bullet.
type mdItem struct {
	Depth int
	Num   string
	Spans []span
}

// span is inline content: plain text (Kind ""), code, b, i, s (struck) or a
// (a link to URL). Kids hold what b, i, s and a contain.
type span struct {
	Kind string
	Text string
	URL  string
	Kids []span
}

var (
	mdHeading = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?$`)
	mdRule    = regexp.MustCompile(`^ {0,3}(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	mdItemRe  = regexp.MustCompile(`^([ \t]*)([-*+]|[0-9]{1,9}[.)])(?:[ \t]+(.*))?$`)
	mdQuote   = regexp.MustCompile(`^ {0,3}>[ \t]?(.*)$`)
	mdSep     = regexp.MustCompile(`^[ \t]*\|?(?:[ \t]*:?-+:?[ \t]*\|)*[ \t]*:?-+:?[ \t]*\|?$`)
)

// markdown splits message text into blocks: fenced code exactly as blocks
// finds it, everything else line by line.
func markdown(text string) []mdBlock {
	var out []mdBlock
	for _, b := range blocks(text) {
		if b.Code {
			out = append(out, mdBlock{Kind: "code", Code: b})
			continue
		}
		var lines []string
		for l := range strings.Lines(b.Text) {
			lines = append(lines, strings.TrimRight(l, " \t\r\n"))
		}
		out = append(out, mdLines(lines)...)
	}
	return out
}

func mdLines(lines []string) []mdBlock {
	var out []mdBlock
	var text []string
	flush := func() {
		if len(text) > 0 {
			out = append(out, mdBlock{Kind: "p", Spans: inline(strings.Join(text, "\n"))})
			text = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.TrimSpace(l) == "":
			flush()
		case mdRule.MatchString(l):
			flush()
			out = append(out, mdBlock{Kind: "hr"})
		case mdHeading.MatchString(l):
			flush()
			m := mdHeading.FindStringSubmatch(l)
			out = append(out, mdBlock{Kind: "h", Level: len(m[1]), Spans: inline(m[2])})
		case mdQuote.MatchString(l):
			flush()
			var q []string
			for ; i < len(lines) && mdQuote.MatchString(lines[i]); i++ {
				q = append(q, mdQuote.FindStringSubmatch(lines[i])[1])
			}
			i--
			out = append(out, mdBlock{Kind: "quote", Spans: inline(strings.Join(q, "\n"))})
		case mdItemRe.MatchString(l):
			flush()
			var items []mdItem
			var texts []string
			for ; i < len(lines); i++ {
				if m := mdItemRe.FindStringSubmatch(lines[i]); m != nil && !mdRule.MatchString(lines[i]) {
					num := ""
					if c := m[2][0]; c >= '0' && c <= '9' {
						num = m[2]
					}
					items = append(items, mdItem{Depth: indent(m[1]), Num: num})
					texts = append(texts, m[3])
				} else if strings.HasPrefix(lines[i], "  ") || strings.HasPrefix(lines[i], "\t") {
					texts[len(texts)-1] += "\n" + strings.TrimSpace(lines[i]) // an item's indented next line
				} else {
					break
				}
			}
			i--
			for k := range items {
				items[k].Spans = inline(texts[k])
			}
			out = append(out, mdBlock{Kind: "list", Items: items})
		case strings.Contains(l, "|") && i+1 < len(lines) && strings.Contains(lines[i+1], "|") && mdSep.MatchString(lines[i+1]):
			flush()
			t := mdBlock{Kind: "table", Head: cells(l)}
			for i += 2; i < len(lines) && strings.Contains(lines[i], "|"); i++ {
				t.Rows = append(t.Rows, cells(lines[i]))
			}
			i--
			out = append(out, t)
		default:
			text = append(text, l)
		}
	}
	flush()
	return out
}

// indent is a list item's nesting level: two spaces (or half a tab) per level.
func indent(s string) int {
	n := 0
	for _, c := range s {
		if c == '\t' {
			n += 4
		} else {
			n++
		}
	}
	return min(n/2, 3)
}

// cells splits a table row at the pipes that aren't escaped.
func cells(row string) [][]span {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, `\|`) {
		row = row[:len(row)-1]
	}
	var out [][]span
	start := 0
	for i := 0; i < len(row); i++ {
		switch row[i] {
		case '\\':
			i++
		case '|':
			out = append(out, inline(strings.TrimSpace(row[start:i])))
			start = i + 1
		}
	}
	return append(out, inline(strings.TrimSpace(row[start:])))
}

// inline parses code spans, emphasis, strikethrough, links and bare URLs.
// ponytail: the first matching closer wins (no CommonMark delimiter stack).
// A closer a search didn't find is remembered, so text full of unclosed
// markers stays linear; nesting stops at four levels.
func inline(s string) []span { return inlineAt(s, 0, false) }

func inlineAt(s string, depth int, inLink bool) []span {
	var out []span
	var text strings.Builder
	add := func(sp span) {
		if text.Len() > 0 {
			out = append(out, span{Text: text.String()})
			text.Reset()
		}
		out = append(out, sp)
	}
	// find returns the first acceptable closer at or after from, or -1. Its
	// last answer per closer is kept: the search starts only move forward,
	// so each stretch of s is scanned once per kind of closer.
	type hit struct{ from, at int }
	last := map[string]hit{}
	find := func(closer string, from int, ok func(int) bool) int {
		if h, seen := last[closer]; seen && from >= h.from && (h.at < 0 || from <= h.at) {
			return h.at
		}
		at := -1
		for j := from; j <= len(s); {
			k := strings.Index(s[j:], closer)
			if k < 0 {
				break
			}
			if ok(j + k) {
				at = j + k
				break
			}
			j += k + 1
		}
		last[closer] = hit{from, at}
		return at
	}
	nested := func(t string, link bool) []span {
		if depth >= 3 {
			return []span{{Text: t}}
		}
		return inlineAt(t, depth+1, inLink || link)
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && strings.IndexByte("\\`*_~[]()#+-.!|<>", s[i+1]) >= 0:
			text.WriteByte(s[i+1])
			i += 2
			continue
		case c == '`':
			n := 1
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			fence := s[i : i+n]
			j := find(fence, i+n, func(j int) bool { return (j+n == len(s) || s[j+n] != '`') && s[j-1] != '`' })
			if j < 0 {
				text.WriteString(fence)
				i += n
				continue
			}
			code := s[i+n : j]
			if len(code) > 1 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
				code = code[1 : len(code)-1]
			}
			add(span{Kind: "code", Text: code})
			i = j + n
			continue
		case (c == '*' || c == '_' || c == '~') && i+1 < len(s) && s[i+1] == c:
			d := s[i : i+2]
			kind := map[byte]string{'*': "b", '_': "b", '~': "s"}[c]
			if i+2 < len(s) && !space(s[i+2]) && (c != '_' || i == 0 || !word(s[i-1])) {
				j := find(d, i+3, func(j int) bool {
					return !space(s[j-1]) && (c != '_' || j+2 == len(s) || !word(s[j+2]))
				})
				if j > 0 {
					add(span{Kind: kind, Kids: nested(s[i+2:j], false)})
					i = j + 2
					continue
				}
			}
			text.WriteString(d)
			i += 2
			continue
		case c == '*' || c == '_':
			d := s[i : i+1]
			if i+1 < len(s) && !space(s[i+1]) && (c != '_' || i == 0 || !word(s[i-1])) {
				j := find(d, i+2, func(j int) bool {
					return !space(s[j-1]) && s[j-1] != c && (j+1 == len(s) || s[j+1] != c) &&
						(c != '_' || j+1 == len(s) || !word(s[j+1]))
				})
				if j > 0 {
					add(span{Kind: "i", Kids: nested(s[i+1:j], false)})
					i = j + 1
					continue
				}
			}
		case c == '[' && !inLink:
			if k := find("](", i+1, func(int) bool { return true }); k > 0 {
				if m := find(")", k+2, func(int) bool { return true }); m > 0 && linkable(s[k+2:m]) {
					add(span{Kind: "a", URL: s[k+2 : m], Kids: nested(s[i+1:k], true)})
					i = m + 1
					continue
				}
			}
		case c == 'h' && !inLink && (i == 0 || !word(s[i-1]) && s[i-1] != '/') &&
			(strings.HasPrefix(s[i:], "https://") || strings.HasPrefix(s[i:], "http://")):
			u := bareURL(s[i:])
			if linkable(u) {
				add(span{Kind: "a", URL: u, Kids: []span{{Text: u}}})
			} else {
				text.WriteString(u) // as text, and scanned once
			}
			i += len(u)
			continue
		}
		text.WriteByte(c)
		i++
	}
	if text.Len() > 0 {
		out = append(out, span{Text: text.String()})
	}
	return out
}

// bareURL is the URL at the start of s, without trailing punctuation that
// more likely ends the sentence (a ")" stays when the URL opened one).
func bareURL(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool {
		return r <= ' ' || strings.ContainsRune("<>\"'`", r) || r == 0x7f
	})
	if end < 0 {
		end = len(s)
	}
	u := s[:end]
	for len(u) > 0 {
		last := u[len(u)-1]
		if strings.IndexByte(".,;:!?*_~", last) >= 0 || last == ')' && strings.Count(u, "(") < strings.Count(u, ")") {
			u = u[:len(u)-1]
			continue
		}
		break
	}
	return u
}

// linkable reports whether a peer's URL may become a link: absolute http or
// https with a host, and nothing that ends an attribute or a URL early.
func linkable(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n\"'<>`\\") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func space(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func word(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// plainText is a block's text without markup, for quoting it in a reply.
func plainText(b mdBlock) string {
	switch b.Kind {
	case "code":
		return b.Code.Body()
	case "list":
		var parts []string
		for _, it := range b.Items {
			parts = append(parts, plain(it.Spans))
		}
		return strings.Join(parts, " ")
	case "table":
		var parts []string
		for _, c := range b.Head {
			parts = append(parts, plain(c))
		}
		return strings.Join(parts, " ")
	}
	return plain(b.Spans)
}

func plain(ss []span) string {
	var b strings.Builder
	for _, s := range ss {
		if s.Kids != nil {
			b.WriteString(plain(s.Kids))
		} else {
			b.WriteString(s.Text)
		}
	}
	return b.String()
}
