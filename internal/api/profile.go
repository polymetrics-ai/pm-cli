package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	ProfilesSchemaVersion = "pm.client-profiles/v1"
	maximumProfilesBytes  = 64 << 10
	maximumProfiles       = 64
)

var (
	ErrProfileNotFound = errors.New("server profile not found")
	profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	handlePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$`)
)

// ServerProfile is deliberately non-secret. It selects one server instance,
// refers to a PM session held elsewhere and carries presentation/view defaults.
// It can never select a workspace or contain provider authentication material.
type ServerProfile struct {
	Name           string
	ServerURL      string
	InstanceID     string
	SessionHandle  string
	Output         *OutputFormat
	Wait           *bool
	RequestTimeout *time.Duration
}

func (profile ServerProfile) validate() error {
	if !profileNamePattern.MatchString(profile.Name) {
		return errors.New("server profile name is invalid")
	}
	parsed, err := url.ParseRequestURI(profile.ServerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("server profile URL must be an HTTPS origin")
	}
	if !validIdentity(profile.InstanceID, 512) {
		return errors.New("server profile instance identity is invalid")
	}
	if !handlePattern.MatchString(profile.SessionHandle) {
		return errors.New("server profile session handle is invalid")
	}
	if profile.Output != nil {
		if _, err := ParseOutputFormat(string(*profile.Output)); err != nil {
			return err
		}
	}
	if profile.RequestTimeout != nil && (*profile.RequestTimeout < time.Second || *profile.RequestTimeout > 5*time.Minute) {
		return errors.New("server profile request timeout must be between 1s and 5m")
	}
	return nil
}

// DefaultSessionHandle produces a fixed-size, non-secret key handle. Profile
// and instance bytes cannot expand the OS keyring command line.
func DefaultSessionHandle(profileName, instanceID string) (string, error) {
	if !profileNamePattern.MatchString(profileName) || !validIdentity(instanceID, 512) {
		return "", errors.New("cannot derive session handle from invalid profile identity")
	}
	digest := sha256.Sum256([]byte(profileName + "\x00" + instanceID))
	return "pm-session-" + hex.EncodeToString(digest[:]), nil
}

func DefaultProfilesPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil || directory == "" || !filepath.IsAbs(directory) {
		return "", errors.New("user configuration directory is unavailable")
	}
	return filepath.Join(directory, "pm", "profiles.json"), nil
}

type profilesDocumentWire struct {
	SchemaVersion string                       `json:"schema_version"`
	Profiles      map[string]serverProfileWire `json:"profiles"`
}

type serverProfileWire struct {
	ServerURL      string  `json:"server_url"`
	InstanceID     string  `json:"instance_id"`
	SessionHandle  string  `json:"session_handle"`
	Output         *string `json:"output,omitempty"`
	Wait           *bool   `json:"wait,omitempty"`
	RequestTimeout *string `json:"request_timeout,omitempty"`
}

// LoadServerProfile reads one bounded, strict profile document. Missing files
// and missing names are distinct from malformed existing configuration.
func LoadServerProfile(path, name string) (ServerProfile, error) {
	profiles, err := loadServerProfiles(path, false)
	if err != nil {
		return ServerProfile{}, err
	}
	profile, ok := profiles[name]
	if !ok {
		return ServerProfile{}, ErrProfileNotFound
	}
	return cloneServerProfile(profile), nil
}

func ListServerProfiles(path string) ([]ServerProfile, error) {
	profiles, err := loadServerProfiles(path, true)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	result := make([]ServerProfile, 0, len(names))
	for _, name := range names {
		result = append(result, cloneServerProfile(profiles[name]))
	}
	return result, nil
}

func AddServerProfile(path string, profile ServerProfile) error {
	if err := profile.validate(); err != nil {
		return err
	}
	profiles, err := loadServerProfiles(path, true)
	if err != nil {
		return err
	}
	if _, exists := profiles[profile.Name]; exists {
		return errors.New("server profile already exists")
	}
	if len(profiles) >= maximumProfiles {
		return errors.New("server profile limit reached")
	}
	profiles[profile.Name] = cloneServerProfile(profile)
	return writeServerProfiles(path, profiles)
}

func RemoveServerProfile(path, name string) error {
	if !profileNamePattern.MatchString(name) {
		return errors.New("server profile name is invalid")
	}
	profiles, err := loadServerProfiles(path, false)
	if err != nil {
		return err
	}
	if _, exists := profiles[name]; !exists {
		return ErrProfileNotFound
	}
	delete(profiles, name)
	return writeServerProfiles(path, profiles)
}

func loadServerProfiles(path string, missingIsEmpty bool) (map[string]ServerProfile, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("server profile path must be absolute")
	}
	information, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && missingIsEmpty {
		return make(map[string]ServerProfile), nil
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProfileNotFound
		}
		return nil, errors.New("server profile file is unavailable")
	}
	if information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() || information.Size() > maximumProfilesBytes {
		return nil, errors.New("server profile file must be a bounded regular file")
	}
	if information.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("server profile file permissions are too broad")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("server profile file is unavailable")
	}
	defer file.Close()
	document, err := io.ReadAll(io.LimitReader(file, maximumProfilesBytes+1))
	if err != nil || len(document) == 0 || len(document) > maximumProfilesBytes {
		return nil, errors.New("server profile document is empty or exceeds its bound")
	}
	return decodeServerProfiles(document)
}

func decodeServerProfiles(document []byte) (map[string]ServerProfile, error) {
	root, err := decodeJSONObject(document)
	if err != nil {
		return nil, errors.New("server profile document is malformed")
	}
	if err := validateObjectFieldNames(root, []string{"schema_version", "profiles"}, []string{"schema_version", "profiles"}); err != nil {
		return nil, errors.New("server profile document has unknown or missing fields")
	}
	if bytes.Equal(bytes.TrimSpace(root["schema_version"]), []byte("null")) || bytes.Equal(bytes.TrimSpace(root["profiles"]), []byte("null")) {
		return nil, errors.New("server profile document forbids null")
	}
	var schemaVersion string
	if json.Unmarshal(root["schema_version"], &schemaVersion) != nil || schemaVersion != ProfilesSchemaVersion {
		return nil, errors.New("server profile schema version is unsupported")
	}
	var rawProfiles map[string]json.RawMessage
	if json.Unmarshal(root["profiles"], &rawProfiles) != nil || rawProfiles == nil || len(rawProfiles) > maximumProfiles {
		return nil, errors.New("server profiles must be a bounded object")
	}
	profiles := make(map[string]ServerProfile, len(rawProfiles))
	for name, raw := range rawProfiles {
		profile, err := decodeServerProfile(name, raw)
		if err != nil {
			return nil, fmt.Errorf("server profile %q is invalid: %w", name, err)
		}
		profiles[name] = profile
	}
	return profiles, nil
}

func decodeServerProfile(name string, document []byte) (ServerProfile, error) {
	object, err := decodeJSONObject(document)
	if err != nil {
		return ServerProfile{}, errors.New("profile must be an object")
	}
	allowed := []string{"server_url", "instance_id", "session_handle", "output", "wait", "request_timeout"}
	required := []string{"server_url", "instance_id", "session_handle"}
	if err := validateObjectFieldNames(object, allowed, required); err != nil {
		return ServerProfile{}, errors.New("profile has unknown, wrongly cased or missing fields")
	}
	for field, raw := range object {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ServerProfile{}, fmt.Errorf("%s cannot be null", field)
		}
	}
	var wire serverProfileWire
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || requireJSONDecoderEOF(decoder) != nil {
		return ServerProfile{}, errors.New("profile field has the wrong type")
	}
	profile := ServerProfile{Name: name, ServerURL: wire.ServerURL, InstanceID: wire.InstanceID, SessionHandle: wire.SessionHandle, Wait: wire.Wait}
	if wire.Output != nil {
		output, err := ParseOutputFormat(*wire.Output)
		if err != nil {
			return ServerProfile{}, err
		}
		profile.Output = &output
	}
	if wire.RequestTimeout != nil {
		requestTimeout, err := time.ParseDuration(*wire.RequestTimeout)
		if err != nil || requestTimeout.String() != *wire.RequestTimeout {
			return ServerProfile{}, errors.New("request_timeout must be a canonical duration")
		}
		profile.RequestTimeout = &requestTimeout
	}
	if err := profile.validate(); err != nil {
		return ServerProfile{}, err
	}
	return profile, nil
}

func writeServerProfiles(path string, profiles map[string]ServerProfile) error {
	if path == "" || !filepath.IsAbs(path) || len(profiles) > maximumProfiles {
		return errors.New("server profile destination is invalid")
	}
	wire := profilesDocumentWire{SchemaVersion: ProfilesSchemaVersion, Profiles: make(map[string]serverProfileWire, len(profiles))}
	for name, profile := range profiles {
		profile.Name = name
		if err := profile.validate(); err != nil {
			return err
		}
		entry := serverProfileWire{ServerURL: profile.ServerURL, InstanceID: profile.InstanceID, SessionHandle: profile.SessionHandle, Wait: cloneBool(profile.Wait)}
		if profile.Output != nil {
			value := string(*profile.Output)
			entry.Output = &value
		}
		if profile.RequestTimeout != nil {
			value := profile.RequestTimeout.String()
			entry.RequestTimeout = &value
		}
		wire.Profiles[name] = entry
	}
	document, err := json.MarshalIndent(wire, "", "  ")
	if err != nil || len(document)+1 > maximumProfilesBytes {
		return errors.New("server profile document exceeds its bound")
	}
	document = append(document, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("server profile directory is unavailable")
	}
	if information, err := os.Lstat(directory); err != nil || information.Mode()&os.ModeSymlink != 0 || !information.IsDir() {
		return errors.New("server profile directory must be a real directory")
	}
	if information, err := os.Lstat(path); err == nil {
		if information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() || information.Mode().Perm()&0o077 != 0 {
			return errors.New("existing server profile file is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("server profile destination is unavailable")
	}
	temporary, err := os.CreateTemp(directory, ".profiles-*")
	if err != nil {
		return errors.New("server profile temporary file is unavailable")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("server profile permissions could not be set")
	}
	if _, err := temporary.Write(document); err != nil {
		temporary.Close()
		return errors.New("server profile document could not be written")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("server profile document could not be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("server profile document could not be closed")
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return errors.New("server profile document could not be committed")
	}
	return nil
}

func cloneServerProfile(profile ServerProfile) ServerProfile {
	copy := profile
	if profile.Output != nil {
		value := *profile.Output
		copy.Output = &value
	}
	copy.Wait = cloneBool(profile.Wait)
	if profile.RequestTimeout != nil {
		value := *profile.RequestTimeout
		copy.RequestTimeout = &value
	}
	return copy
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

var (
	ErrInvalidRequestKey   = errors.New("client request key is invalid")
	ErrRecoveryConflict    = errors.New("recovery key already identifies another intent")
	ErrRecoveryUnavailable = errors.New("client recovery record is unavailable")
)

// RequestKeySelection distinguishes an explicit retained-request selection
// from a newly allocated request identity.
type RequestKeySelection struct {
	RequestKey string
	Allocated  bool
}

// SelectRequestKey is the common Go client request-key owner. An explicit key
// selects that retained request. An absent key is allocated cryptographically
// and never recovered merely because a later command has equal payload.
func SelectRequestKey(explicit string) (RequestKeySelection, error) {
	if explicit != "" {
		if !validIdentity(explicit, 200) {
			return RequestKeySelection{}, ErrInvalidRequestKey
		}
		return RequestKeySelection{RequestKey: explicit}, nil
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return RequestKeySelection{}, ErrRecoveryUnavailable
	}
	return RequestKeySelection{
		RequestKey: "pm_" + base64.RawURLEncoding.EncodeToString(random[:]),
		Allocated:  true,
	}, nil
}

type RecoveryRecord struct {
	InstanceID              string
	WorkspaceID             string
	PrincipalKind           PrincipalKind
	PrincipalID             string
	Operation               string
	Target                  string
	RequestKey              string
	IntentFingerprint       contract.SemanticDigest
	TypedRequestFingerprint contract.SemanticDigest
}

// RecoveryStore durably records only a selected or client-allocated request
// key and secret-free digests. Raw mutation values and credentials cannot enter it.
type RecoveryStore interface {
	Persist(context.Context, RecoveryRecord) error
}

type FileRecoveryStore struct {
	directory string
}

func NewFileRecoveryStore(directory string) (*FileRecoveryStore, error) {
	if directory == "" || !filepath.IsAbs(directory) {
		return nil, ErrRecoveryUnavailable
	}
	return &FileRecoveryStore{directory: filepath.Clean(directory)}, nil
}

func DefaultRecoveryDirectory(profilesPath string) (string, error) {
	if profilesPath == "" || !filepath.IsAbs(profilesPath) {
		return "", ErrRecoveryUnavailable
	}
	return filepath.Join(filepath.Dir(profilesPath), "client-recovery"), nil
}

type recoveryWire struct {
	SchemaVersion           string `json:"schema_version"`
	ScopeFingerprint        string `json:"scope_fingerprint"`
	RequestKey              string `json:"request_key"`
	IntentFingerprint       string `json:"intent_fingerprint"`
	TypedRequestFingerprint string `json:"typed_request_fingerprint"`
}

func (store *FileRecoveryStore) Persist(ctx context.Context, record RecoveryRecord) error {
	if store == nil || ctx == nil ||
		!validIdentity(record.InstanceID, 512) || !validIdentity(record.WorkspaceID, 512) ||
		!record.PrincipalKind.Valid() || !validIdentity(record.PrincipalID, 512) ||
		!validIdentity(record.Operation, 512) || len(record.Target) > 1024 ||
		(record.Target != "" && strings.TrimSpace(record.Target) != record.Target) ||
		!validIdentity(record.RequestKey, 200) || record.IntentFingerprint.Validate() != nil ||
		record.TypedRequestFingerprint.Validate() != nil {
		return ErrRecoveryUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scopeDigest := sha256.Sum256([]byte(record.InstanceID + "\x00" + record.WorkspaceID + "\x00" +
		string(record.PrincipalKind) + "\x00" + record.PrincipalID + "\x00" + record.Operation + "\x00" + record.Target))
	scopeFingerprint := "sha256:" + hex.EncodeToString(scopeDigest[:])
	wire := recoveryWire{
		SchemaVersion:           "pm.client-recovery/v1",
		ScopeFingerprint:        scopeFingerprint,
		RequestKey:              record.RequestKey,
		IntentFingerprint:       string(record.IntentFingerprint),
		TypedRequestFingerprint: string(record.TypedRequestFingerprint),
	}
	document, err := json.Marshal(wire)
	if err != nil || len(document) > 2048 {
		return ErrRecoveryUnavailable
	}
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		return ErrRecoveryUnavailable
	}
	information, err := os.Lstat(store.directory)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.IsDir() || information.Mode().Perm()&0o077 != 0 {
		return ErrRecoveryUnavailable
	}
	nameDigest := sha256.Sum256([]byte(scopeFingerprint + "\x00" + record.RequestKey))
	path := filepath.Join(store.directory, hex.EncodeToString(nameDigest[:])+".json")
	temporary, err := os.CreateTemp(store.directory, ".recovery-*")
	if err != nil {
		return ErrRecoveryUnavailable
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return ErrRecoveryUnavailable
	}
	if _, err := temporary.Write(document); err != nil {
		temporary.Close()
		return ErrRecoveryUnavailable
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return ErrRecoveryUnavailable
	}
	if err := temporary.Close(); err != nil {
		return ErrRecoveryUnavailable
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return ErrRecoveryUnavailable
		}
		return verifyRecoveryFile(path, wire)
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return ErrRecoveryUnavailable
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil || closeErr != nil {
		return ErrRecoveryUnavailable
	}
	return nil
}

func verifyRecoveryFile(path string, expected recoveryWire) error {
	information, err := os.Lstat(path)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() ||
		information.Mode().Perm()&0o077 != 0 || information.Size() <= 0 || information.Size() > 2048 {
		return ErrRecoveryUnavailable
	}
	document, err := os.ReadFile(path)
	if err != nil || len(document) > 2048 {
		return ErrRecoveryUnavailable
	}
	object, err := decodeJSONObject(document)
	if err != nil || validateObjectFieldNames(object,
		[]string{"schema_version", "scope_fingerprint", "request_key", "intent_fingerprint", "typed_request_fingerprint"},
		[]string{"schema_version", "scope_fingerprint", "request_key", "intent_fingerprint", "typed_request_fingerprint"}) != nil {
		return ErrRecoveryUnavailable
	}
	var actual recoveryWire
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&actual) != nil || requireJSONDecoderEOF(decoder) != nil {
		return ErrRecoveryUnavailable
	}
	if actual != expected {
		return ErrRecoveryConflict
	}
	return nil
}
