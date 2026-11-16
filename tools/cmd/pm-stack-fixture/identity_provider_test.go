package main

import (
	"bytes"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func generateTestFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	repositoryRoot := realTemporaryDirectory(t)
	writeExampleConfiguration(t, repositoryRoot)
	root := filepath.Join(realTemporaryDirectory(t), "fixture")
	var stdout, stderr bytes.Buffer
	if status := run([]string{
		"--root", root,
		"--project", "pm-fixture-test",
		"--repo-root", repositoryRoot,
	}, &stdout, &stderr); status != 0 {
		t.Fatalf("generate fixture status = %d: %s", status, stderr.String())
	}
	var environment map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &environment); err != nil {
		t.Fatalf("decode generated environment: %v", err)
	}
	return root, environment
}

func TestGeneratedIdentityProviderUsesASecondLoopbackAddressAndExactIssuer(t *testing.T) {
	root, environment := generateTestFixture(t)
	host := environment["PM_STACK_IDENTITY_PROVIDER_HOST"]
	address, err := netip.ParseAddr(host)
	if err != nil || !address.Is4() || !address.IsLoopback() || address.String() != host ||
		address == netip.AddrFrom4([4]byte{127, 0, 0, 1}) {
		t.Fatalf("identity provider host = %q, want a canonical IPv4 loopback address other than PM's", host)
	}
	port := environment["PM_STACK_IDENTITY_PROVIDER_PORT"]
	if number, err := strconv.Atoi(port); err != nil || number < 1024 || number > 65535 {
		t.Fatalf("identity provider port = %q, want a non-privileged port", port)
	}
	for _, name := range portNames {
		if name != "IDENTITY_PROVIDER" && environment["PM_STACK_"+name+"_PORT"] == port {
			t.Fatalf("identity provider port reuses PM_STACK_%s_PORT", name)
		}
	}
	if got, want := environment["PM_STACK_IDENTITY_PROVIDER_ISSUER"], "https://"+net.JoinHostPort(host, port)+"/realms/pm-test"; got != want {
		t.Fatalf("issuer = %q, want %q", got, want)
	}
	directory := filepath.Join(root, "identity-provider")
	if environment["PM_STACK_IDENTITY_PROVIDER_DIR"] != directory {
		t.Fatalf("identity provider directory = %q, want %q", environment["PM_STACK_IDENTITY_PROVIDER_DIR"], directory)
	}
	secret := readFixtureText(t, directory, "admin-client-secret")
	for _, value := range environment {
		if strings.Contains(value, secret) {
			t.Fatal("the generated environment carries the admin client secret")
		}
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("identity provider directory mode = %v (%v), want a 0700 directory", info.Mode(), err)
	}
	for name, mode := range map[string]os.FileMode{
		"ca.pem":              0o600,
		"server.pem":          0o640,
		"server-key.pem":      0o640,
		"realm.json":          0o640,
		"admin-client-id":     0o600,
		"admin-client-secret": 0o600,
	} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %v (%v), want regular %v", name, info.Mode(), err, mode)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 6 {
		t.Fatalf("identity provider directory has %d entries (%v), want 6", len(entries), err)
	}
}

