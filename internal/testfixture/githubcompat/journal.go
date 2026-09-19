package githubcompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	// journalFileMode is the mode of the journal this destination creates. The
	// record is private evidence, never world readable.
	journalFileMode = 0o600

	// redactionExcluded is the only replacement this destination reports: the
	// secret-bearing field was not copied into the journal at all. The contract
	// accepts hash, scoped_reference and excluded, and excluded is the honest
	// one for a value that was deliberately never read.
	redactionExcluded = "excluded"
)

// secretBearingMarkers mirrors the direct-read proof runner's own refusal list
// (internal/proof, rejectSecretBearingFields): a field whose normalized name
// contains one of these markers is provider authentication material, so
// recording it as an ordinary field would make the journal unreadable to the
// runner and would copy a secret into evidence.
var secretBearingMarkers = []string{
	"authorization", "token", "secret", "password", "credential",
	"cookie", "apikey", "privatekey", "signature", "clientsecret",
}

// secretNameNormalizer normalizes a received field name the same way the runner
// does before it looks for a secret-bearing marker.
var secretNameNormalizer = strings.NewReplacer("_", "", "-", "")

// jsonPointerEscaper escapes one RFC 6901 reference token.
var jsonPointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// journalEntry is one journal line: the controlled destination's own record of
// one served request. The member set and declaration order are the proof
// runner's decode contract:
//
//	contract.DecodeStrictRequiredObject(line, &entry,
//		"method", "target", "media_type", "status", "response_media_type",
//		"request_fields", "response_fields", "redactions")
//
// Every required member is therefore always present with an explicit value:
// request_fields, response_fields and redactions are always objects and an
// array even when empty, and no member is ever JSON null. request_body_sha256
// is present only for a request body that was read in full.
// response_body_sha256 is always present: the hash of the exact bytes served,
// which is the empty document's hash when a response carries no body.
type journalEntry struct {
	Method             string                     `json:"method"`
	Target             string                     `json:"target"`
	MediaType          string                     `json:"media_type"`
	RequestBodySHA256  *contract.SourceHash       `json:"request_body_sha256,omitempty"`
	Status             int                        `json:"status"`
	ResponseMediaType  string                     `json:"response_media_type"`
	ResponseBodySHA256 *contract.SourceHash       `json:"response_body_sha256,omitempty"`
	RequestFields      map[string]json.RawMessage `json:"request_fields"`
	ResponseFields     map[string]json.RawMessage `json:"response_fields"`
	Redactions         []contract.ProofRedaction  `json:"redactions"`
}

// journal appends one observation line per served request. It is the
// destination's independent record: the proof runner never writes it, counts its
// entries before a case step and requires exactly one new entry afterwards.
type journal struct {
	path string
}

// openJournal validates the declared absolute journal path. The file itself is
// created by the first appended entry, so a destination that has served nothing
// leaves no zero-byte file behind: the runner refuses an empty journal as an
// incomplete input rather than as an empty observation.
func openJournal(path string) (*journal, error) {
	if err := validateJournalPath(path); err != nil {
		return nil, err
	}
	return &journal{path: path}, nil
}

// validateJournalPath refuses a journal path this destination cannot append to:
// a relative or empty path, an existing path that is not a regular file, or a
// path whose parent is not an existing directory.
func validateJournalPath(path string) error {
	if path == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) {
		return fmt.Errorf("%w: journal path must be a declared absolute path", ErrConfiguration)
	}
	clean := filepath.Clean(path)
	information, err := os.Stat(clean)
	switch {
	case err == nil:
		if !information.Mode().IsRegular() {
			return fmt.Errorf("%w: journal path must name a regular file", ErrConfiguration)
		}
	case errors.Is(err, fs.ErrNotExist):
		parent, statErr := os.Stat(filepath.Dir(clean))
		if statErr != nil || !parent.IsDir() {
			return fmt.Errorf("%w: journal directory must be an existing directory", ErrConfiguration)
		}
	default:
		return fmt.Errorf("%w: journal path cannot be inspected", ErrConfiguration)
	}
	return nil
}

// append writes one strict JSON line and nothing else. The destination serves
// one request at a time, so file order is arrival order, and the single append
// never rewrites, batches or reorders an observation already recorded.
func (journal *journal) append(entry journalEntry) error {
	if entry.RequestFields == nil || entry.ResponseFields == nil || entry.Redactions == nil {
		return errors.New("controlled provider observation requires present request, response and redaction fields")
	}
	if err := refuseSecretFields(entry); err != nil {
		return err
	}
	var document bytes.Buffer
	encoder := json.NewEncoder(&document)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(entry); err != nil {
		return fmt.Errorf("encode controlled provider observation: %w", err)
	}
	file, err := os.OpenFile(journal.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, journalFileMode)
	if err != nil {
		return fmt.Errorf("open controlled provider observation journal: %w", err)
	}
	if _, err := file.Write(document.Bytes()); err != nil {
		_ = file.Close()
		return fmt.Errorf("append controlled provider observation: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close controlled provider observation journal: %w", err)
	}
	return nil
}

// refuseSecretFields fails closed when a recorded observation would carry a name
// this destination classifies as provider authentication material, so a journal
// line is never one the runner has to refuse.
func refuseSecretFields(entry journalEntry) error {
	if err := refuseSecretFieldNames("request_fields", entry.RequestFields); err != nil {
		return err
	}
	return refuseSecretFieldNames("response_fields", entry.ResponseFields)
}

// refuseSecretFieldNames checks one recorded field object and the nested field
// object this destination records under it.
func refuseSecretFieldNames(path string, fields map[string]json.RawMessage) error {
	for name, value := range fields {
		if secretBearing(name) {
			return fmt.Errorf("%s records secret-bearing field %q", path, name)
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(value, &nested); err != nil || nested == nil {
			continue
		}
		for nestedName := range nested {
			if secretBearing(nestedName) {
				return fmt.Errorf("%s.%s records secret-bearing field %q", path, name, nestedName)
			}
		}
	}
	return nil
}

// secretBearing reports whether a received name carries provider authentication
// material. The normalized marker match is deliberately conservative: a name is
// redacted even when it only resembles a credential field.
func secretBearing(name string) bool {
	normalized := strings.ToLower(secretNameNormalizer.Replace(name))
	for _, marker := range secretBearingMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// excludedRedaction reports one excluded field, as an RFC 6901 pointer into the
// recorded fields, without recording the value. location is the recorded field
// object the name belongs to, for example headers or query.
func excludedRedaction(location, name string) contract.ProofRedaction {
	return contract.ProofRedaction{
		Pointer:     "/" + location + "/" + jsonPointerEscaper.Replace(name),
		Replacement: redactionExcluded,
	}
}

// rawTextField renders one recorded field as a JSON string carrying the exact
// text this destination received.
func rawTextField(value string) json.RawMessage {
	document, _ := json.Marshal(value)
	return json.RawMessage(document)
}

// hashOf returns the contract hash of one exact byte document.
func hashOf(document []byte) *contract.SourceHash {
	hashed := contract.RawSourceHash(document)
	return &hashed
}
