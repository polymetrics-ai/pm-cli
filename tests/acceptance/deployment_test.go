package acceptance

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// deployment is one new PM deployment configuration on the job-owned stack,
// apart from the lane of PR #261. Each deployment has its own deployment ID,
// loopback listen address, CA, identity provider, HTTPS edge and public
// origin, and a Temporal namespace that is the stack namespace unless a step
// changes it. pm-server listens on plain HTTP behind the HTTPS edge, because
// authenticated API and browser traffic needs such an edge
// (docs/development/setup.md:149).
type deployment struct {
	stack         stackInputs
	dir           string
	deploymentID  string
	listen        string
	origin        string
	caFile        string
	namespace     string
	secrets       string
	configuration string
	authority     *laneAuthority
	identity      *laneIdentityProvider
	edge          *httptest.Server
}

// newDeployment makes the deployment directory, the CA, the identity
// provider, the HTTPS edge and the secret root. The first start writes the
// configuration.
func newDeployment(stack stackInputs) (*deployment, error) {
	d := &deployment{stack: stack, namespace: stack.temporal.namespace}
	if err := d.open(); err != nil {
		return nil, errors.Join(err, d.close())
	}
	return d, nil
}

func (d *deployment) open() error {
	var err error
	if d.dir, err = os.MkdirTemp("", "pm-acceptance-deployment-"); err != nil {
		return fmt.Errorf("create the deployment directory: %w", err)
	}
	for _, directory := range []string{"home", "tmp"} {
		if err := os.Mkdir(filepath.Join(d.dir, directory), 0o700); err != nil {
			return fmt.Errorf("create the deployment %s directory: %w", directory, err)
		}
	}
	if d.authority, err = newLaneAuthority(); err != nil {
		return err
	}
	d.caFile = filepath.Join(d.dir, "deployment-ca.pem")
	if err := writePrivateFile(d.caFile, d.authority.pem); err != nil {
		return err
	}
	if d.identity, err = newLaneIdentityProvider(d.authority); err != nil {
		return err
	}
	if d.listen, err = reserveLoopbackAddress(); err != nil {
		return err
	}
	if d.edge, err = newLaneEdge(d.authority, d.listen); err != nil {
		return err
	}
	d.origin = d.edge.URL
	d.deploymentID = "pm-acceptance-" + randomHex(6)
	d.secrets, err = d.writeSecrets(d.stack)
	return err
}

// useTemporalNamespace sets the Temporal namespace of the configuration. It
// must come before the first start, because the first start writes the
// configuration.
func (d *deployment) useTemporalNamespace(namespace string) error {
	if d.configuration != "" {
		return errors.New("the deployment configuration is already written")
	}
	d.namespace = namespace
	return nil
}

// configurationPath writes the configuration at the first call and returns
// its path.
func (d *deployment) configurationPath() (string, error) {
	if d.configuration == "" {
		path, err := d.writeConfiguration(d.stack, d.listen)
		if err != nil {
			return "", err
		}
		d.configuration = path
	}
	return d.configuration, nil
}

// home is the working directory and HOME of the started processes.
func (d *deployment) home() string {
	return filepath.Join(d.dir, "home")
}

// processEnvironment is a clean environment: no proxy, no inherited PM or
// cloud setting.
func (d *deployment) processEnvironment() []string {
	return []string{"HOME=" + d.home(), "PATH=/usr/bin:/bin", "LANG=C", "TMPDIR=" + filepath.Join(d.dir, "tmp")}
}

// close stops the edge and the identity provider and removes the deployment
// directory. The steps stop the processes before they close the deployment.
func (d *deployment) close() error {
	if d.edge != nil {
		d.edge.Close()
	}
	d.identity.close()
	if d.dir != "" {
		return os.RemoveAll(d.dir)
	}
	return nil
}

