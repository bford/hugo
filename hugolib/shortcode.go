// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hugolib

import (
	"bytes"
	"fmt"
	"html/template"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gohugoio/hugo/helpers"

	"errors"

	"github.com/gohugoio/hugo/common/herrors"

	"github.com/gohugoio/hugo/parser/pageparser"
	"github.com/gohugoio/hugo/resources/page"

	"github.com/gohugoio/hugo/common/maps"
	"github.com/gohugoio/hugo/common/text"
	"github.com/gohugoio/hugo/common/urls"
	"github.com/gohugoio/hugo/output"

	bp "github.com/gohugoio/hugo/bufferpool"
	"github.com/gohugoio/hugo/tpl"
)

var (
	_ urls.RefLinker  = (*ShortcodeWithPage)(nil)
	_ pageWrapper     = (*ShortcodeWithPage)(nil)
	_ text.Positioner = (*ShortcodeWithPage)(nil)
)

// ShortcodeWithPage is the "." context in a shortcode template.
type ShortcodeWithPage struct {
	Params        any
	Inner         template.HTML
	Page          page.Page
	Parent        *ShortcodeWithPage
	Name          string
	IsNamedParams bool

	// Zero-based ordinal in relation to its parent. If the parent is the page itself,
	// this ordinal will represent the position of this shortcode in the page content.
	Ordinal int

	// Indentation before the opening shortcode in the source.
	indentation string

	innerDeindentInit sync.Once
	innerDeindent     template.HTML

	// pos is the position in bytes in the source file. Used for error logging.
	posInit   sync.Once
	posOffset int
	pos       text.Position

	scratch *maps.Scratch
}

// InnerDeindent returns the (potentially de-indented) inner content of the shortcode.
func (scp *ShortcodeWithPage) InnerDeindent() template.HTML {
	if scp.indentation == "" {
		return scp.Inner
	}
	scp.innerDeindentInit.Do(func() {
		b := bp.GetBuffer()
		text.VisitLinesAfter(string(scp.Inner), func(s string) {
			if strings.HasPrefix(s, scp.indentation) {
				b.WriteString(strings.TrimPrefix(s, scp.indentation))
			} else {
				b.WriteString(s)
			}
		})
		scp.innerDeindent = template.HTML(b.String())
		bp.PutBuffer(b)
	})

	return scp.innerDeindent
}

// Position returns this shortcode's detailed position. Note that this information
// may be expensive to calculate, so only use this in error situations.
func (scp *ShortcodeWithPage) Position() text.Position {
	scp.posInit.Do(func() {
		if p, ok := mustUnwrapPage(scp.Page).(pageContext); ok {
			scp.pos = p.posOffset(scp.posOffset)
		}
	})
	return scp.pos
}

// Site returns information about the current site.
func (scp *ShortcodeWithPage) Site() page.Site {
	return scp.Page.Site()
}

// Ref is a shortcut to the Ref method on Page. It passes itself as a context
// to get better error messages.
func (scp *ShortcodeWithPage) Ref(args map[string]any) (string, error) {
	return scp.Page.RefFrom(args, scp)
}

// RelRef is a shortcut to the RelRef method on Page. It passes itself as a context
// to get better error messages.
func (scp *ShortcodeWithPage) RelRef(args map[string]any) (string, error) {
	return scp.Page.RelRefFrom(args, scp)
}

// Scratch returns a scratch-pad scoped for this shortcode. This can be used
// as a temporary storage for variables, counters etc.
func (scp *ShortcodeWithPage) Scratch() *maps.Scratch {
	if scp.scratch == nil {
		scp.scratch = maps.NewScratch()
	}
	return scp.scratch
}

