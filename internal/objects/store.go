// Package objects owns shared immutable object bytes. A verified object is not
// published until its scoped PostgreSQL registration commits.
package objects

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"mime"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrInvalidObject     = errors.New("invalid shared object request")
	ErrObjectLimit       = errors.New("shared object exceeds its finite byte bound")
	ErrAttemptUsed       = errors.New("shared object upload attempt is already consumed")
	ErrObjectUnavailable = errors.New("shared object backend is unavailable")
	ErrObjectIntegrity   = errors.New("shared object verification failed")
	ErrObjectConflict    = errors.New("shared object attempt key already exists")
	ErrInvalidRange      = errors.New("shared object range is invalid")
)

// Bounds are resolved by the caller's typed server policy. Backend-local
// concurrency does not replace the caller's durable scoped capacity reservation.
type Bounds struct {
	MaxObjectBytes int64
	MaxInFlight    int
	RequestTimeout time.Duration
}

func (bounds Bounds) validate() error {
	if bounds.MaxObjectBytes <= 0 || bounds.MaxObjectBytes == math.MaxInt64 ||
		bounds.MaxInFlight <= 0 || int64(bounds.MaxInFlight) > math.MaxInt64/bounds.MaxObjectBytes ||
		bounds.RequestTimeout <= 0 {
		return ErrInvalidObject
	}
	return nil
}

// UploadDescriptor is the immutable identity that must be durably reserved
// before attempting object I/O. It is neither authorization nor publication.
type UploadDescriptor struct {
	Scope            contract.Scope
	ArtifactID       string
	ObjectAttemptKey string
	SHA256           string
	ByteLength       int64
	MediaType        string
}

// Attempt shares its one-use state even when copied. A retry requires a new
// attempt and a new immutable key; an uncertain PUT is never silently repeated.
type Attempt struct {
	state *attemptState
}

type attemptState struct {
	descriptor UploadDescriptor
	payload    []byte
	consumed   atomic.Bool
}

// NewAttempt transfers ownership of payload on success. The caller must not
// subsequently read or modify that slice. Close releases an unused attempt;
// Upload releases the payload after consuming the attempt.
func NewAttempt(scope contract.Scope, artifactID, mediaType string, payload []byte, maximumBytes int64) (Attempt, error) {
	if err := scope.Validate(); err != nil {
		return Attempt{}, ErrInvalidObject
	}
	if !validArtifactID(artifactID) || mediaType == "" || len(mediaType) > 512 || !utf8.ValidString(mediaType) {
		return Attempt{}, ErrInvalidObject
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return Attempt{}, ErrInvalidObject
	}
	if maximumBytes <= 0 || maximumBytes == math.MaxInt64 {
		return Attempt{}, ErrInvalidObject
	}
	if int64(len(payload)) > maximumBytes {
		return Attempt{}, ErrObjectLimit
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Attempt{}, ErrObjectUnavailable
	}
	digest := sha256.Sum256(payload)
	return Attempt{state: &attemptState{
		descriptor: UploadDescriptor{
			Scope:            scope,
			ArtifactID:       artifactID,
			ObjectAttemptKey: objectKeyPrefix(scope, artifactID) + hex.EncodeToString(nonce[:]),
			SHA256:           hex.EncodeToString(digest[:]),
			ByteLength:       int64(len(payload)),
			MediaType:        mediaType,
		},
		payload: payload,
	}}, nil
}

func (attempt Attempt) Descriptor() (UploadDescriptor, error) {
	if attempt.state == nil {
		return UploadDescriptor{}, ErrInvalidObject
	}
	return attempt.state.descriptor, nil
}

// Close cannot clear a payload already owned by an in-flight Upload.
func (attempt Attempt) Close() error {
	if attempt.state != nil && attempt.state.consumed.CompareAndSwap(false, true) {
		clear(attempt.state.payload)
		attempt.state.payload = nil
	}
	return nil
}

func (Attempt) String() string           { return "shared object upload attempt [REDACTED]" }
func (attempt Attempt) GoString() string { return attempt.String() }

// VerifiedObject can only be produced after the object owner has independently
// read and verified the actual uploaded bytes and their backend generation.
// Its descriptor still requires scoped PostgreSQL registration before delivery.
type VerifiedObject struct {
	ref contract.ObjectRef
}

func (object VerifiedObject) Reference() (contract.ObjectRef, error) {
	if err := object.ref.Validate(); err != nil {
		return contract.ObjectRef{}, ErrObjectIntegrity
	}
	return object.ref, nil
}

// ByteRange is a resolved half-open interval within a registered object. HTTP
// range syntax and authorization remain with the consuming API boundary.
type ByteRange struct {
	Offset int64
	Length int64
}

func (selection ByteRange) validate(byteLength int64) error {
	if selection.Offset < 0 || selection.Length < 0 || selection.Offset > byteLength ||
		selection.Length > byteLength-selection.Offset {
		return ErrInvalidRange
	}
	return nil
}

func validArtifactID(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func objectKeyPrefix(scope contract.Scope, artifactID string) string {
	organization := sha256.Sum256([]byte(scope.OrgID))
	workspace := sha256.Sum256([]byte(scope.WorkspaceID))
	artifact := sha256.Sum256([]byte(artifactID))
	return "pm/v1/org/" + hex.EncodeToString(organization[:]) +
		"/workspace/" + hex.EncodeToString(workspace[:]) +
		"/artifact/" + hex.EncodeToString(artifact[:]) + "/attempt/"
}

func validateObjectKey(ref contract.ObjectRef) error {
	prefix := objectKeyPrefix(contract.Scope{OrgID: ref.OrgID, WorkspaceID: ref.WorkspaceID}, ref.ArtifactID)
	if !strings.HasPrefix(ref.ObjectAttemptKey, prefix) {
		return ErrObjectIntegrity
	}
	nonce := strings.TrimPrefix(ref.ObjectAttemptKey, prefix)
	if len(nonce) != 64 {
		return ErrObjectIntegrity
	}
	for _, character := range nonce {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return ErrObjectIntegrity
			}
		}
	}
	return nil
}
