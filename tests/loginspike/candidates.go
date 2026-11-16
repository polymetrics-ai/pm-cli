package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// candidate is one open-source OIDC provider that runs as a container in the
// job's engine, like the stack services. Run 1 used the host network, and
// Keycloak failed because its HTTP listener found port 8080 in use on the
// runner. Each provider now has its own network namespace and publishes one
// port on the runner loopback only.
type candidate struct {
	id         string
	references []string
	user       string
	login      string
	issuerPath string
	ready      *regexp.Regexp
	readyLimit time.Duration
	// prepare writes the configuration into dir and returns the container
	// arguments after the image reference is known.
	prepare func(dir string, port int, issuer, redirectURI, clientSecret, password string, certPEM, keyPEM []byte) (func(image string) []string, error)
}

var candidates = []candidate{
	{
		id:         "2.dex",
		references: []string{"ghcr.io/dexidp/dex:v2.45.1", "ghcr.io/dexidp/dex:v2.45.0", "ghcr.io/dexidp/dex:v2.44.0"},
		user:       "operator@login-spike.invalid",
		login:      "operator@login-spike.invalid",
		issuerPath: "/dex",
		ready:      regexp.MustCompile(`(?i)listening.*https`),
		readyLimit: 90 * time.Second,
		prepare:    prepareDex,
	},
	{
		id:         "2.keycloak",
		references: []string{"quay.io/keycloak/keycloak:26.8.1", "quay.io/keycloak/keycloak:26.8.0", "quay.io/keycloak/keycloak:26.7.5"},
		user:       "pm-operator",
		login:      "pm-operator",
		issuerPath: "/realms/pm",
		ready:      regexp.MustCompile(`started in .*Listening on`),
		readyLimit: 4 * time.Minute,
		prepare:    prepareKeycloak,
	},
}

// prepareDex writes a Dex configuration. The user password is stored only as a
// bcrypt hash. The client secret is in the file, as Dex static clients need it.
func prepareDex(dir string, port int, issuer, redirectURI, clientSecret, password string, certPEM, keyPEM []byte) (func(string) []string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return nil, errors.New("hash the Dex user password")
	}
	config := map[string]any{
		"issuer":  issuer,
		"storage": map[string]any{"type": "memory"},
		"web": map[string]any{
			"https":   "0.0.0.0:5556",
			"tlsCert": "/cfg/tls.pem",
			"tlsKey":  "/cfg/tls-key.pem",
		},
		"logger":           map[string]any{"level": "info", "format": "text"},
		"oauth2":           map[string]any{"skipApprovalScreen": true, "responseTypes": []string{"code"}},
		"enablePasswordDB": true,
		"staticClients": []map[string]any{{
			"id":           clientID,
			"secret":       clientSecret,
			"name":         "PM login spike",
			"redirectURIs": []string{redirectURI},
		}},
		"staticPasswords": []map[string]any{{
			"email":    "operator@login-spike.invalid",
			"hash":     string(hash),
			"username": "pm-operator",
			"userID":   "4c1f2a8e-6f0b-4d7e-9a51-0d3b8c2e7f10",
		}},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	// JSON is valid YAML, so Dex reads this file directly.
	for name, contents := range map[string][]byte{"config.yaml": encoded, "tls.pem": certPEM, "tls-key.pem": keyPEM} {
		if err := writePrivateFile(filepath.Join(dir, name), contents); err != nil {
			return nil, err
		}
	}
	return func(image string) []string {
		return []string{
			"--user", "0:0", "--publish", "127.0.0.1:" + strconv.Itoa(port) + ":5556", "--read-only",
			"--tmpfs", "/tmp:size=16m", "--memory", "256m", "--cpus", "0.5", "--pids-limit", "128",
			"--security-opt", "no-new-privileges",
			"--volume", dir + ":/cfg:ro",
			image, "dex", "serve", "/cfg/config.yaml",
		}
	}, nil
}

// prepareKeycloak writes a realm import. Keycloak needs the client secret and
// the user password in the import file; the file is 0600 in the job directory
// and is removed with it.
func prepareKeycloak(dir string, port int, issuer, redirectURI, clientSecret, password string, certPEM, keyPEM []byte) (func(string) []string, error) {
	realm := map[string]any{
		"realm":       "pm",
		"enabled":     true,
		"sslRequired": "all",
		"clients": []map[string]any{{
			"clientId":                  clientID,
			"enabled":                   true,
			"protocol":                  "openid-connect",
			"publicClient":              false,
			"clientAuthenticatorType":   "client-secret",
			"secret":                    clientSecret,
			"standardFlowEnabled":       true,
			"directAccessGrantsEnabled": false,
			"implicitFlowEnabled":       false,
			"serviceAccountsEnabled":    false,
			"consentRequired":           false,
			"redirectUris":              []string{redirectURI},
			"webOrigins":                []string{},
			"attributes":                map[string]string{"pkce.code.challenge.method": "S256"},
		}},
		"users": []map[string]any{{
			"username":        "pm-operator",
			"enabled":         true,
			"email":           "operator@login-spike.invalid",
			"emailVerified":   true,
			"firstName":       "PM",
			"lastName":        "Operator",
			"requiredActions": []string{},
			"credentials":     []map[string]any{{"type": "password", "value": password, "temporary": false}},
		}},
	}
	encoded, err := json.MarshalIndent(realm, "", "  ")
	if err != nil {
		return nil, err
	}
	for name, contents := range map[string][]byte{"pm-realm.json": encoded, "tls.pem": certPEM, "tls-key.pem": keyPEM} {
		if err := writePrivateFile(filepath.Join(dir, name), contents); err != nil {
			return nil, err
		}
	}
	clear(encoded)
	return func(image string) []string {
		return []string{
			"--user", "0:0", "--publish", "127.0.0.1:" + strconv.Itoa(port) + ":8443",
			"--memory", "1536m", "--cpus", "2", "--pids-limit", "512",
			"--security-opt", "no-new-privileges",
			"--volume", filepath.Join(dir, "pm-realm.json") + ":/opt/keycloak/data/import/pm-realm.json:ro",
			"--volume", filepath.Join(dir, "tls.pem") + ":/cfg/tls.pem:ro",
			"--volume", filepath.Join(dir, "tls-key.pem") + ":/cfg/tls-key.pem:ro",
			image, "start-dev", "--import-realm",
			"--http-enabled=false",
			"--http-host=0.0.0.0",
			"--https-port=8443",
			"--https-certificate-file=/cfg/tls.pem",
			"--https-certificate-key-file=/cfg/tls-key.pem",
			"--hostname=https://127.0.0.1:" + strconv.Itoa(port),
		}
	}, nil
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func candidateDir(parent, id string) (string, error) {
	dir := filepath.Join(parent, "idp-"+id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s", dir)
	}
	return dir, nil
}