// Get is a convenience method to look up shortcode parameters by its key.
func (scp *ShortcodeWithPage) Get(key any) any {
	if scp.Params == nil {
		return nil
	}
	if reflect.ValueOf(scp.Params).Len() == 0 {
		return nil
	}

	var x reflect.Value

	switch key.(type) {
	case int64, int32, int16, int8, int:
		if reflect.TypeOf(scp.Params).Kind() == reflect.Map {
			// We treat this as a non error, so people can do similar to
			// {{ $myParam := .Get "myParam" | default .Get 0 }}
			// Without having to do additional checks.
			return nil
		} else if reflect.TypeOf(scp.Params).Kind() == reflect.Slice {
			idx := int(reflect.ValueOf(key).Int())
			ln := reflect.ValueOf(scp.Params).Len()
			if idx > ln-1 {
				return ""
			}
			x = reflect.ValueOf(scp.Params).Index(idx)
		}
	case string:
		if reflect.TypeOf(scp.Params).Kind() == reflect.Map {
			x = reflect.ValueOf(scp.Params).MapIndex(reflect.ValueOf(key))
			if !x.IsValid() {
				return ""
			}
		} else if reflect.TypeOf(scp.Params).Kind() == reflect.Slice {
			// We treat this as a non error, so people can do similar to
			// {{ $myParam := .Get "myParam" | default .Get 0 }}
			// Without having to do additional checks.
			return nil
		}
	}

	return x.Interface()
}

func (scp *ShortcodeWithPage) page() page.Page {
	return scp.Page
}

// Note - this value must not contain any markup syntax
const shortcodePlaceholderPrefix = "HAHAHUGOSHORTCODE"

func createShortcodePlaceholder(id string, ordinal int) string {
	return shortcodePlaceholderPrefix + "-" + id + strconv.Itoa(ordinal) + "-HBHB"
}

type shortcode struct {
	name      string
	isInline  bool  // inline shortcode. Any inner will be a Go template.
	isClosing bool  // whether a closing tag was provided
	inner     []any // string or nested shortcode
	params    any   // map or array
	ordinal   int
	err       error

	indentation string // indentation from source.

	info   tpl.Info       // One of the output formats (arbitrary)
	templs []tpl.Template // All output formats

	// If set, the rendered shortcode is sent as part of the surrounding content
	// to Goldmark and similar.
	// Before Hug0 0.55 we didn't send any shortcode output to the markup
	// renderer, and this flag told Hugo to process the {{ .Inner }} content
	// separately.
	// The old behaviour can be had by starting your shortcode template with:
	//    {{ $_hugo_config := `{ "version": 1 }`}}
	doMarkup bool

	// the placeholder in the source when passed to Goldmark etc.
	// This also identifies the rendered shortcode.
	placeholder string

	pos    int // the position in bytes in the source file
	length int // the length in bytes in the source file
}

func (s shortcode) insertPlaceholder() bool {
	return !s.doMarkup || s.configVersion() == 1
}

func (s shortcode) configVersion() int {
	if s.info == nil {
		// Not set for inline shortcodes.
		return 2
	}

	return s.info.ParseInfo().Config.Version
}

func (s shortcode) innerString() string {
	var sb strings.Builder

	for _, inner := range s.inner {
		sb.WriteString(inner.(string))
	}

	return sb.String()
}

func (sc shortcode) String() string {
	// for testing (mostly), so any change here will break tests!
	var params any
	switch v := sc.params.(type) {
	case map[string]any:
		// sort the keys so test assertions won't fail
		var keys []string
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tmp := make(map[string]any)

		for _, k := range keys {
			tmp[k] = v[k]
		}
		params = tmp

	default:
		// use it as is
		params = sc.params
	}

	return fmt.Sprintf("%s(%q, %t){%s}", sc.name, params, sc.doMarkup, sc.inner)
}

type shortcodeHandler struct {
	p *pageState

	s *Site

	// Ordered list of shortcodes for a page.
	shortcodes []*shortcode

	// All the shortcode names in this set.
	nameSet   map[string]bool
	nameSetMu sync.RWMutex

	// Configuration
	enableInlineShortcodes bool
}

func newShortcodeHandler(p *pageState, s *Site) *shortcodeHandler {
	sh := &shortcodeHandler{
		p:                      p,
		s:                      s,
		enableInlineShortcodes: s.ExecHelper.Sec().EnableInlineShortcodes,
		shortcodes:             make([]*shortcode, 0, 4),
		nameSet:                make(map[string]bool),
	}

	return sh
}

const (
	innerNewlineRegexp = "\n"
	innerCleanupRegexp = `\A<p>(.*)</p>\n\z`
	innerCleanupExpand = "$1"
)

