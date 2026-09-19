package compile

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// This file owns the documentation dialect: the strict selector grammar of one
// captured HTML documentation family, the byte-exact verification that every
// declared selector exists in the pinned capture, and the resolved view the
// loader and the normalizer read. A documentation anchor is not a JSON
// pointer: it names one declared identifier in one pinned document, and its
// witness is the exact start tag that declares it.

const (
	// maxDocumentationAnchorBytes bounds one anchor name.
	maxDocumentationAnchorBytes = 128
	// maxDocumentationTagBytes bounds the declaring start tag so a pathological
	// capture cannot make witness extraction unbounded.
	maxDocumentationTagBytes = 4096
	// documentationAnchorAttribute is the HTML attribute that declares an
	// anchor. The PostgreSQL documentation declares section anchors with it.
	documentationAnchorAttribute = `id="`
)

// isDocumentationDialect reports whether the dialect describes connector
// behavior through pinned official documentation pages rather than through a
// machine-readable interface description.
func isDocumentationDialect(dialect Dialect) bool {
	switch dialect {
	case DialectPostgreSQLDocs16:
		return true
	default:
		return false
	}
}

// validSourceSelector admits one declared source selector under the source
// set's own grammar: an RFC 6901 JSON pointer for the interface-description
// dialects, or a documentation anchor for the documentation dialect. The
// OpenAPI pointer grammar and behaviour are unchanged.
func validSourceSelector(dialect Dialect, selector string) bool {
	if isDocumentationDialect(dialect) {
		return validDocumentationSelector(selector)
	}
	return selector != "" && validJSONPointer(strings.TrimPrefix(selector, "#"))
}

// validDocumentationSelector admits exactly one documentation anchor selector:
// a leading "#" followed by a bounded anchor name. A malformed selector is
// refused before any captured byte is read.
func validDocumentationSelector(selector string) bool {
	anchor, ok := documentationSelectorAnchor(selector)
	return ok && validDocumentationAnchor(anchor)
}

// documentationSelectorAnchor splits one well-formed selector into its anchor.
func documentationSelectorAnchor(selector string) (string, bool) {
	if !strings.HasPrefix(selector, "#") {
		return "", false
	}
	anchor := strings.TrimPrefix(selector, "#")
	if anchor == "" || len(anchor) > maxDocumentationAnchorBytes {
		return "", false
	}
	return anchor, true
}

// validDocumentationAnchor admits one anchor name. The grammar itself lives
// with the wire contract so one definition governs both the compiler selector
// and every emitted source reference.
func validDocumentationAnchor(anchor string) bool {
	return contract.ValidDocumentationAnchor(anchor)
}

// DocumentationAnchor is one verified anchor of one pinned document. SHA256 is
// the digest of the exact start tag that declares the anchor, so a source fact
// bound to it detects any change to the declaration site.
type DocumentationAnchor struct {
	Selector string `json:"selector"`
	SHA256   string `json:"sha256"`
}

// DocumentationDocument is one captured documentation page with the anchors its
// declarations name, in selector order.
type DocumentationDocument struct {
	Metadata SourceDocument
	Anchors  []DocumentationAnchor
	byAnchor map[string]string
}

// DocumentationSource is the resolved documentation view of one captured
// source set. It exists only for documentation dialects.
type DocumentationSource struct {
	Documents []DocumentationDocument
	byID      map[string]int
}

// AnchorSHA256 returns the exact declaration witness of one anchor.
func (source *DocumentationSource) AnchorSHA256(sourceID, anchor string) (string, bool) {
	if source == nil {
		return "", false
	}
	index, ok := source.byID[sourceID]
	if !ok {
		return "", false
	}
	digest, ok := source.Documents[index].byAnchor[anchor]
	return digest, ok
}

