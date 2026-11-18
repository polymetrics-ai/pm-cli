package acceptance

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// lineFragmentMinimumLength is the shortest line of a multi-line secret file
// that the check compares on its own. A shorter line, for example a PEM
// border, could match ordinary output by chance. The minimum applies only to
// such line fragments: the check keeps every complete secret value and every
// connection password, whatever its length.
const lineFragmentMinimumLength = 8

// secretForm is one form of one secret value. source names the file member
// and the form. It holds no secret value, so a message can name it.
type secretForm struct {
	source string
	value  []byte
}

// readDeploymentSecrets reads each regular file of the deployment secrets
// directory once and returns the forms of its secret value.
func readDeploymentSecrets(root string) ([]secretForm, error) {
	var forms []secretForm
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return walkErr
		}
		contents, err := readBoundedFile(path, laneFileLimit)
		if err != nil {
			return err
		}
		forms = append(forms, secretForms(entry.Name(), contents)...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the deployment secrets directory: %w", err)
	}
	if len(forms) == 0 {
		return nil, errors.New("the deployment secrets directory holds no secret value")
	}
	return forms, nil
}

// secretForms returns the forms of one secret file that output could hold:
//   - the raw bytes, not trimmed, and their base64 and hexadecimal forms, as
//     a binary secret such as the wrapping key appears on the wire;
//   - for text, the trimmed text and its encoded forms, each line of the text
//     of at least lineFragmentMinimumLength bytes, and the password of a
//     connection string with its escaped forms.
//
// Every non-empty complete value and every connection password is kept,
// whatever its length.
func secretForms(member string, contents []byte) []secretForm {
	var forms []secretForm
	add := func(form string, value []byte) {
		if len(value) > 0 {
			forms = append(forms, secretForm{source: member + " (" + form + ")", value: value})
		}
	}
	addEncoded := func(form string, value []byte) {
		add(form, value)
		add(form+", base64", []byte(base64.StdEncoding.EncodeToString(value)))
		add(form+", unpadded base64", []byte(base64.RawStdEncoding.EncodeToString(value)))
		add(form+", URL base64", []byte(base64.URLEncoding.EncodeToString(value)))
		add(form+", unpadded URL base64", []byte(base64.RawURLEncoding.EncodeToString(value)))
		add(form+", hexadecimal", []byte(hex.EncodeToString(value)))
		add(form+", upper-case hexadecimal", []byte(strings.ToUpper(hex.EncodeToString(value))))
	}
	addEncoded("raw bytes", contents)
	if !utf8.Valid(contents) {
		return forms
	}
	text := strings.TrimSpace(string(contents))
	addEncoded("trimmed text", []byte(text))
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); len(line) >= lineFragmentMinimumLength {
			add("line", []byte(line))
		}
	}
	if parsed, err := url.Parse(text); err == nil && parsed.User != nil {
		if password, ok := parsed.User.Password(); ok {
			add("connection password", []byte(password))
			add("connection password, query escaped", []byte(url.QueryEscape(password)))
			add("connection password, path escaped", []byte(url.PathEscape(password)))
		}
	}
	return forms
}

// findSecret reports the source of the first secret form that output holds.
// It compares the raw output and each JSON string token of output, after
// JSON decoding, so a secret that output writes with JSON escapes also
// counts. complete is true only when output is a sequence of complete JSON
// values, so that the check inspected every string. A caller must not treat
// output as clean when complete is false.
func findSecret(output []byte, forms []secretForm) (source string, found, complete bool) {
	tokens, complete := jsonStringTokens(output)
	texts := [][]byte{output}
	for _, value := range tokens {
		texts = append(texts, []byte(value))
	}
	for _, text := range texts {
		for _, form := range forms {
			if bytes.Contains(text, form.value) {
				return form.source, true, complete
			}
		}
	}
	return "", false, complete
}

// jsonStringTokens reads output once as a sequence of JSON values, token by
// token, and returns every string token: each object key and each string
// value. A token scan keeps each member of an object, also a duplicate member
// that a decoded map would lose. complete is false when output holds text
// that is not JSON, or a JSON value that does not end. The scan counts the
// open objects and arrays, because the decoder reports end of file without
// an error when output stops between tokens inside an open value.
func jsonStringTokens(output []byte) ([]string, bool) {
	var values []string
	depth := 0
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return values, depth == 0
		}
		if err != nil {
			return values, false
		}
		switch value := token.(type) {
		case string:
			values = append(values, value)
		case json.Delim:
			switch value {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
}