func renderShortcode(
	level int,
	s *Site,
	tplVariants tpl.TemplateVariants,
	sc *shortcode,
	parent *ShortcodeWithPage,
	p *pageState) (string, bool, error) {
	var tmpl tpl.Template

	// Tracks whether this shortcode or any of its children has template variations
	// in other languages or output formats. We are currently only interested in
	// the output formats, so we may get some false positives -- we
	// should improve on that.
	var hasVariants bool

	if sc.isInline {
		if !p.s.ExecHelper.Sec().EnableInlineShortcodes {
			return "", false, nil
		}
		templName := path.Join("_inline_shortcode", p.File().Path(), sc.name)
		if sc.isClosing {
			templStr := sc.innerString()

			var err error
			tmpl, err = s.TextTmpl().Parse(templName, templStr)
			if err != nil {
				fe := herrors.NewFileErrorFromName(err, p.File().Filename())
				pos := fe.Position()
				pos.LineNumber += p.posOffset(sc.pos).LineNumber
				fe = fe.UpdatePosition(pos)
				return "", false, p.wrapError(fe)
			}

		} else {
			// Re-use of shortcode defined earlier in the same page.
			var found bool
			tmpl, found = s.TextTmpl().Lookup(templName)
			if !found {
				return "", false, fmt.Errorf("no earlier definition of shortcode %q found", sc.name)
			}
		}
	} else {
		var found, more bool
		tmpl, found, more = s.Tmpl().LookupVariant(sc.name, tplVariants)
		if !found {
			s.Log.Errorf("Unable to locate template for shortcode %q in page %q", sc.name, p.File().Path())
			return "", false, nil
		}
		hasVariants = hasVariants || more
	}

	data := &ShortcodeWithPage{Ordinal: sc.ordinal, posOffset: sc.pos, indentation: sc.indentation, Params: sc.params, Page: newPageForShortcode(p), Parent: parent, Name: sc.name}
	if sc.params != nil {
		data.IsNamedParams = reflect.TypeOf(sc.params).Kind() == reflect.Map
	}

	if len(sc.inner) > 0 {
		var inner string
		for _, innerData := range sc.inner {
			switch innerData := innerData.(type) {
			case string:
				inner += innerData
			case *shortcode:
				s, more, err := renderShortcode(level+1, s, tplVariants, innerData, data, p)
				if err != nil {
					return "", false, err
				}
				hasVariants = hasVariants || more
				inner += s
			default:
				s.Log.Errorf("Illegal state on shortcode rendering of %q in page %q. Illegal type in inner data: %s ",
					sc.name, p.File().Path(), reflect.TypeOf(innerData))
				return "", false, nil
			}
		}

		// Pre Hugo 0.55 this was the behaviour even for the outer-most
		// shortcode.
		if sc.doMarkup && (level > 0 || sc.configVersion() == 1) {
			var err error
			b, err := p.pageOutput.contentRenderer.RenderContent([]byte(inner), false)
			if err != nil {
				return "", false, err
			}

			newInner := b.Bytes()

			// If the type is “” (unknown) or “markdown”, we assume the markdown
			// generation has been performed. Given the input: `a line`, markdown
			// specifies the HTML `<p>a line</p>\n`. When dealing with documents as a
			// whole, this is OK. When dealing with an `{{ .Inner }}` block in Hugo,
			// this is not so good. This code does two things:
			//
			// 1.  Check to see if inner has a newline in it. If so, the Inner data is
			//     unchanged.
			// 2   If inner does not have a newline, strip the wrapping <p> block and
			//     the newline.
			switch p.m.markup {
			case "", "markdown":
				if match, _ := regexp.MatchString(innerNewlineRegexp, inner); !match {
					cleaner, err := regexp.Compile(innerCleanupRegexp)

					if err == nil {
						newInner = cleaner.ReplaceAll(newInner, []byte(innerCleanupExpand))
					}
				}
			}

			// TODO(bep) we may have plain text inner templates.
			data.Inner = template.HTML(newInner)
		} else {
			data.Inner = template.HTML(inner)
		}

	}

	result, err := renderShortcodeWithPage(s.Tmpl(), tmpl, data)

	if err != nil && sc.isInline {
		fe := herrors.NewFileErrorFromName(err, p.File().Filename())
		pos := fe.Position()
		pos.LineNumber += p.posOffset(sc.pos).LineNumber
		fe = fe.UpdatePosition(pos)
		return "", false, fe
	}

	if len(sc.inner) == 0 && len(sc.indentation) > 0 {
		b := bp.GetBuffer()
		i := 0
		text.VisitLinesAfter(result, func(line string) {
			// The first line is correctly indented.
			if i > 0 {
				b.WriteString(sc.indentation)
			}
			i++
			b.WriteString(line)
		})

		result = b.String()
		bp.PutBuffer(b)
	}

	return result, hasVariants, err
}