// Fact returns the source-linked fact of one declared selector.
func (source *DocumentationSource) Fact(sourceID, selector string) (SourceFact, bool) {
	anchor, ok := documentationSelectorAnchor(selector)
	if !ok {
		return SourceFact{}, false
	}
	digest, ok := source.AnchorSHA256(sourceID, anchor)
	if !ok {
		return SourceFact{}, false
	}
	return SourceFact{SourceRef: contract.SourceRef{SourceID: sourceID, Pointer: selector}, SHA256: "sha256:" + digest}, true
}

// declaredSelectors collects every selector the source lock declares for one
// source identity: the document's own selector plus every operation reference.
func declaredSelectors(lock SourceLock, sourceID string) []string {
	selectors := make([]string, 0, 8)
	for _, document := range lock.SourceSet.Documents {
		if document.SourceID == sourceID && document.Pointer != "" {
			selectors = append(selectors, document.Pointer)
		}
	}
	for _, operation := range lock.Operations {
		for _, reference := range operation.SourceRefs {
			if reference.SourceID == sourceID {
				selectors = append(selectors, reference.Pointer)
			}
		}
	}
	slices.Sort(selectors)
	return slices.Compact(selectors)
}

// loadDocumentationSource admits one captured documentation source set: every
// document must be bounded, valid UTF-8 and declare every selector the source
// lock names. A missing, ambiguous or unresolvable declaration is refused
// instead of being lowered into a partial source.
func loadDocumentationSource(captured *CapturedSource) (*DocumentationSource, error) {
	if captured == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "captured source is required")
	}
	source := &DocumentationSource{
		Documents: make([]DocumentationDocument, 0, len(captured.Documents)),
		byID:      make(map[string]int, len(captured.Documents)),
	}
	for _, document := range captured.Documents {
		sourceID := document.Metadata.SourceID
		if len(document.Bytes) == 0 {
			return nil, sourceError(ErrInvalidSource, sourceID, "", "captured documentation is empty")
		}
		if int64(len(document.Bytes)) > MaxSourceDocumentBytes {
			return nil, sourceError(ErrInvalidSource, sourceID, "", "captured documentation exceeds the compiler byte bound")
		}
		if !utf8.Valid(document.Bytes) {
			return nil, sourceError(ErrInvalidSource, sourceID, "", "captured documentation is not valid UTF-8")
		}
		anchors := make([]DocumentationAnchor, 0, 8)
		byAnchor := make(map[string]string, 8)
		for _, selector := range declaredSelectors(captured.Lock, sourceID) {
			anchor, ok := documentationSelectorAnchor(selector)
			if !ok {
				return nil, sourceError(ErrInvalidSource, sourceID, selector, "declared documentation selector is malformed")
			}
			if _, duplicate := byAnchor[anchor]; duplicate {
				continue
			}
			tag, err := documentationAnchorTag(document.Bytes, anchor)
			if err != nil {
				return nil, sourceError(ErrSourceFidelity, sourceID, selector, err.Error())
			}
			digest := digestHex(tag)
			byAnchor[anchor] = digest
			anchors = append(anchors, DocumentationAnchor{Selector: selector, SHA256: "sha256:" + digest})
		}
		slices.SortFunc(anchors, func(left, right DocumentationAnchor) int {
			return strings.Compare(left.Selector, right.Selector)
		})
		source.byID[sourceID] = len(source.Documents)
		source.Documents = append(source.Documents, DocumentationDocument{
			Metadata: document.Metadata, Anchors: anchors, byAnchor: byAnchor,
		})
	}
	slices.SortFunc(source.Documents, func(left, right DocumentationDocument) int {
		return strings.Compare(left.Metadata.SourceID, right.Metadata.SourceID)
	})
	source.byID = make(map[string]int, len(source.Documents))
	for index, document := range source.Documents {
		source.byID[document.Metadata.SourceID] = index
	}
	return source, nil
}

