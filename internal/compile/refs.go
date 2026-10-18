package compile

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	maxYAMLNodes         = 200_000
	maxYAMLAliases       = 1_024
	maxYAMLExpandedNodes = 400_000
)

var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

type ReferenceIdentity struct {
	SourceID       string `json:"source_id"`
	FromPointer    string `json:"from_pointer"`
	Reference      string `json:"reference"`
	TargetSourceID string `json:"target_source_id"`
	TargetPointer  string `json:"target_pointer"`
	Recursive      bool   `json:"recursive"`
}

type ResolvedDocument struct {
	Metadata      SourceDocument
	CanonicalJSON []byte
	RewrittenJSON []byte
	Root          bool
	References    []ReferenceIdentity
}

type ResolvedSource struct {
	Captured  *CapturedSource
	Documents []ResolvedDocument
	byID      map[string]*parsedDocument
	refs      map[referenceKey]ReferenceIdentity
}

type parsedDocument struct {
	metadata  SourceDocument
	root      any
	canonical []byte
	nodes     map[string]*yaml.Node
	refs      []rawReference
	// documentation retains the exact bytes of a supplemental
	// official-documentation capture. Such a document is never parsed: it has no
	// YAML tree, no nodes and no $ref scan, and its only addressing is an exact
	// byte range.
	documentation bool
	bytes         []byte
}

type rawReference struct {
	fromPointer string
	reference   string
}

type referenceKey struct {
	sourceID    string
	fromPointer string
	reference   string
}

func ResolveRefs(source *CapturedSource) (*ResolvedSource, error) {
	if source == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "captured source is required")
	}
	resolved := &ResolvedSource{
		Captured: source,
		byID:     make(map[string]*parsedDocument, len(source.Documents)),
		refs:     make(map[referenceKey]ReferenceIdentity),
	}
	for _, document := range source.Documents {
		parsed, err := parseCapturedDocument(document)
		if err != nil {
			return nil, err
		}
		resolved.byID[document.Metadata.SourceID] = parsed
	}

	declared := make(map[referenceKey]SourceReference)
	for _, document := range source.Documents {
		for _, reference := range document.Metadata.References {
			key := referenceKey{
				sourceID:    document.Metadata.SourceID,
				fromPointer: reference.FromPointer,
				reference:   reference.Reference,
			}
			declared[key] = reference
		}
	}
	usedDeclarations := make(map[referenceKey]struct{}, len(declared))
	for sourceID, document := range resolved.byID {
		for _, reference := range document.refs {
			key := referenceKey{sourceID: sourceID, fromPointer: reference.fromPointer, reference: reference.reference}
			identity := ReferenceIdentity{
				SourceID:    sourceID,
				FromPointer: reference.fromPointer,
				Reference:   reference.reference,
			}
			if strings.HasPrefix(reference.reference, "#") {
				identity.TargetSourceID = sourceID
				identity.TargetPointer = strings.TrimPrefix(reference.reference, "#")
				if entry, ok := declared[key]; ok {
					if entry.TargetSourceID != identity.TargetSourceID || entry.TargetPointer != identity.TargetPointer {
						return nil, sourceError(ErrSourceFidelity, sourceID, reference.fromPointer, "declared local reference target differs from the source $ref")
					}
					usedDeclarations[key] = struct{}{}
				}
			} else {
				entry, ok := declared[key]
				if !ok {
					return nil, sourceError(ErrUnsafeReference, sourceID, reference.fromPointer, "external reference is absent from the explicit capture map")
				}
				usedDeclarations[key] = struct{}{}
				identity.TargetSourceID = entry.TargetSourceID
				identity.TargetPointer = entry.TargetPointer
			}
			target := resolved.byID[identity.TargetSourceID]
			if target == nil {
				return nil, sourceError(ErrUnsafeReference, sourceID, reference.fromPointer, "captured reference target is missing")
			}
			if _, ok := target.nodes[identity.TargetPointer]; !ok {
				return nil, sourceError(ErrUnsafeReference, sourceID, reference.fromPointer, "captured reference target pointer does not exist")
			}
			identity.Recursive = identity.TargetSourceID == sourceID && pointerContains(reference.fromPointer, identity.TargetPointer)
			resolved.refs[key] = identity
		}
	}
	for key := range declared {
		if _, used := usedDeclarations[key]; !used {
			return nil, sourceError(ErrSourceFidelity, key.sourceID, key.fromPointer, "declared captured reference does not match an actual source $ref")
		}
	}

	ids := make([]string, 0, len(resolved.byID))
	for sourceID := range resolved.byID {
		ids = append(ids, sourceID)
	}
	slices.Sort(ids)
	resolved.Documents = make([]ResolvedDocument, 0, len(ids))
	for _, sourceID := range ids {
		document := resolved.byID[sourceID]
		if document.documentation {
			// Nothing in a documentation capture is rewritten: it has no
			// references, so its exact bytes are both its canonical and its
			// rewritten form.
			resolved.Documents = append(resolved.Documents, ResolvedDocument{
				Metadata:      document.metadata,
				CanonicalJSON: bytes.Clone(document.canonical),
				RewrittenJSON: bytes.Clone(document.canonical),
				Root:          false,
				References:    []ReferenceIdentity{},
			})
			continue
		}
		rewritten, err := rewriteExternalReferences(document.root, sourceID, "", resolved.refs)
		if err != nil {
			return nil, err
		}
		rewrittenJSON, err := json.Marshal(rewritten)
		if err != nil {
			return nil, sourceError(ErrInvalidSource, sourceID, "", "canonical captured source cannot be encoded")
		}
		identities := make([]ReferenceIdentity, 0, len(document.refs))
		for _, reference := range document.refs {
			identity := resolved.refs[referenceKey{sourceID: sourceID, fromPointer: reference.fromPointer, reference: reference.reference}]
			identities = append(identities, identity)
		}
		slices.SortFunc(identities, compareReferenceIdentity)
		resolved.Documents = append(resolved.Documents, ResolvedDocument{
			Metadata:      document.metadata,
			CanonicalJSON: bytes.Clone(document.canonical),
			RewrittenJSON: rewrittenJSON,
			Root:          sourceRootDialect(document.root) != "",
			References:    identities,
		})
	}
	return resolved, nil
}