func (s *shortcodeHandler) hasShortcodes() bool {
	return s != nil && len(s.shortcodes) > 0
}

func (s *shortcodeHandler) addName(name string) {
	s.nameSetMu.Lock()
	defer s.nameSetMu.Unlock()
	s.nameSet[name] = true
}

func (s *shortcodeHandler) transferNames(in *shortcodeHandler) {
	s.nameSetMu.Lock()
	defer s.nameSetMu.Unlock()
	for k := range in.nameSet {
		s.nameSet[k] = true
	}

}

func (s *shortcodeHandler) hasName(name string) bool {
	s.nameSetMu.RLock()
	defer s.nameSetMu.RUnlock()
	_, ok := s.nameSet[name]
	return ok
}

func (s *shortcodeHandler) renderShortcodesForPage(p *pageState, f output.Format) (map[string]string, bool, error) {
	rendered := make(map[string]string)

	tplVariants := tpl.TemplateVariants{
		Language:     p.Language().Lang,
		OutputFormat: f,
	}

	var hasVariants bool

	for _, v := range s.shortcodes {
		s, more, err := renderShortcode(0, s.s, tplVariants, v, nil, p)
		if err != nil {
			err = p.parseError(fmt.Errorf("failed to render shortcode %q: %w", v.name, err), p.source.parsed.Input(), v.pos)
			return nil, false, err
		}
		hasVariants = hasVariants || more
		rendered[v.placeholder] = s

	}

	return rendered, hasVariants, nil
}

func (s *shortcodeHandler) parseError(err error, input []byte, pos int) error {
	if s.p != nil {
		return s.p.parseError(err, input, pos)
	}
	return err
}