// resolveDocumentationSource is the resolution step of the documentation
// dialect: captured pages are bounded, UTF-8 and carry every declared anchor,
// so no YAML or JSON lowering is attempted on documentation bytes.
func resolveDocumentationSource(captured *CapturedSource) (*ResolvedSource, error) {
	documentation, err := loadDocumentationSource(captured)
	if err != nil {
		return nil, err
	}
	resolved := &ResolvedSource{
		Captured:      captured,
		Documentation: documentation,
		byID:          map[string]*parsedDocument{},
		refs:          map[referenceKey]ReferenceIdentity{},
	}
	for _, document := range documentation.Documents {
		resolved.Documents = append(resolved.Documents, ResolvedDocument{Metadata: document.Metadata})
	}
	return resolved, nil
}

// loadDocumentationDialect is the loader path of the documentation dialect.
// Captured bytes are already bounded and verified by resolution; this step
// refuses a source that was not resolved from its pinned bytes so no
// documentation operation can be normalized without a verified anchor index.
func loadDocumentationDialect(source *ResolvedSource) (*LoadedSource, error) {
	if source.Documentation == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "documentation source was not resolved from its captured bytes")
	}
	if len(source.Documentation.Documents) != len(source.Captured.Documents) {
		return nil, sourceError(ErrSourceFidelity, "", "", "documentation view disagrees with the captured source set")
	}
	return &LoadedSource{
		Resolved:      source,
		Documentation: source.Documentation,
		byID:          map[string]*LoadedDocument{},
	}, nil
}

// documentationAnchorTag locates the single start tag of the captured bytes
// that declares one anchor and returns its exact bytes. An anchor that is
// absent, declared more than once, or declared in markup whose start tag cannot
// be resolved exactly is refused.
func documentationAnchorTag(data []byte, anchor string) ([]byte, error) {
	declaration := []byte(documentationAnchorAttribute + anchor + `"`)
	var witness []byte
	offset := 0
	for {
		index := bytes.Index(data[offset:], declaration)
		if index < 0 {
			break
		}
		index += offset
		offset = index + len(declaration)
		if index > 0 && isHTMLNameByte(data[index-1]) {
			// A longer attribute name (data-id, aria-id) is not this anchor.
			continue
		}
		start, end, ok := documentationTagSpan(data, index)
		if !ok {
			return nil, fmt.Errorf("anchor %q is declared in markup that has no single resolvable start tag", anchor)
		}
		if witness != nil {
			return nil, fmt.Errorf("anchor %q is declared more than once in the pinned bytes", anchor)
		}
		witness = data[start:end]
	}
	if witness == nil {
		return nil, fmt.Errorf("declared anchor %q is absent from the pinned bytes", anchor)
	}
	return witness, nil
}

// documentationTagSpan resolves the start tag containing one declaration
// offset: the tag begins at the nearest preceding "<" that no ">" separates
// from the declaration and whose name begins with an ASCII letter, and ends at
// the first ">" outside a quoted attribute value.
func documentationTagSpan(data []byte, index int) (int, int, bool) {
	start := bytes.LastIndexByte(data[:index], '<')
	if start < 0 || start+1 >= len(data) || !isHTMLNameStart(data[start+1]) {
		return 0, 0, false
	}
	if bytes.IndexByte(data[start:index], '>') >= 0 {
		return 0, 0, false
	}
	for cursor := index + 1; cursor < len(data) && cursor-start <= maxDocumentationTagBytes; cursor++ {
		switch data[cursor] {
		case '"', '\'':
			quote := data[cursor]
			cursor++
			for cursor < len(data) && cursor-start <= maxDocumentationTagBytes && data[cursor] != quote {
				cursor++
			}
			if cursor >= len(data) || cursor-start > maxDocumentationTagBytes {
				return 0, 0, false
			}
		case '>':
			return start, cursor + 1, true
		}
	}
	return 0, 0, false
}

func isHTMLNameStart(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func isHTMLNameByte(character byte) bool {
	return isHTMLNameStart(character) || character >= '0' && character <= '9' || character == '-' || character == '_' || character == ':'
}
