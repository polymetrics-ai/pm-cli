package contract

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidContract        = errors.New("invalid contract")
	ErrUnsupportedVersion     = errors.New("unsupported contract version")
	ErrDuplicateField         = errors.New("duplicate field")
	ErrUnknownField           = errors.New("unknown field")
	ErrFoundationIncompatible = errors.New("foundation incompatible")
	ErrUnsupportedSchema      = errors.New("unsupported schema")
	ErrInvalidValue           = errors.New("invalid typed value")
	ErrIncompatibleMedia      = errors.New("incompatible media")
	ErrInvalidIdentity        = errors.New("invalid identity")
	ErrInvalidControls        = errors.New("invalid result controls")
	ErrInvalidDispatch        = errors.New("invalid dispatch")
	ErrInvalidSettlement      = errors.New("invalid settlement")
)

// ContractError identifies a safe contract path without including the rejected value.
type ContractError struct {
	Kind   error
	Path   string
	Detail string
}

func (e *ContractError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Path == "" {
		return fmt.Sprintf("%v: %s", e.Kind, e.Detail)
	}
	return fmt.Sprintf("%v at %s: %s", e.Kind, e.Path, e.Detail)
}

func (e *ContractError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

func contractError(kind error, path, detail string) error {
	return &ContractError{Kind: kind, Path: path, Detail: detail}
}
