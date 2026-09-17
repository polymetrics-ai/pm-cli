package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

func renderWireResponse(writer io.Writer, output pmapi.OutputFormat, response pmapi.WireResponse) error {
	envelope, err := pmapi.DecodeEnvelope(response)
	if err != nil {
		return err
	}
	if err := pmapi.EnvelopeError(envelope); err != nil {
		return err
	}
	if len(envelope.Body) == 0 {
		_, err = fmt.Fprintln(writer, "ok")
		return err
	}
	return renderDocument(writer, output, envelope.Body)
}

func renderDocument(writer io.Writer, output pmapi.OutputFormat, document []byte) error {
	if writer == nil {
		return errors.New("output writer is unavailable")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return pmapi.ErrMalformedEnvelope
	}
	if output == pmapi.OutputJSON {
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
		_, err = writer.Write(encoded)
		return err
	}
	return renderTextValue(writer, value)
}

func renderTextValue(writer io.Writer, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			encoded, err := json.Marshal(typed[key])
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\n", key, encoded); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, element := range typed {
			encoded, err := json.Marshal(element)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(writer, string(encoded)); err != nil {
				return err
			}
		}
		return nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(writer, string(encoded))
		return err
	}
}

// Render writes one already typed, secret-safe value using the selected CLI
// presentation format. Transport problems must be decoded before this call.
func Render(writer io.Writer, output pmapi.OutputFormat, value any) error {
	return renderSafeValue(writer, output, value)
}

func renderSafeValue(writer io.Writer, output pmapi.OutputFormat, value any) error {
	document, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return renderDocument(writer, output, document)
}

func renderProfiles(writer io.Writer, profiles []pmapi.ServerProfile) error {
	for _, profile := range profiles {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", profile.Name, profile.ServerURL, profile.InstanceID); err != nil {
			return err
		}
	}
	return nil
}
