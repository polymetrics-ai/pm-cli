package contract

import (
	"encoding/json"
	"unicode/utf8"
)

type FieldLocation string

const (
	FieldLocationPath     FieldLocation = "path"
	FieldLocationQuery    FieldLocation = "query"
	FieldLocationHeader   FieldLocation = "header"
	FieldLocationCookie   FieldLocation = "cookie"
	FieldLocationBody     FieldLocation = "body"
	FieldLocationRecord   FieldLocation = "record"
	FieldLocationMetadata FieldLocation = "metadata"
)

type Sensitivity string

const (
	SensitivityPublic Sensitivity = "public"
	SensitivitySecret Sensitivity = "secret"
)

type FieldBinding struct {
	FieldID       string        `json:"field_id"`
	WireName      string        `json:"wire_name"`
	Location      FieldLocation `json:"location"`
	JSONPointer   string        `json:"json_pointer"`
	Required      bool          `json:"required"`
	Nullable      bool          `json:"nullable"`
	Style         string        `json:"style"`
	Explode       bool          `json:"explode"`
	AllowReserved bool          `json:"allow_reserved"`
	Sensitivity   Sensitivity   `json:"sensitivity"`
	Schema        Schema        `json:"schema"`
	SourceDefault *PresentValue `json:"source_default,omitempty"`
}

func DecodeFieldBinding(document []byte) (FieldBinding, error) {
	var binding FieldBinding
	if err := decodeContractJSON(document, &binding); err != nil {
		return FieldBinding{}, err
	}
	if err := binding.Validate(); err != nil {
		return FieldBinding{}, err
	}
	return binding, nil
}

func (b *FieldBinding) UnmarshalJSON(data []byte) error {
	type wire struct {
		FieldID       string          `json:"field_id"`
		WireName      *string         `json:"wire_name"`
		Location      FieldLocation   `json:"location"`
		JSONPointer   *string         `json:"json_pointer"`
		Required      *bool           `json:"required"`
		Nullable      *bool           `json:"nullable"`
		Style         string          `json:"style"`
		Explode       *bool           `json:"explode"`
		AllowReserved *bool           `json:"allow_reserved"`
		Sensitivity   Sensitivity     `json:"sensitivity"`
		Schema        Schema          `json:"schema"`
		SourceDefault json.RawMessage `json:"source_default,omitempty"`
	}
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	if decoded.WireName == nil || decoded.JSONPointer == nil ||
		decoded.Required == nil || decoded.Nullable == nil || decoded.Explode == nil || decoded.AllowReserved == nil {
		return contractError(ErrInvalidContract, "field_binding", "wire_name, json_pointer, required, nullable, explode, and allow_reserved must be explicit")
	}
	var sourceDefault *PresentValue
	if decoded.SourceDefault != nil {
		var value PresentValue
		if err := decodeStrictJSON(decoded.SourceDefault, &value); err != nil {
			return err
		}
		sourceDefault = new(value)
	}
	*b = FieldBinding{
		FieldID:       decoded.FieldID,
		WireName:      *decoded.WireName,
		Location:      decoded.Location,
		JSONPointer:   *decoded.JSONPointer,
		Required:      *decoded.Required,
		Nullable:      *decoded.Nullable,
		Style:         decoded.Style,
		Explode:       *decoded.Explode,
		AllowReserved: *decoded.AllowReserved,
		Sensitivity:   decoded.Sensitivity,
		Schema:        decoded.Schema,
		SourceDefault: sourceDefault,
	}
	return b.Validate()
}

func (b FieldBinding) Validate() error {
	if !validIdentityPart(b.FieldID) {
		return contractError(ErrInvalidContract, "field_binding.field_id", "must be a nonempty valid UTF-8 stable ID")
	}
	if !utf8.ValidString(b.WireName) {
		return contractError(ErrInvalidContract, "field_binding.wire_name", "must preserve the exact valid UTF-8 provider wire name")
	}
	if !validFieldLocation(b.Location) {
		return contractError(ErrInvalidContract, "field_binding.location", "location is unsupported")
	}
	if b.WireName == "" && b.Location != FieldLocationBody && b.Location != FieldLocationRecord && b.Location != FieldLocationMetadata {
		return contractError(ErrInvalidContract, "field_binding.wire_name", "may be empty only for an exact JSON property name")
	}
	if !validJSONPointer(b.JSONPointer) {
		return contractError(ErrInvalidContract, "field_binding.json_pointer", "must be an absolute RFC 6901 JSON pointer")
	}
	if !validStyleForLocation(b.Style, b.Location) {
		return contractError(ErrInvalidContract, "field_binding.style", "style is unsupported for the declared location")
	}
	if b.AllowReserved && b.Location != FieldLocationQuery {
		return contractError(ErrInvalidContract, "field_binding.allow_reserved", "is valid only for query fields")
	}
	if b.Location == FieldLocationPath && (!b.Required || b.Nullable) {
		return contractError(ErrInvalidContract, "field_binding", "path fields must be required and non-null")
	}
	if b.Sensitivity != SensitivityPublic && b.Sensitivity != SensitivitySecret {
		return contractError(ErrInvalidContract, "field_binding.sensitivity", "must be public or secret")
	}
	if err := b.Schema.Validate(); err != nil {
		return err
	}
	if b.Nullable != b.Schema.Nullable {
		return contractError(ErrInvalidContract, "field_binding.nullable", "must match the declared schema nullability")
	}
	if b.SourceDefault != nil {
		if b.Sensitivity == SensitivitySecret {
			return contractError(ErrInvalidContract, "field_binding.source_default", "secret bindings cannot publish a default value")
		}
		if b.SourceDefault.Presence() == PresenceAbsent {
			return contractError(ErrInvalidContract, "field_binding.source_default", "an absent value is not a declared source default")
		}
		if err := b.SourceDefault.Validate(); err != nil {
			return err
		}
		if !valueMatchesSchema(*b.SourceDefault, b.Schema) {
			return contractError(ErrInvalidContract, "field_binding.source_default", "does not match the declared typed schema")
		}
	}
	return nil
}

func validJSONPointer(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	if value == "" {
		return true
	}
	if value[0] != '/' {
		return false
	}
	for index, character := range value {
		if character != '~' {
			continue
		}
		if index+1 >= len(value) || (value[index+1] != '0' && value[index+1] != '1') {
			return false
		}
	}
	return true
}

func validFieldLocation(location FieldLocation) bool {
	switch location {
	case FieldLocationPath, FieldLocationQuery, FieldLocationHeader, FieldLocationCookie,
		FieldLocationBody, FieldLocationRecord, FieldLocationMetadata:
		return true
	default:
		return false
	}
}

func validStyleForLocation(style string, location FieldLocation) bool {
	switch location {
	case FieldLocationPath:
		return style == "simple" || style == "label" || style == "matrix"
	case FieldLocationQuery:
		return style == "form" || style == "spaceDelimited" || style == "pipeDelimited" || style == "deepObject"
	case FieldLocationHeader:
		return style == "simple"
	case FieldLocationCookie:
		return style == "form"
	case FieldLocationBody, FieldLocationRecord, FieldLocationMetadata:
		return style == "json" || style == "binary"
	default:
		return false
	}
}