func parseCapturedDocument(document CapturedDocument) (*parsedDocument, error) {
	if document.Metadata.Dialect == DialectOfficialDocumentation {
		// A documentation capture is retained byte-for-byte and is never parsed
		// as YAML or JSON: an admitted document must not be able to change
		// meaning through a parser, and documentation bytes are not structured
		// data to begin with.
		return &parsedDocument{
			metadata:      document.Metadata,
			canonical:     bytes.Clone(document.Bytes),
			documentation: true,
			bytes:         bytes.Clone(document.Bytes),
		}, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(document.Bytes))
	var tree yaml.Node
	if err := decoder.Decode(&tree); err != nil {
		return nil, sourceError(ErrInvalidSource, document.Metadata.SourceID, "", "source is not valid bounded YAML or JSON")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, sourceError(ErrInvalidSource, document.Metadata.SourceID, "", "multiple YAML documents are unsupported")
		}
		return nil, sourceError(ErrInvalidSource, document.Metadata.SourceID, "", "source contains invalid trailing YAML")
	}
	if len(tree.Content) != 1 {
		return nil, sourceError(ErrInvalidSource, document.Metadata.SourceID, "", "source must contain exactly one root value")
	}
	budget := yamlBudget{}
	if err := inspectYAMLNode(tree.Content[0], document.Metadata.SourceID, "", &budget, make(map[*yaml.Node]bool)); err != nil {
		return nil, err
	}
	root, err := yamlNodeValue(tree.Content[0], document.Metadata.SourceID, "", 0)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(root)
	if err != nil {
		return nil, sourceError(ErrInvalidSource, document.Metadata.SourceID, "", "source cannot be represented as exact JSON data")
	}
	parsed := &parsedDocument{
		metadata:  document.Metadata,
		root:      root,
		canonical: canonical,
		nodes:     make(map[string]*yaml.Node),
	}
	indexYAMLNodes(tree.Content[0], "", parsed.nodes, &parsed.refs)
	return parsed, nil
}

