package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrObjectCapacityExceeded  = errors.New("scoped object capacity exceeded")
	ErrObjectReservationClosed = errors.New("object reservation is no longer available")
)

// ObjectCapacityLimits are retained-byte and physical-attempt limits, not input
// or workflow-payload budgets. Bound or published attempts remain charged.
type ObjectCapacityLimits struct {
	OrganizationBytes    uint64
	WorkspaceBytes       uint64
	OrganizationAttempts uint64
	WorkspaceAttempts    uint64
}

func (limits ObjectCapacityLimits) Validate() error {
	if limits.WorkspaceBytes == 0 || limits.WorkspaceAttempts == 0 ||
		limits.WorkspaceBytes > limits.OrganizationBytes ||
		limits.WorkspaceAttempts > limits.OrganizationAttempts ||
		limits.OrganizationBytes > math.MaxInt64 || limits.OrganizationAttempts > math.MaxInt64 {
		return errors.New("object capacity limits are invalid")
	}
	return nil
}

type ObjectReservationRequest struct {
	ReservationID string
	ConnectionID  string
	ArtifactID    string
	Purpose       ArtifactPurpose
	MaximumBytes  uint64
}

type ObjectReservationState string

const (
	ObjectReservationHeld      ObjectReservationState = "held"
	ObjectReservationBound     ObjectReservationState = "bound"
	ObjectReservationPublished ObjectReservationState = "published"
	ObjectReservationReleased  ObjectReservationState = "released"
)

// ObjectAttemptBinding names the one immutable physical attempt that may consume
// a reservation. It is not evidence that those bytes reached object storage.
type ObjectAttemptBinding struct {
	Scope            contract.Scope
	ArtifactID       string
	ObjectAttemptKey string
	SHA256           string
	ByteLength       int64
	MediaType        string
}

type ObjectReservation struct {
	Scope        contract.Scope
	Request      ObjectReservationRequest
	State        ObjectReservationState
	ChargedBytes uint64
	Binding      ObjectAttemptBinding
	ReservedAt   time.Time
	PublishedAt  time.Time
}

// ArtifactPublicationReceipt binds the existing immutable artifact registration
// to its charged attempt. A creating transaction must commit before delivery.
type ArtifactPublicationReceipt struct {
	ReservationID string
	ConnectionID  string
	Purpose       ArtifactPurpose
	Object        contract.ObjectRef
	PublishedAt   time.Time
}

