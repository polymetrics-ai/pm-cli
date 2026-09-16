package contract

// CredentialRef is the safe, opaque binding visible to pure contracts. It never
// contains provider credential material, a private locator or a raw scope token.
type CredentialRef struct {
	CredentialRef          string   `json:"credential_ref"`
	WorkspaceID            string   `json:"workspace_id"`
	ConnectionID           string   `json:"connection_id"`
	ProviderAccountWitness string   `json:"provider_account_witness"`
	CiphertextVersion      uint64   `json:"ciphertext_version"`
	KeyRef                 string   `json:"key_ref"`
	Scopes                 []string `json:"scopes"`
	AuthEpoch              uint64   `json:"auth_epoch"`
}

func (r *CredentialRef) UnmarshalJSON(data []byte) error {
	type wire CredentialRef
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"credential_ref", "workspace_id", "connection_id", "provider_account_witness",
		"ciphertext_version", "key_ref", "scopes", "auth_epoch",
	); err != nil {
		return err
	}
	value := CredentialRef(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

func (r CredentialRef) Validate() error {
	identities := [...]struct {
		path  string
		value string
	}{
		{path: "credential_ref.credential_ref", value: r.CredentialRef},
		{path: "credential_ref.workspace_id", value: r.WorkspaceID},
		{path: "credential_ref.connection_id", value: r.ConnectionID},
		{path: "credential_ref.provider_account_witness", value: r.ProviderAccountWitness},
		{path: "credential_ref.key_ref", value: r.KeyRef},
	}
	for _, identity := range identities {
		if !validIdentityPart(identity.value) {
			return contractError(ErrInvalidIdentity, identity.path, "must be a nonempty opaque reference")
		}
	}
	if r.CiphertextVersion == 0 {
		return contractError(ErrInvalidContract, "credential_ref.ciphertext_version", "must be a positive immutable ciphertext version")
	}
	if err := validateIdentityList(r.Scopes, "credential_ref.scopes", true); err != nil {
		return err
	}
	if r.AuthEpoch == 0 {
		return contractError(ErrInvalidContract, "credential_ref.auth_epoch", "must bind a positive authorization epoch")
	}
	return nil
}