// writeSecrets makes the deployment operator secret root. It copies only the
// stack secret members that the two processes read, and it adds the
// deployment identity provider secret, its CA and a new wrapping key.
// pm-server opens the root with os.Root, so the members are copies, not links.
func (d *deployment) writeSecrets(stack stackInputs) (string, error) {
	root := filepath.Join(d.dir, "secrets")
	copies := []struct {
		reference string
		members   []string
	}{
		{stack.postgresRef, []string{"pm-app-endpoint"}},
		{stack.temporalRef, []string{"ca.pem", "client.pem", "client-key.pem"}},
		{stack.artifactsRef, []string{"ca.pem", "access-key", "secret-key"}},
	}
	for _, bundle := range copies {
		relative, err := secretBundlePath(bundle.reference)
		if err != nil {
			return "", err
		}
		for _, member := range bundle.members {
			contents, err := readBoundedFile(filepath.Join(stack.secretsDir, relative, member), laneFileLimit)
			if err != nil {
				return "", err
			}
			if err := writeSecretMember(root, relative, member, contents); err != nil {
				return "", err
			}
		}
	}
	identity, err := secretBundlePath(laneIdentityRef)
	if err != nil {
		return "", err
	}
	if err := writeSecretMember(root, identity, "oidc-client-secret", []byte(d.identity.clientSecret)); err != nil {
		return "", err
	}
	if err := writeSecretMember(root, identity, "ca.pem", d.authority.pem); err != nil {
		return "", err
	}
	encryption, err := secretBundlePath(laneEncryptionRef)
	if err != nil {
		return "", err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate the deployment wrapping key: %w", err)
	}
	if err := writeSecretMember(root, encryption, "wrapping-key", key); err != nil {
		return "", err
	}
	return root, nil
}

// writeConfiguration writes the deployment copy of the stack configuration.
// The copy keeps the stack PostgreSQL, Temporal, artifact, admission and
// limit settings. It changes the deployment identity, the public origin, the
// identity provider, the wrapping key reference and the Temporal namespace,
// and adds the server and worker controls. The stack configuration file does
// not change.
func (d *deployment) writeConfiguration(stack stackInputs, backend string) (string, error) {
	document := maps.Clone(stack.document)
	temporal := map[string]json.RawMessage{}
	if err := json.Unmarshal(document["temporal"], &temporal); err != nil {
		return "", fmt.Errorf("decode the stack temporal configuration: %w", err)
	}
	namespace, err := json.Marshal(d.namespace)
	if err != nil {
		return "", fmt.Errorf("encode the deployment Temporal namespace: %w", err)
	}
	temporal["namespace"] = namespace
	values := map[string]any{
		"deployment_id": d.deploymentID,
		"public_origin": d.origin,
		"server": map[string]any{
			"listen":          backend,
			"trusted_proxies": []string{"127.0.0.1/32"},
			"drain_timeout":   "5s",
		},
		"identity": map[string]any{
			"issuer":            d.identity.issuer,
			"client_id":         laneOIDCClientID,
			"client_secret_ref": laneIdentityRef,
			"ca_ref":            laneIdentityRef,
		},
		"encryption": map[string]any{"wrapping_key_ref": laneEncryptionRef},
		"worker":     laneWorkerControls,
		"temporal":   temporal,
	}
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("encode the deployment %s configuration: %w", key, err)
		}
		document[key] = encoded
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the deployment configuration: %w", err)
	}
	path := filepath.Join(d.dir, "pm-deployment.json")
	if err := writePrivateFile(path, append(encoded, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

// newLaneEdge starts the HTTPS edge of the lane. pm-server listens on plain
// HTTP behind a trusted edge, as it does in a deployment. The edge keeps the
// public Host and sets the X-Forwarded headers that pm-server checks.
func newLaneEdge(authority *laneAuthority, backend string) (*httptest.Server, error) {
	certificate, err := authority.serverCertificate("PM acceptance lane edge")
	if err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "http", Host: backend}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	edge := httptest.NewUnstartedServer(&httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(response, "lane backend unavailable", http.StatusBadGateway)
		},
	})
	edge.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	edge.Config.ReadHeaderTimeout = 5 * time.Second
	edge.StartTLS()
	return edge, nil
}

// reserveLoopbackAddress returns a free loopback address for the pm-server
// listener. pm-server opens its own listener, so the lane closes this one.
func reserveLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve a loopback address for pm-server: %w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("release the reserved pm-server address: %w", err)
	}
	return address, nil
}
