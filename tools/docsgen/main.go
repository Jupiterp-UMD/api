// Command docsgen renders docs.md into docs.html.
//
//	go generate ./...          # from the api/ directory
//	go run ./tools/docsgen     # equivalently
//
// The two files used to be maintained by hand, in parallel. That survived
// while the API had seven stable endpoints and stopped surviving the grade
// work, which added 235 lines to one and 439 to the other in a single commit;
// they had already drifted by the time this was written. Markdown is the
// source now and docs.html is a build artifact, still committed so that
// deploying the binary does not require a generation step.
//
// Two things are deliberately reproduced rather than improved:
//
//   - The heading anchor scheme. `docs.md` is full of internal "jump" links
//     written against the ids the previous generator produced, and changing
//     the scheme would break every one of them along with any external link
//     into a section of the docs.
//
//   - The page shell. Same doctype, same favicon, same docs.css.
//
// One thing is deliberately dropped: the old output carried highlight.js
// `hljs-*` spans on every code block. docs.css styles none of them, so they
// were several hundred lines of markup with no rendered effect.
package main

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

const (
	inputPath  = "docs.md"
	outputPath = "docs.html"
)

const header = `<!DOCTYPE html>
<html>
    <head>
        <meta charset="utf-8">
        <title>Jupiterp API Docs</title>
        <meta name="viewport" content="width=device-width, initial-scale=1">
        <link rel="icon" href="/favicon.svg?" type="image/svg+xml">
        <link rel="stylesheet" type="text/css" href="docs.css">
    </head>
    <body>
`

const footer = `    </body>
</html>
`

// Runs of anything that is not a lowercase letter or digit.
var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// slugID reproduces the anchor ids the previous generator produced.
//
// Lowercase, then every run of non-alphanumeric characters becomes a single
// hyphen. Leading and trailing hyphens are NOT trimmed, which is why the
// headings in docs.md link to "#-v0-courses-" rather than "#v0-courses": the
// backticks around the path each became a hyphen. Trimming would be tidier and
// would break every existing link.
func slugID(heading string) string {
	return nonSlugChars.ReplaceAllString(strings.ToLower(heading), "-")
}

// headingIDs assigns ids using slugID, deduplicating repeats the way the
// previous generator did.
type headingIDs struct {
	seen map[string]int
}

func (h *headingIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	if kind != ast.KindHeading {
		return value
	}
	id := slugID(string(value))
	if h.seen == nil {
		h.seen = map[string]int{}
	}
	h.seen[id]++
	if n := h.seen[id]; n > 1 {
		id = fmt.Sprintf("%s-%d", id, n-1)
	}
	return []byte(id)
}

func (h *headingIDs) Put(value []byte) {}

func main() {
	source, err := os.ReadFile(inputPath)
	if err != nil {
		fatal("reading %s: %v", inputPath, err)
	}

	md := goldmark.New(
		// GFM gives tables, which the endpoint and query-parameter listings
		// are built out of, plus strikethrough and autolinks.
		goldmark.WithExtensions(extension.GFM),
		// The id generator is supplied per-conversion through the parser
		// context below, not here: WithIDs is a context option.
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		// Raw HTML passes through. Several table cells embed <ul><li> lists,
		// which markdown cannot express inside a cell, and escaping them
		// renders the markup as literal text. docs.md is a file in this
		// repository rather than user input, so there is nothing to sanitise
		// it against.
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)

	var body bytes.Buffer
	ctx := parser.NewContext(parser.WithIDs(&headingIDs{}))
	if err := md.Convert(source, &body, parser.WithContext(ctx)); err != nil {
		fatal("rendering markdown: %v", err)
	}

	var out bytes.Buffer
	out.WriteString(header)
	out.Write(indent(body.Bytes(), "        "))
	out.WriteString(footer)

	if err := os.WriteFile(outputPath, out.Bytes(), 0o644); err != nil {
		fatal("writing %s: %v", outputPath, err)
	}

	fmt.Printf("wrote %s (%d bytes) from %s\n", outputPath, out.Len(), inputPath)
	verifyAnchors(source, out.Bytes())
}

// indent shifts rendered HTML to sit inside the <body> block, matching how the
// committed file has always been laid out. Lines inside <pre> are left alone:
// leading whitespace there is content.
func indent(src []byte, prefix string) []byte {
	var out bytes.Buffer
	inPre := false
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "<pre") {
			inPre = true
		}
		if inPre || strings.TrimSpace(line) == "" {
			out.WriteString(line)
		} else {
			out.WriteString(prefix + line)
		}
		if strings.Contains(line, "</pre>") {
			inPre = false
		}
		out.WriteString("\n")
	}
	return out.Bytes()
}

var linkRef = regexp.MustCompile(`\]\(#([^)]+)\)`)

// verifyAnchors reports internal "jump" links whose target heading does not
// exist in the generated HTML.
//
// docs.md is one large table of contents pointing into itself, and a renamed
// heading breaks those links silently -- the page still renders, the link just
// goes nowhere. This turns that into build output.
func verifyAnchors(source, rendered []byte) {
	html := string(rendered)
	var broken []string
	seen := map[string]bool{}

	for _, match := range linkRef.FindAllSubmatch(source, -1) {
		anchor := string(match[1])
		if seen[anchor] {
			continue
		}
		seen[anchor] = true
		if !strings.Contains(html, `id="`+anchor+`"`) {
			broken = append(broken, anchor)
		}
	}

	if len(broken) == 0 {
		fmt.Printf("all %d internal anchors resolve\n", len(seen))
		return
	}
	fmt.Fprintf(os.Stderr, "\n%d internal link(s) point at a heading that does not exist:\n", len(broken))
	for _, anchor := range broken {
		fmt.Fprintf(os.Stderr, "  #%s\n", anchor)
	}
	os.Exit(1)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "docsgen: "+format+"\n", args...)
	os.Exit(1)
}
