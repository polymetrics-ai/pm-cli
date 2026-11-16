package main

import (
	"context"
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
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	identityRef   = "secret://spike/identity"
	encryptionRef = "secret://spike/encryption"
	clientID      = "pm-server"
)

// deployment is one pm-server behind an HTTPS edge, with its own CA, ID,
// secret root and configuration. One deployment exists for each identity
// provider candidate.
type deployment struct {
	stack        stackInputs
	dir          string
	ca           *authority
	caFile       string
	deploymentID string
	listen       string
	edge         *httptest.Server
	origin       string
	process      *exec.Cmd
	logPath      string
	exited       chan error
}

func newDeployment(stack stackInputs, parent, name string) (*deployment, error) {
	d := &deployment{stack: stack, deploymentID: "pm-login-spike-" + name + "-" + randomHex(4)}
	d.dir = filepath.Join(parent, "deployment-"+name)
	if err := os.MkdirAll(filepath.Join(d.dir, "home"), 0o700); err != nil {
		return nil, errors.New("create the deployment directory")
	}
	var err error
	if d.ca, err = newAuthority(); err != nil {
		return nil, err
	}
	d.caFile = filepath.Join(d.dir, "ca.pem")
	if err := writePrivateFile(d.caFile, d.ca.pem); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("reserve the pm-server address")
	}
	d.listen = listener.Addr().String()
	_ = listener.Close()
	certificate, _, _, err := d.ca.leaf("PM login spike edge")
	if err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "http", Host: d.listen}
	d.edge = httptest.NewUnstartedServer(&httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(response, "spike backend unavailable", http.StatusBadGateway)
		},
	})
	d.edge.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	d.edge.Config.ReadHeaderTimeout = 5 * time.Second
	d.edge.StartTLS()
	d.origin = d.edge.URL
	return d, nil
}

func (d *deployment) redirectURI() string { return d.origin + "/v1/sessions/browser/callback" }

// writeSecrets copies the stack members that pm-server reads and adds the
// identity provider client secret, its CA and a new wrapping key.
func (d *deployment) writeSecrets(clientSecret string, issuerCA []byte) (string, error) {
	root := filepath.Join(d.dir, "secrets")
	copies := []struct {
		reference string
		members   []string
	}{
		{d.stack.postgresRef, []string{"pm-app-endpoint"}},
		{d.stack.temporalRef, []string{"ca.pem", "client.pem", "client-key.pem"}},
		{d.stack.artifactsRef, []string{"ca.pem", "access-key", "secret-key"}},
	}
	for _, bundle := range copies {
		relative, err := secretBundlePath(bundle.reference)
		if err != nil {
			return "", err
		}
		for _, member := range bundle.members {
			contents, err := readBoundedFile(filepath.Join(d.stack.secretsDir, relative, member), 1<<20)
			if err != nil {
				return "", err
			}
			if err := writeSecretMember(root, relative, member, contents); err != nil {
				return "", err
			}
		}
	}
	identity, _ := secretBundlePath(identityRef)
	if err := writeSecretMember(root, identity, "oidc-client-secret", []byte(clientSecret)); err != nil {
		return "", err
	}
	if err := writeSecretMember(root, identity, "ca.pem", issuerCA); err != nil {
		return "", err
	}
	encryption, _ := secretBundlePath(encryptionRef)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", errors.New("generate the wrapping key")
	}
	if err := writeSecretMember(root, encryption, "wrapping-key", key); err != nil {
		return "", err
	}
	return root, nil
}

// writeConfiguration writes the deployment copy of the stack configuration
// with the deployment identity, the identity provider and the edge.
func (d *deployment) writeConfiguration(issuer string) (string, error) {
	document := maps.Clone(d.stack.document)
	values := map[string]any{
		"deployment_id": d.deploymentID,
		"public_origin": d.origin,
		"server": map[string]any{
			"listen":          d.listen,
			"trusted_proxies": []string{"127.0.0.1/32"},
			"drain_timeout":   "5s",
		},
		"identity": map[string]any{
			"issuer":              issuer,
			"client_id":           clientID,
			"client_secret_ref":   identityRef,
			"ca_ref":              identityRef,
			"step_up_maximum_age": "10m",
		},
		"encryption": map[string]any{"wrapping_key_ref": encryptionRef},
	}
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		document[key] = encoded
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(d.dir, "pm-server.json")
	return path, writePrivateFile(path, append(encoded, '\n'))
}

// start runs pm-server with a clean environment. The environment holds no
// session bus address and no token.
func (d *deployment) start(binary, configuration, secrets string) error {
	d.logPath = filepath.Join(d.dir, "pm-server.log")
	logFile, err := os.OpenFile(d.logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create the pm-server log")
	}
	command := exec.Command(binary, "--config", configuration, "--secrets-directory", secrets)
	command.Dir = filepath.Join(d.dir, "home")
	command.Env = []string{"HOME=" + filepath.Join(d.dir, "home"), "PATH=/usr/bin:/bin", "LANG=C", "TMPDIR=" + d.dir}
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return errors.New("start pm-server")
	}
	d.process = command
	d.exited = make(chan error, 1)
	go func() {
		d.exited <- command.Wait()
		_ = logFile.Close()
	}()
	return nil
}

// waitReady asks the health route through the edge until it answers 200 or
// the limit passes. pm-server at c59aa20 has no ready signal yet (#72 adds it),
// so this spike polls. A Gherkin step must wait for the ready signal instead.
func (d *deployment) waitReady(limit time.Duration) error {
	client := d.ca.client(nil, false)
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		select {
		case err := <-d.exited:
			d.exited <- err
			return errors.New("pm-server exited before it was ready")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.origin+"/health/ready", nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				cancel()
				return nil
			}
		}
		cancel()
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("pm-server was not ready within %s", limit)
}

func (d *deployment) stop() error {
	if d.process != nil && d.process.Process != nil {
		_ = syscall.Kill(-d.process.Process.Pid, syscall.SIGTERM)
		select {
		case <-d.exited:
		case <-time.After(20 * time.Second):
			_ = syscall.Kill(-d.process.Process.Pid, syscall.SIGKILL)
			<-d.exited
		}
		d.process = nil
	}
	if d.edge != nil {
		d.edge.Close()
		d.edge = nil
	}
	return nil
}