// pageTokens state:
// - before: positioned just before the shortcode start
// - after: shortcode(s) consumed (plural when they are nested)
func (s *shortcodeHandler) extractShortcode(ordinal, level int, source []byte, pt *pageparser.Iterator) (*shortcode, error) {
	if s == nil {
		panic("handler nil")
	}
	sc := &shortcode{ordinal: ordinal}

	// Back up one to identify any indentation.
	if pt.Pos() > 0 {
		pt.Backup()
		item := pt.Next()
		if item.IsIndentation() {
			sc.indentation = item.ValStr(source)
		}
	}

	cnt := 0
	nestedOrdinal := 0
	nextLevel := level + 1
	const errorPrefix = "failed to extract shortcode"

	fail := func(err error, i pageparser.Item) error {
		return s.parseError(fmt.Errorf("%s: %w", errorPrefix, err), source, i.Pos())
	}

Loop:
	for {
		currItem := pt.Next()
		switch {
		case currItem.IsLeftShortcodeDelim():
			next := pt.Peek()
			if next.IsRightShortcodeDelim() {
				// no name: {{< >}} or {{% %}}
				return sc, errors.New("shortcode has no name")
			}
			if next.IsShortcodeClose() {
				continue
			}

			if cnt > 0 {
				// nested shortcode; append it to inner content
				pt.Backup()
				nested, err := s.extractShortcode(nestedOrdinal, nextLevel, source, pt)
				nestedOrdinal++
				if nested != nil && nested.name != "" {
					s.addName(nested.name)
				}

				if err == nil {
					sc.inner = append(sc.inner, nested)
				} else {
					return sc, err
				}

			} else {
				sc.doMarkup = currItem.IsShortcodeMarkupDelimiter()
			}

			cnt++

		case currItem.IsRightShortcodeDelim():
			// we trust the template on this:
			// if there's no inner, we're done
			if !sc.isInline {
				if sc.info == nil {
					// This should not happen.
					return sc, fail(errors.New("BUG: template info not set"), currItem)
				}
				if !sc.info.ParseInfo().IsInner {
					return sc, nil
				}
			}

		case currItem.IsShortcodeClose():
			next := pt.Peek()
			if !sc.isInline {
				if sc.info == nil || !sc.info.ParseInfo().IsInner {
					if next.IsError() {
						// return that error, more specific
						continue
					}
					return sc, fail(fmt.Errorf("shortcode %q has no .Inner, yet a closing tag was provided", next.ValStr(source)), next)
				}
			}
			if next.IsRightShortcodeDelim() {
				// self-closing
				pt.Consume(1)
			} else {
				sc.isClosing = true
				pt.Consume(2)
			}

			return sc, nil
		case currItem.IsText():
			sc.inner = append(sc.inner, currItem.ValStr(source))
		case currItem.Type == pageparser.TypeEmoji:
			// TODO(bep) avoid the duplication of these "text cases", to prevent
			// more of #6504 in the future.
			val := currItem.ValStr(source)
			if emoji := helpers.Emoji(val); emoji != nil {
				sc.inner = append(sc.inner, string(emoji))
			} else {
				sc.inner = append(sc.inner, val)
			}
		case currItem.IsShortcodeName():

			sc.name = currItem.ValStr(source)

			// Used to check if the template expects inner content.
			templs := s.s.Tmpl().LookupVariants(sc.name)
			if templs == nil {
				return nil, fmt.Errorf("%s: template for shortcode %q not found", errorPrefix, sc.name)
			}

			sc.info = templs[0].(tpl.Info)
			sc.templs = templs
		case currItem.IsInlineShortcodeName():
			sc.name = currItem.ValStr(source)
			sc.isInline = true
		case currItem.IsShortcodeParam():
			if !pt.IsValueNext() {
				continue
			} else if pt.Peek().IsShortcodeParamVal() {
				// named params
				if sc.params == nil {
					params := make(map[string]any)
					params[currItem.ValStr(source)] = pt.Next().ValTyped(source)
					sc.params = params
				} else {
					if params, ok := sc.params.(map[string]any); ok {
						params[currItem.ValStr(source)] = pt.Next().ValTyped(source)
					} else {
						return sc, fmt.Errorf("%s: invalid state: invalid param type %T for shortcode %q, expected a map", errorPrefix, params, sc.name)
					}
				}
			} else {
				// positional params
				if sc.params == nil {
					var params []any
					params = append(params, currItem.ValTyped(source))
					sc.params = params
				} else {
					if params, ok := sc.params.([]any); ok {
						params = append(params, currItem.ValTyped(source))
						sc.params = params
					} else {
						return sc, fmt.Errorf("%s: invalid state: invalid param type %T for shortcode %q, expected a slice", errorPrefix, params, sc.name)
					}
				}
			}
		case currItem.IsDone():
			// handled by caller
			pt.Backup()
			break Loop

		}
	}
	return sc, nil
}

// Replace prefixed shortcode tokens with the real content.
// Note: This function will rewrite the input slice.
func replaceShortcodeTokens(source []byte, replacements map[string]string) ([]byte, error) {
	if len(replacements) == 0 {
		return source, nil
	}

	start := 0

	pre := []byte(shortcodePlaceholderPrefix)
	post := []byte("HBHB")
	pStart := []byte("<p>")
	pEnd := []byte("</p>")

	k := bytes.Index(source[start:], pre)

	for k != -1 {
		j := start + k
		postIdx := bytes.Index(source[j:], post)
		if postIdx < 0 {
			// this should never happen, but let the caller decide to panic or not
			return nil, errors.New("illegal state in content; shortcode token missing end delim")
		}

		end := j + postIdx + 4

		newVal := []byte(replacements[string(source[j:end])])

		// Issue #1148: Check for wrapping p-tags <p>
		if j >= 3 && bytes.Equal(source[j-3:j], pStart) {
			if (k+4) < len(source) && bytes.Equal(source[end:end+4], pEnd) {
				j -= 3
				end += 4
			}
		}

		// This and other cool slice tricks: https://github.com/golang/go/wiki/SliceTricks
		source = append(source[:j], append(newVal, source[end:]...)...)
		start = j
		k = bytes.Index(source[start:], pre)

	}

	return source, nil
}

func renderShortcodeWithPage(h tpl.TemplateHandler, tmpl tpl.Template, data *ShortcodeWithPage) (string, error) {
	buffer := bp.GetBuffer()
	defer bp.PutBuffer(buffer)

	err := h.Execute(tmpl, buffer, data)
	if err != nil {
		return "", fmt.Errorf("failed to process shortcode: %w", err)
	}
	return buffer.String(), nil
}
