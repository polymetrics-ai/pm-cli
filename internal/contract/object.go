package contract

// ObjectRef is the sole Go representation of the canonical eight-field
// ArtifactRef wire contract. Validation establishes descriptor shape only; it
// does not prove registered-object membership or the bytes stored remotely.
type ObjectRef struct {
	OrgID             string `json:"org_id"`
	WorkspaceID       string `json:"workspace_id"`
	ArtifactID        string `json:"artifact_id"`
	ObjectAttemptKey  string `json:"object_attempt_key"`
	SHA256            string `json:"sha256"`
	ByteLength        int64  `json:"byte_length"`
	MediaType         string `json:"media_type"`
	BackendGeneration string `json:"backend_generation"`
}

func (r *ObjectRef) UnmarshalJSON(data []byte) error {
	type wire struct {
		OrgID             *string `json:"org_id"`
		WorkspaceID       *string `json:"workspace_id"`
		ArtifactID        *string `json:"artifact_id"`
		ObjectAttemptKey  *string `json:"object_attempt_key"`
		SHA256            *string `json:"sha256"`
		ByteLength        *int64  `json:"byte_length"`
		MediaType         *string `json:"media_type"`
		BackendGeneration *string `json:"backend_generation"`
	}
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	if decoded.OrgID == nil || decoded.WorkspaceID == nil || decoded.ArtifactID == nil ||
		decoded.ObjectAttemptKey == nil || decoded.SHA256 == nil || decoded.ByteLength == nil ||
		decoded.MediaType == nil || decoded.BackendGeneration == nil {
		return contractError(ErrInvalidValue, "binary_ref", "must explicitly declare every immutable artifact reference field")
	}
	value := ObjectRef{
		OrgID:             *decoded.OrgID,
		WorkspaceID:       *decoded.WorkspaceID,
		ArtifactID:        *decoded.ArtifactID,
		ObjectAttemptKey:  *decoded.ObjectAttemptKey,
		SHA256:            *decoded.SHA256,
		ByteLength:        *decoded.ByteLength,
		MediaType:         *decoded.MediaType,
		BackendGeneration: *decoded.BackendGeneration,
	}
	if err := value.Validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

func (r ObjectRef) Validate() error {
	if !validIdentityPart(r.OrgID) {
		return contractError(ErrInvalidValue, "binary_ref.org_id", "must scope the artifact to one organization")
	}
	if !validIdentityPart(r.WorkspaceID) {
		return contractError(ErrInvalidValue, "binary_ref.workspace_id", "must scope the artifact to one workspace")
	}
	if !validIdentityPart(r.ArtifactID) {
		return contractError(ErrInvalidValue, "binary_ref.artifact_id", "must be a nonempty valid UTF-8 opaque ID")
	}
	if !validIdentityPart(r.ObjectAttemptKey) {
		return contractError(ErrInvalidValue, "binary_ref.object_attempt_key", "must identify the unique publication attempt")
	}
	if !validSHA256Hex(r.SHA256) {
		return contractError(ErrInvalidValue, "binary_ref.sha256", "must be an exact lowercase SHA-256")
	}
	if r.ByteLength < 0 {
		return contractError(ErrInvalidValue, "binary_ref.byte_length", "must not be negative")
	}
	if err := validateMediaType(r.MediaType); err != nil {
		return err
	}
	if !validIdentityPart(r.BackendGeneration) {
		return contractError(ErrInvalidValue, "binary_ref.backend_generation", "must be a nonempty immutable backend generation")
	}
	return nil
}