// ReserveObjectCapacity holds a finite ceiling before provider/object I/O. The
// caller commits this transaction before using the reservation. Exact replay
// never increments either counter, including after a lost commit reply.
func ReserveObjectCapacity(ctx context.Context, tx *Transaction, request ObjectReservationRequest, limits ObjectCapacityLimits) (ObjectReservation, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ObjectReservation{}, err
	}
	if err := limits.Validate(); err != nil {
		return ObjectReservation{}, err
	}
	if !validStoreID(request.ReservationID) || !validStoreID(request.ConnectionID) ||
		!validStoreID(request.ArtifactID) || !request.Purpose.valid() ||
		request.MaximumBytes == 0 || request.MaximumBytes > math.MaxInt64 {
		return ObjectReservation{}, errors.New("object capacity request is invalid")
	}
	if _, err := LoadConnection(ctx, tx, request.ConnectionID); err != nil {
		return ObjectReservation{}, err
	}
	organization, workspace, err := lockObjectCapacity(ctx, tx, scope)
	if err != nil {
		return ObjectReservation{}, err
	}
	stored, err := loadObjectReservation(ctx, tx, request.ReservationID, true)
	if err == nil {
		if stored.Request != request {
			return ObjectReservation{}, ErrImmutableConflict
		}
		if stored.State == ObjectReservationReleased {
			return ObjectReservation{}, ErrObjectReservationClosed
		}
		return stored, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return ObjectReservation{}, err
	}
	if !objectCapacityFits(organization, request.MaximumBytes, limits.OrganizationBytes, limits.OrganizationAttempts) ||
		!objectCapacityFits(workspace, request.MaximumBytes, limits.WorkspaceBytes, limits.WorkspaceAttempts) {
		return ObjectReservation{}, ErrObjectCapacityExceeded
	}
	if err := changeObjectCapacity(ctx, tx, scope, int64(request.MaximumBytes), 1); err != nil {
		return ObjectReservation{}, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.object_reservations (
			org_id, workspace_id, reservation_id, connection_id, artifact_id,
			purpose, maximum_bytes, charged_bytes
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
	`, scope.OrgID, scope.WorkspaceID, request.ReservationID, request.ConnectionID,
		request.ArtifactID, string(request.Purpose), request.MaximumBytes); err != nil {
		return ObjectReservation{}, fmt.Errorf("reserve immutable object capacity: %w", err)
	}
	return loadObjectReservation(ctx, tx, request.ReservationID, false)
}

// BindObjectAttempt permanently consumes the attempt slot before upload. Only
// unused byte slack is returned; a bound attempt cannot expire or be cancelled.
// An upload retry needs a new reservation and a new immutable attempt key.
func BindObjectAttempt(ctx context.Context, tx *Transaction, reservationID string, binding ObjectAttemptBinding) (ObjectReservation, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ObjectReservation{}, err
	}
	if binding.Scope != scope {
		return ObjectReservation{}, ErrScopeMismatch
	}
	if err := validateObjectAttemptBinding(binding); err != nil {
		return ObjectReservation{}, err
	}
	if _, _, err := lockObjectCapacity(ctx, tx, scope); err != nil {
		return ObjectReservation{}, err
	}
	reservation, err := loadObjectReservation(ctx, tx, reservationID, true)
	if err != nil {
		return ObjectReservation{}, err
	}
	if reservation.State == ObjectReservationBound || reservation.State == ObjectReservationPublished {
		if reservation.Binding != binding {
			return ObjectReservation{}, ErrImmutableConflict
		}
		return reservation, nil
	}
	if reservation.State != ObjectReservationHeld {
		return ObjectReservation{}, ErrObjectReservationClosed
	}
	if binding.ArtifactID != reservation.Request.ArtifactID || uint64(binding.ByteLength) > reservation.Request.MaximumBytes {
		return ObjectReservation{}, ErrImmutableConflict
	}
	slack := reservation.ChargedBytes - uint64(binding.ByteLength)
	if slack != 0 {
		if err := changeObjectCapacity(ctx, tx, scope, -int64(slack), 0); err != nil {
			return ObjectReservation{}, err
		}
	}
	if _, err := tx.exec(ctx, `
		UPDATE pm.object_reservations SET state = 'bound', charged_bytes = $2,
			object_attempt_key = $3, sha256 = $4, byte_length = $2, media_type = $5,
			bound_at = clock_timestamp()
		WHERE reservation_id = $1
	`, reservationID, binding.ByteLength, binding.ObjectAttemptKey, binding.SHA256, binding.MediaType); err != nil {
		return ObjectReservation{}, fmt.Errorf("bind immutable object attempt: %w", err)
	}
	return loadObjectReservation(ctx, tx, reservationID, false)
}

// CancelObjectReservation releases only a provably unbound reservation. Neither
// an elapsed deadline nor a missing upload acknowledgement frees a bound attempt.
func CancelObjectReservation(ctx context.Context, tx *Transaction, reservationID string) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if _, _, err := lockObjectCapacity(ctx, tx, scope); err != nil {
		return err
	}
	reservation, err := loadObjectReservation(ctx, tx, reservationID, true)
	if err != nil {
		return err
	}
	if reservation.State == ObjectReservationReleased {
		return nil
	}
	if reservation.State != ObjectReservationHeld {
		return ErrObjectReservationClosed
	}
	if err := changeObjectCapacity(ctx, tx, scope, -int64(reservation.ChargedBytes), -1); err != nil {
		return err
	}
	if _, err := tx.exec(ctx, `
		UPDATE pm.object_reservations SET state = 'released', charged_bytes = 0,
			released_at = clock_timestamp() WHERE reservation_id = $1
	`, reservationID); err != nil {
		return fmt.Errorf("release unused object capacity: %w", err)
	}
	return nil
}

func LoadObjectReservation(ctx context.Context, tx *Transaction, reservationID string) (ObjectReservation, error) {
	return loadObjectReservation(ctx, tx, reservationID, false)
}

// CommitObjectPublication registers verified bytes and their immutable receipt in
// the caller's transaction. Result/outcome/manifest transitions may join this
// transaction; there is deliberately no object I/O or second commit here.
func CommitObjectPublication(ctx context.Context, tx *Transaction, reservationID string, object PreverifiedObjectRef) (ArtifactPublicationReceipt, error) {
	reservation, err := loadObjectReservation(ctx, tx, reservationID, true)
	if err != nil {
		return ArtifactPublicationReceipt{}, err
	}
	ref := object.ref
	if err := ref.Validate(); err != nil {
		return ArtifactPublicationReceipt{}, ErrObjectNotVerified
	}
	if ref.OrgID != reservation.Scope.OrgID || ref.WorkspaceID != reservation.Scope.WorkspaceID {
		return ArtifactPublicationReceipt{}, ErrScopeMismatch
	}
	if !objectBindingMatchesReference(reservation.Binding, ref) {
		return ArtifactPublicationReceipt{}, ErrImmutableConflict
	}
	if reservation.State == ObjectReservationPublished {
		receipt, err := LoadArtifactPublication(ctx, tx, reservationID)
		if err != nil {
			return ArtifactPublicationReceipt{}, err
		}
		if receipt.Object != ref || receipt.Purpose != reservation.Request.Purpose {
			return ArtifactPublicationReceipt{}, ErrImmutableConflict
		}
		return receipt, nil
	}
	if reservation.State != ObjectReservationBound {
		return ArtifactPublicationReceipt{}, ErrObjectReservationClosed
	}
	if err := registerArtifact(ctx, tx, reservation.Request.ConnectionID, object, reservation.Request.Purpose); err != nil {
		return ArtifactPublicationReceipt{}, err
	}
	if _, err := tx.exec(ctx, `
		UPDATE pm.object_reservations SET state = 'published',
			published_artifact_id = artifact_id, published_at = clock_timestamp()
		WHERE reservation_id = $1
	`, reservationID); err != nil {
		return ArtifactPublicationReceipt{}, fmt.Errorf("commit immutable artifact publication: %w", err)
	}
	return LoadArtifactPublication(ctx, tx, reservationID)
}

// LoadArtifactPublication is the occurrence witness after an uncertain COMMIT.
// An uploaded object or an otherwise matching desired row is not this receipt.
func LoadArtifactPublication(ctx context.Context, tx *Transaction, reservationID string) (ArtifactPublicationReceipt, error) {
	reservation, err := loadObjectReservation(ctx, tx, reservationID, false)
	if err != nil {
		return ArtifactPublicationReceipt{}, err
	}
	if reservation.State != ObjectReservationPublished {
		return ArtifactPublicationReceipt{}, ErrNotFound
	}
	artifact, err := loadArtifactRecord(ctx, tx, reservation.Request.ArtifactID)
	if err != nil {
		return ArtifactPublicationReceipt{}, err
	}
	if !artifact.connectionID.Valid || artifact.connectionID.String != reservation.Request.ConnectionID ||
		artifact.purpose != reservation.Request.Purpose ||
		!objectBindingMatchesReference(reservation.Binding, artifact.ref) {
		return ArtifactPublicationReceipt{}, ErrImmutableConflict
	}
	if err := artifact.ref.Validate(); err != nil {
		return ArtifactPublicationReceipt{}, ErrObjectNotVerified
	}
	return ArtifactPublicationReceipt{
		ReservationID: reservationID, ConnectionID: reservation.Request.ConnectionID,
		Purpose: reservation.Request.Purpose, Object: artifact.ref,
		PublishedAt: reservation.PublishedAt,
	}, nil
}

type objectCapacity struct {
	bytes    uint64
	attempts uint64
}

func objectCapacityFits(current objectCapacity, bytes, maximumBytes, maximumAttempts uint64) bool {
	return current.bytes <= maximumBytes && bytes <= maximumBytes-current.bytes && current.attempts < maximumAttempts
}

// Every counter mutation takes the same org -> workspace -> reservation order.
func lockObjectCapacity(ctx context.Context, tx *Transaction, scope contract.Scope) (objectCapacity, objectCapacity, error) {
	if _, err := tx.exec(ctx, `INSERT INTO pm.object_org_capacity (org_id) VALUES ($1) ON CONFLICT DO NOTHING`, scope.OrgID); err != nil {
		return objectCapacity{}, objectCapacity{}, fmt.Errorf("ensure organization object capacity: %w", err)
	}
	var organization, workspace objectCapacity
	if err := tx.queryRow(ctx, `SELECT reserved_bytes, reserved_attempts FROM pm.object_org_capacity WHERE org_id = $1 FOR UPDATE`, scope.OrgID).Scan(&organization.bytes, &organization.attempts); err != nil {
		return objectCapacity{}, objectCapacity{}, fmt.Errorf("lock organization object capacity: %w", err)
	}
	if _, err := tx.exec(ctx, `INSERT INTO pm.object_workspace_capacity (org_id, workspace_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, scope.OrgID, scope.WorkspaceID); err != nil {
		return objectCapacity{}, objectCapacity{}, fmt.Errorf("ensure workspace object capacity: %w", err)
	}
	if err := tx.queryRow(ctx, `SELECT reserved_bytes, reserved_attempts FROM pm.object_workspace_capacity WHERE org_id = $1 AND workspace_id = $2 FOR UPDATE`, scope.OrgID, scope.WorkspaceID).Scan(&workspace.bytes, &workspace.attempts); err != nil {
		return objectCapacity{}, objectCapacity{}, fmt.Errorf("lock workspace object capacity: %w", err)
	}
	return organization, workspace, nil
}

func changeObjectCapacity(ctx context.Context, tx *Transaction, scope contract.Scope, bytes, attempts int64) error {
	if _, err := tx.exec(ctx, `UPDATE pm.object_org_capacity SET reserved_bytes = reserved_bytes + $2, reserved_attempts = reserved_attempts + $3 WHERE org_id = $1`, scope.OrgID, bytes, attempts); err != nil {
		return fmt.Errorf("adjust organization object capacity: %w", err)
	}
	if _, err := tx.exec(ctx, `UPDATE pm.object_workspace_capacity SET reserved_bytes = reserved_bytes + $3, reserved_attempts = reserved_attempts + $4 WHERE org_id = $1 AND workspace_id = $2`, scope.OrgID, scope.WorkspaceID, bytes, attempts); err != nil {
		return fmt.Errorf("adjust workspace object capacity: %w", err)
	}
	return nil
}

func loadObjectReservation(ctx context.Context, tx *Transaction, reservationID string, lock bool) (ObjectReservation, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ObjectReservation{}, err
	}
	if !validStoreID(reservationID) {
		return ObjectReservation{}, ErrNotFound
	}
	query := `SELECT connection_id, artifact_id, purpose, maximum_bytes, state, charged_bytes,
		object_attempt_key, sha256, byte_length, media_type, reserved_at, published_at
		FROM pm.object_reservations WHERE reservation_id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var reservation ObjectReservation
	reservation.Scope = scope
	reservation.Request.ReservationID = reservationID
	var key, digest, mediaType sql.NullString
	var length sql.NullInt64
	var publishedAt sql.NullTime
	var purpose string
	if err := tx.queryRow(ctx, query, reservationID).Scan(
		&reservation.Request.ConnectionID, &reservation.Request.ArtifactID, &purpose,
		&reservation.Request.MaximumBytes, &reservation.State, &reservation.ChargedBytes,
		&key, &digest, &length, &mediaType, &reservation.ReservedAt, &publishedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return ObjectReservation{}, ErrNotFound
	} else if err != nil {
		return ObjectReservation{}, fmt.Errorf("load object reservation: %w", err)
	}
	reservation.Request.Purpose = ArtifactPurpose(purpose)
	if !reservation.Request.Purpose.valid() {
		return ObjectReservation{}, ErrImmutableConflict
	}
	reservation.ReservedAt = reservation.ReservedAt.UTC()
	switch reservation.State {
	case ObjectReservationHeld, ObjectReservationReleased:
		if key.Valid || digest.Valid || length.Valid || mediaType.Valid || publishedAt.Valid {
			return ObjectReservation{}, ErrImmutableConflict
		}
	case ObjectReservationBound, ObjectReservationPublished:
		if !key.Valid || !digest.Valid || !length.Valid || !mediaType.Valid ||
			publishedAt.Valid != (reservation.State == ObjectReservationPublished) {
			return ObjectReservation{}, ErrImmutableConflict
		}
		reservation.Binding = ObjectAttemptBinding{
			Scope: scope, ArtifactID: reservation.Request.ArtifactID,
			ObjectAttemptKey: key.String, SHA256: digest.String,
			ByteLength: length.Int64, MediaType: mediaType.String,
		}
		if err := validateObjectAttemptBinding(reservation.Binding); err != nil {
			return ObjectReservation{}, err
		}
		if publishedAt.Valid {
			reservation.PublishedAt = publishedAt.Time.UTC()
		}
	default:
		return ObjectReservation{}, ErrImmutableConflict
	}
	return reservation, nil
}

func validateObjectAttemptBinding(binding ObjectAttemptBinding) error {
	if err := binding.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(binding.ArtifactID) || binding.ObjectAttemptKey == "" || len(binding.ObjectAttemptKey) > 2048 ||
		strings.ContainsAny(binding.ObjectAttemptKey, "\x00\r\n") || len(binding.SHA256) != 64 || binding.ByteLength < 0 ||
		binding.MediaType == "" || len(binding.MediaType) > 512 || !utf8.ValidString(binding.MediaType) || strings.ContainsAny(binding.MediaType, "\r\n") {
		return errors.New("immutable object attempt descriptor is invalid")
	}
	for _, character := range binding.SHA256 {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return errors.New("immutable object attempt digest is invalid")
		}
	}
	if _, _, err := mime.ParseMediaType(binding.MediaType); err != nil {
		return errors.New("immutable object attempt media type is invalid")
	}
	return nil
}

func objectBindingMatchesReference(binding ObjectAttemptBinding, ref contract.ObjectRef) bool {
	return binding.Scope.OrgID == ref.OrgID && binding.Scope.WorkspaceID == ref.WorkspaceID &&
		binding.ArtifactID == ref.ArtifactID && binding.ObjectAttemptKey == ref.ObjectAttemptKey &&
		binding.SHA256 == ref.SHA256 && binding.ByteLength == ref.ByteLength && binding.MediaType == ref.MediaType
}