type yamlBudget struct {
	nodes         int
	aliases       int
	expandedNodes int
}

func inspectYAMLNode(node *yaml.Node, sourceID, pointer string, budget *yamlBudget, expansionStack map[*yaml.Node]bool) error {
	if node == nil {
		return sourceError(ErrInvalidSource, sourceID, pointer, "YAML node is missing")
	}
	budget.nodes++
	budget.expandedNodes++
	if budget.nodes > maxYAMLNodes || budget.expandedNodes > maxYAMLExpandedNodes {
		return sourceError(ErrInvalidSource, sourceID, pointer, "YAML node or alias expansion budget exceeded")
	}
	if node.Kind == yaml.AliasNode {
		budget.aliases++
		if budget.aliases > maxYAMLAliases {
			return sourceError(ErrInvalidSource, sourceID, pointer, "YAML alias budget exceeded")
		}
		if node.Alias == nil || expansionStack[node.Alias] {
			return sourceError(ErrInvalidSource, sourceID, pointer, "cyclic or missing YAML alias is unsupported")
		}
		expansionStack[node.Alias] = true
		err := inspectYAMLNode(node.Alias, sourceID, pointer, budget, expansionStack)
		delete(expansionStack, node.Alias)
		return err
	}
	if err := validateYAMLTag(node, sourceID, pointer); err != nil {
		return err
	}
	switch node.Kind {
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return sourceError(ErrInvalidSource, sourceID, pointer, "YAML mapping has an unmatched key")
		}
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return sourceError(ErrInvalidSource, sourceID, pointer, "YAML mapping keys must be strings")
			}
			if key.Value == "<<" || key.Tag == "!!merge" {
				return sourceError(ErrInvalidSource, sourceID, joinPointer(pointer, key.Value), "YAML merge keys are unsupported")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return sourceError(ErrInvalidSource, sourceID, joinPointer(pointer, key.Value), "duplicate YAML key")
			}
			seen[key.Value] = struct{}{}
			if err := inspectYAMLNode(node.Content[index+1], sourceID, joinPointer(pointer, key.Value), budget, expansionStack); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := inspectYAMLNode(child, sourceID, joinPointer(pointer, strconv.Itoa(index)), budget, expansionStack); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := inspectYAMLNode(child, sourceID, pointer, budget, expansionStack); err != nil {
				return err
			}
		}
	default:
		return sourceError(ErrInvalidSource, sourceID, pointer, "YAML node kind is unsupported")
	}
	return nil
}

func validateYAMLTag(node *yaml.Node, sourceID, pointer string) error {
	allowed := false
	switch node.Kind {
	case yaml.MappingNode:
		allowed = node.Tag == "!!map" || node.Tag == ""
	case yaml.SequenceNode:
		allowed = node.Tag == "!!seq" || node.Tag == ""
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str", "!!bool", "!!int", "!!float", "!!null":
			allowed = true
		}
	case yaml.DocumentNode, yaml.AliasNode:
		allowed = node.Tag == ""
	}
	if !allowed {
		return sourceError(ErrInvalidSource, sourceID, pointer, "unsupported YAML tag")
	}
	return nil
}