func TestGeneratedIdentityProviderCertificateNamesOnlyItsAddress(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	address := netip.AddrFrom4([4]byte{127, 31, 41, 59})
	bundle, err := generateIdentityProviderBundle("pm-fixture-test", address, now)
	if err != nil {
		t.Fatalf("generate identity provider TLS material: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle["ca.pem"]) {
		t.Fatal("generated CA is not valid PEM")
	}
	pair, err := tls.X509KeyPair(bundle["server.pem"], bundle["server-key.pem"])
	if err != nil {
		t.Fatalf("load generated identity provider identity: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := leaf.PublicKey.(*rsa.PublicKey); !ok || key.N.BitLen() != 2048 {
		t.Fatal("identity provider key is not RSA 2048, the form the spike proved Keycloak reads")
	}
	if len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.IP(address.AsSlice())) {
		t.Fatalf("identity provider names = %v %v, want only its address", leaf.DNSNames, leaf.IPAddresses)
	}
	verify := func(name string) error {
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, DNSName: name, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
	if err := verify(address.String()); err != nil {
		t.Fatalf("identity provider certificate does not verify for its address: %v", err)
	}
	for _, name := range []string{"127.0.0.1", "localhost", "::1"} {
		if err := verify(name); err == nil {
			t.Fatalf("identity provider certificate also verifies for %q", name)
		}
	}
	verifyTLSHandshake(t, roots, pair, nil, address.String(), now)
}

func TestGeneratedRealmImportHasOnlyAScopedServiceAccount(t *testing.T) {
	root, _ := generateTestFixture(t)
	directory := filepath.Join(root, "identity-provider")
	secret := readFixtureText(t, directory, "admin-client-secret")
	if decoded, err := hex.DecodeString(secret); err != nil || len(decoded) != 32 {
		t.Fatal("admin client secret is not 32 random bytes in hex")
	}
	if got := readFixtureText(t, directory, "admin-client-id"); got != "pm-test-admin" {
		t.Fatalf("admin client id = %q, want pm-test-admin", got)
	}
	contents, err := os.ReadFile(filepath.Join(directory, "realm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var realm struct {
		Realm       string `json:"realm"`
		Enabled     bool   `json:"enabled"`
		SSLRequired string `json:"sslRequired"`
		Clients     []struct {
			ClientID                  string   `json:"clientId"`
			PublicClient              bool     `json:"publicClient"`
			Secret                    string   `json:"secret"`
			ServiceAccountsEnabled    bool     `json:"serviceAccountsEnabled"`
			StandardFlowEnabled       bool     `json:"standardFlowEnabled"`
			ImplicitFlowEnabled       bool     `json:"implicitFlowEnabled"`
			DirectAccessGrantsEnabled bool     `json:"directAccessGrantsEnabled"`
			RedirectURIs              []string `json:"redirectUris"`
		} `json:"clients"`
		Users []map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(contents, &realm); err != nil {
		t.Fatalf("decode realm import: %v", err)
	}
	if realm.Realm != "pm-test" || !realm.Enabled || realm.SSLRequired != "all" {
		t.Fatalf("realm = %q enabled=%v ssl=%q, want enabled test realm pm-test with TLS required", realm.Realm, realm.Enabled, realm.SSLRequired)
	}
	if len(realm.Clients) != 1 {
		t.Fatalf("realm import has %d clients, want only the admin client", len(realm.Clients))
	}
	client := realm.Clients[0]
	if client.ClientID != "pm-test-admin" || client.PublicClient || client.Secret != secret ||
		!client.ServiceAccountsEnabled || client.StandardFlowEnabled || client.ImplicitFlowEnabled ||
		client.DirectAccessGrantsEnabled || len(client.RedirectURIs) != 0 {
		t.Fatal("admin client is not a confidential service-account client with every other flow disabled")
	}
	if len(realm.Users) != 1 {
		t.Fatalf("realm import has %d users, want only the service account", len(realm.Users))
	}
	user := realm.Users[0]
	var username, link string
	var clientRoles map[string][]string
	if json.Unmarshal(user["username"], &username) != nil || json.Unmarshal(user["serviceAccountClientId"], &link) != nil ||
		json.Unmarshal(user["clientRoles"], &clientRoles) != nil {
		t.Fatal("service account user is malformed")
	}
	if username != "service-account-pm-test-admin" || link != "pm-test-admin" {
		t.Fatalf("service account = %q linked to %q", username, link)
	}
	if len(clientRoles) != 1 || !slices.Equal(clientRoles["realm-management"], []string{"manage-clients", "manage-users"}) {
		t.Fatalf("service account roles = %v, want only realm-management manage-clients and manage-users", clientRoles)
	}
	for _, key := range []string{"credentials", "realmRoles", "groups"} {
		if _, exists := user[key]; exists {
			t.Fatalf("service account carries %q", key)
		}
	}

	otherRoot, _ := generateTestFixture(t)
	if readFixtureText(t, filepath.Join(otherRoot, "identity-provider"), "admin-client-secret") == secret {
		t.Fatal("two fixtures share one admin client secret")
	}
}

func TestSecondLoopbackAddressSkipsTheFirstAddressAndEdgeHosts(t *testing.T) {
	random := bytes.NewReader([]byte{0, 0, 1, 9, 9, 0, 9, 9, 255, 1, 2, 3})
	address, err := selectSecondLoopbackAddress(random)
	if err != nil {
		t.Fatal(err)
	}
	if address != netip.AddrFrom4([4]byte{127, 1, 2, 3}) {
		t.Fatalf("selected %v, want the first acceptable random address", address)
	}
	if _, err := selectSecondLoopbackAddress(bytes.NewReader([]byte{0, 0})); err == nil {
		t.Fatal("selection accepted a short random source")
	}
}

func readFixtureText(t *testing.T, directory, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(contents)
}
