package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// bundleName is the form of a checked-in connector name. It keeps the name
// inside internal/connectors/data.
var bundleName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// declaresBundle is the step "the (deployment|second deployment)
// configuration declares the "<name>" connector bundle" of the 284b
// scenarios (criteria-worker-v4-delta-284b-bundle.md, section "What the step
// must check, and what it must not do"). It declares one checked-in
// connector bundle as operator configuration of the deployment.
//
// It uses the shared deployment writer unchanged: it adds the key
// "connectors" to the copy of the stack configuration that this deployment
// holds, and then it writes the configuration once. The shared step of S0b
// does not change, and a scenario that does not use this step keeps a
// configuration with no "connectors" key. The step writes no PM row, runs no
// process and no SQL, copies no bundle and does not touch the secrets
// directory.
func (s *bootstrapScenario) declaresBundle(which, name string) error {
	d := s.shared.deployment
	if which == "second deployment" {
		d = s.second
	}
	if d == nil {
		return fmt.Errorf("the scenario has no %s configuration", which)
	}
	return declareCheckedInBundle(d, s.shared.harness.bins.root, name)
}

// declareCheckedInBundle declares one checked-in connector bundle of the
// checkout at root in the configuration of the deployment, with checks 1 to
// 5 of the 284b bundle addendum. It writes the configuration once. The lane
// uses it too, for the lane configuration (lane.start).
func declareCheckedInBundle(d *deployment, root, name string) error {
	// Item 1: the checked-in bundle of the checkout under test, by its
	// absolute and clean path.
	if !bundleName.MatchString(name) {
		return fmt.Errorf("item 1: %q is not the name of a checked-in connector bundle", name)
	}
	path := filepath.Join(root, "internal", "connectors", "data", name+".bundle.json")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("item 1: the bundle path %q is not absolute and clean", path)
	}
	// Item 2: a regular file whose connector_id is the name.
	information, err := os.Lstat(path)
	if err != nil || !information.Mode().IsRegular() {
		return fmt.Errorf("item 2: the checked-in bundle %s is not a regular file: %v", path, err)
	}
	contents, err := readBoundedFile(path, laneFileLimit)
	if err != nil {
		return fmt.Errorf("item 2: read the checked-in bundle: %w", err)
	}
	var bundle struct {
		ConnectorID string `json:"connector_id"`
	}
	if err := json.Unmarshal(contents, &bundle); err != nil || bundle.ConnectorID != name {
		return fmt.Errorf("item 2: the connector_id of %s is %q, want %q (%v)", path, bundle.ConnectorID, name, err)
	}
	// Item 3: no list yet.
	if _, found := d.stack.document["connectors"]; found {
		return errors.New("item 3: the deployment configuration already declares connectors")
	}
	// Item 4: before the first command that reads the configuration. The
	// shared writer writes the file at its first call, so a written file
	// means that a step already used the configuration.
	if d.configuration != "" {
		return errors.New("item 4: the deployment configuration file is already written")
	}
	written := filepath.Join(d.dir, "pm-deployment.json")
	if _, err := os.Lstat(written); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("item 4: the deployment configuration file already exists, or it cannot be checked: %v", err)
	}
	// The declaration: exactly one entry, in this deployment's own copy of
	// the stack configuration.
	connectors, err := json.Marshal(map[string][]string{"bundles": {path}})
	if err != nil {
		return fmt.Errorf("encode the connectors key: %w", err)
	}
	before := d.stack.document
	document := maps.Clone(before)
	document["connectors"] = connectors
	d.stack.document = document
	configuration, err := d.configurationPath()
	if err != nil {
		return err
	}
	return checkDeclaredConfiguration(configuration, before, path)
}

// checkDeclaredConfiguration is item 5. It reads the written configuration
// once. connectors.bundles must hold exactly the one path. Each other key of
// the stack configuration must be there, and the file must have no key that
// the shared writer does not write.
func checkDeclaredConfiguration(configuration string, stackDocument map[string]json.RawMessage, path string) error {
	contents, err := readBoundedFile(configuration, laneFileLimit)
	if err != nil {
		return fmt.Errorf("item 5: read the deployment configuration: %w", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		return fmt.Errorf("item 5: decode the deployment configuration: %w", err)
	}
	var connectors map[string][]string
	if err := json.Unmarshal(document["connectors"], &connectors); err != nil ||
		len(connectors) != 1 || len(connectors["bundles"]) != 1 || connectors["bundles"][0] != path {
		return fmt.Errorf("item 5: connectors is %s, want exactly one bundle entry %q", document["connectors"], path)
	}
	// The shared writer sets these keys and copies every other key of the
	// stack configuration unchanged (deployment_test.go, writeConfiguration).
	written := []string{"deployment_id", "public_origin", "server", "identity", "encryption", "worker", "temporal"}
	for key, value := range stackDocument {
		if slices.Contains(written, key) {
			continue
		}
		var want, got any
		if json.Unmarshal(value, &want) != nil || json.Unmarshal(document[key], &got) != nil ||
			!jsonEqual(want, got) {
			return fmt.Errorf("item 5: the configuration key %q differs from the stack configuration", key)
		}
	}
	for key := range document {
		if _, found := stackDocument[key]; !found && key != "connectors" && !slices.Contains(written, key) {
			return fmt.Errorf("item 5: the configuration has the key %q, which the shared writer does not write", key)
		}
	}
	return nil
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}