func yamlNodeValue(node *yaml.Node, sourceID, pointer string, depth int) (any, error) {
	if depth > maxYAMLNodes {
		return nil, sourceError(ErrInvalidSource, sourceID, pointer, "YAML expansion depth exceeded")
	}
	if node.Kind == yaml.AliasNode {
		return yamlNodeValue(node.Alias, sourceID, pointer, depth+1)
	}
	switch node.Kind {
	case yaml.MappingNode:
		value := make(map[string]any, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index].Value
			child, err := yamlNodeValue(node.Content[index+1], sourceID, joinPointer(pointer, key), depth+1)
			if err != nil {
				return nil, err
			}
			value[key] = child
		}
		return value, nil
	case yaml.SequenceNode:
		value := make([]any, len(node.Content))
		for index, child := range node.Content {
			converted, err := yamlNodeValue(child, sourceID, joinPointer(pointer, strconv.Itoa(index)), depth+1)
			if err != nil {
				return nil, err
			}
			value[index] = converted
		}
		return value, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str":
			return node.Value, nil
		case "!!bool":
			if node.Value == "true" {
				return true, nil
			}
			if node.Value == "false" {
				return false, nil
			}
			return nil, sourceError(ErrInvalidSource, sourceID, pointer, "boolean must use canonical true or false")
		case "!!null":
			return nil, nil
		case "!!int", "!!float":
			if !jsonNumberPattern.MatchString(node.Value) {
				return nil, sourceError(ErrInvalidSource, sourceID, pointer, "numeric scalar must use JSON number syntax")
			}
			return json.Number(node.Value), nil
		default:
			return nil, sourceError(ErrInvalidSource, sourceID, pointer, "scalar tag is unsupported")
		}
	default:
		return nil, sourceError(ErrInvalidSource, sourceID, pointer, "YAML node kind is unsupported")
	}
}

func indexYAMLNodes(node *yaml.Node, pointer string, nodes map[string]*yaml.Node, references *[]rawReference) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	nodes[pointer] = node
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index].Value
			childPointer := joinPointer(pointer, key)
			child := node.Content[index+1]
			indexYAMLNodes(child, childPointer, nodes, references)
			if key == "$ref" && child.Kind == yaml.ScalarNode && child.Tag == "!!str" {
				*references = append(*references, rawReference{fromPointer: childPointer, reference: child.Value})
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			indexYAMLNodes(child, joinPointer(pointer, strconv.Itoa(index)), nodes, references)
		}
	}
}

func rewriteExternalReferences(value any, sourceID, pointer string, references map[referenceKey]ReferenceIdentity) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			childPointer := joinPointer(pointer, key)
			if key == "$ref" {
				reference, ok := child.(string)
				if !ok {
					return nil, sourceError(ErrInvalidSource, sourceID, childPointer, "$ref must be a string")
				}
				if strings.HasPrefix(reference, "#") {
					result[key] = reference
					continue
				}
				identity, ok := references[referenceKey{sourceID: sourceID, fromPointer: childPointer, reference: reference}]
				if !ok {
					return nil, sourceError(ErrUnsafeReference, sourceID, childPointer, "external reference has no closed capture mapping")
				}
				result[key] = "pmcapture://source/" + url.PathEscape(identity.TargetSourceID) + "#" + identity.TargetPointer
				continue
			}
			rewritten, err := rewriteExternalReferences(child, sourceID, childPointer, references)
			if err != nil {
				return nil, err
			}
			result[key] = rewritten
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			rewritten, err := rewriteExternalReferences(child, sourceID, joinPointer(pointer, strconv.Itoa(index)), references)
			if err != nil {
				return nil, err
			}
			result[index] = rewritten
		}
		return result, nil
	default:
		return value, nil
	}
}

func (source *ResolvedSource) Document(sourceID string) (ResolvedDocument, bool) {
	if source == nil {
		return ResolvedDocument{}, false
	}
	for _, document := range source.Documents {
		if document.Metadata.SourceID == sourceID {
			document.CanonicalJSON = bytes.Clone(document.CanonicalJSON)
			document.RewrittenJSON = bytes.Clone(document.RewrittenJSON)
			document.References = slices.Clone(document.References)
			return document, true
		}
	}
	return ResolvedDocument{}, false
}

func (source *ResolvedSource) Reference(sourceID, fromPointer, reference string) (ReferenceIdentity, bool) {
	if source == nil {
		return ReferenceIdentity{}, false
	}
	identity, ok := source.refs[referenceKey{sourceID: sourceID, fromPointer: fromPointer, reference: reference}]
	return identity, ok
}

func (source *ResolvedSource) ReferenceAt(sourceID, fromPointer string) (ReferenceIdentity, bool) {
	if source == nil {
		return ReferenceIdentity{}, false
	}
	for key, identity := range source.refs {
		if key.sourceID == sourceID && key.fromPointer == fromPointer {
			return identity, true
		}
	}
	return ReferenceIdentity{}, false
}

// DocumentationPointerPrefix introduces an exact byte range of a
// documentation capture. The form is deliberately not a JSON pointer: a
// documentation fact can never silently resolve against OpenAPI data, and an
// OpenAPI fact can never silently resolve against documentation bytes. The
// contract owns the grammar of the form.
const DocumentationPointerPrefix = "bytes:"

// Documentation reports whether sourceID names an admitted supplemental
// official-documentation capture.
func (source *ResolvedSource) Documentation(sourceID string) bool {
	if source == nil {
		return false
	}
	document := source.byID[sourceID]
	return document != nil && document.documentation
}

// cite resolves one byte-range pointer against the retained documentation bytes.
func (document *parsedDocument) cite(pointer string) ([]byte, bool) {
	byteRange, ok := contract.ParseSourceByteRange(pointer)
	if !ok || byteRange.End > len(document.bytes) {
		return nil, false
	}
	cited := document.bytes[byteRange.Start:byteRange.End]
	if !utf8.Valid(cited) {
		return nil, false
	}
	return cited, true
}

func (source *ResolvedSource) RawValue(sourceID, pointer string) (any, bool) {
	if source == nil {
		return nil, false
	}
	document := source.byID[sourceID]
	if document == nil {
		return nil, false
	}
	if document.documentation {
		cited, ok := document.cite(strings.TrimPrefix(pointer, "#"))
		if !ok {
			return nil, false
		}
		return string(cited), true
	}
	node, ok := document.nodes[strings.TrimPrefix(pointer, "#")]
	if !ok {
		return nil, false
	}
	value, err := yamlNodeValue(node, sourceID, strings.TrimPrefix(pointer, "#"), 0)
	if err != nil {
		return nil, false
	}
	return value, true
}

// PointerHash returns the exact digest of the value a source pointer addresses.
// A JSON pointer hashes the canonical JSON of the addressed OpenAPI node; a
// documentation byte-range pointer hashes the exact cited bytes, so a consumer
// can recompute either digest from the retained capture alone.
func (source *ResolvedSource) PointerHash(sourceID, pointer string) (string, bool) {
	if source == nil {
		return "", false
	}
	document := source.byID[sourceID]
	if document == nil {
		return "", false
	}
	if document.documentation {
		cited, ok := document.cite(strings.TrimPrefix(pointer, "#"))
		if !ok {
			return "", false
		}
		return "sha256:" + digestHex(cited), true
	}
	node, ok := document.nodes[strings.TrimPrefix(pointer, "#")]
	if !ok {
		return "", false
	}
	value, err := yamlNodeValue(node, sourceID, strings.TrimPrefix(pointer, "#"), 0)
	if err != nil {
		return "", false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return "sha256:" + digestHex(encoded), true
}

func sourceRootDialect(value any) string {
	root, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if version, ok := root["openapi"].(string); ok {
		return version
	}
	if version, ok := root["swagger"].(string); ok {
		return version
	}
	return ""
}

func compareReferenceIdentity(left, right ReferenceIdentity) int {
	if compared := strings.Compare(left.SourceID, right.SourceID); compared != 0 {
		return compared
	}
	if compared := strings.Compare(left.FromPointer, right.FromPointer); compared != 0 {
		return compared
	}
	if compared := strings.Compare(left.Reference, right.Reference); compared != 0 {
		return compared
	}
	if compared := strings.Compare(left.TargetSourceID, right.TargetSourceID); compared != 0 {
		return compared
	}
	return strings.Compare(left.TargetPointer, right.TargetPointer)
}

func pointerContains(fromPointer, targetPointer string) bool {
	if targetPointer == "" {
		return true
	}
	return fromPointer == targetPointer || strings.HasPrefix(fromPointer, targetPointer+"/")
}

func joinPointer(pointer, token string) string {
	return pointer + "/" + escapePointerToken(token)
}

func escapePointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
