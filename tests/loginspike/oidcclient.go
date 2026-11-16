package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// providerFacts are the results of the spike's own client checks against one
// identity provider. The spike uses the PM client registration and the PM
// callback as redirect URI, and never sends these codes to PM.
type providerFacts struct {
	subject      string
	hasAuthTime  bool
	authTimeOK   bool
	pkceRequired bool
	pkceEnforced bool
	nonceOK      bool
}

func clientChecks(id string, d *deployment, issuer, clientSecret, user, password string) (providerFacts, bool) {
	facts := providerFacts{}
	ctx := oidc.ClientContext(context.Background(), d.ca.client(nil, true))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, issuer)
	if !report.check(id+".discovery", err == nil, "discovery document at the exact issuer (%v)", errText(err)) {
		return facts, false
	}
	var metadata struct {
		Methods []string `json:"code_challenge_methods_supported"`
		Claims  []string `json:"claims_supported"`
		Prompts []string `json:"prompt_values_supported"`
	}
	_ = provider.Claims(&metadata)
	report.check(id+".discovery.s256", contains(metadata.Methods, "S256"), "discovery lists PKCE method S256 (methods: %s)", strings.Join(metadata.Methods, ","))
	report.INFO(id+".discovery.claims", "auth_time in claims_supported: %t; prompt values: %s", contains(metadata.Claims, "auth_time"), strings.Join(metadata.Prompts, ","))
	config := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  d.redirectURI(),
		Scopes:       []string{oidc.ScopeOpenID},
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})

	// 1. An authorization request without a code challenge.
	query, steps, err := browserLogin(d.ca, config.AuthCodeURL(randomHex(16), oidc.Nonce(randomHex(16))), d.redirectURI(), user, password)
	switch {
	case err != nil:
		report.INFO(id+".pkce.required", "request without a code challenge ended without a callback (steps %s): %v", steps, err)
		facts.pkceRequired = true
	case query.Get("error") != "":
		facts.pkceRequired = true
	}
	report.check(id+".pkce.required", facts.pkceRequired, "the provider refuses an authorization request without a PKCE challenge (callback error %q)", query.Get("error"))

	// 2. A code exchanged with the wrong verifier.
	rightVerifier := oauth2.GenerateVerifier()
	query, steps, err = browserLogin(d.ca, config.AuthCodeURL(randomHex(16), oauth2.S256ChallengeOption(rightVerifier), oidc.Nonce(randomHex(16))), d.redirectURI(), user, password)
	if !report.check(id+".login.headless", err == nil && query.Get("code") != "", "headless login with the per-job user returns a code (steps %s, %v)", steps, errText(err)) {
		return facts, false
	}
	_, err = config.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(oauth2.GenerateVerifier()))
	facts.pkceEnforced = err != nil
	report.check(id+".pkce.enforced", facts.pkceEnforced, "the token endpoint refuses a code with the wrong PKCE verifier")

	// 3. A forced fresh login with the right verifier: nonce and auth_time.
	nonce := randomHex(16)
	requested := time.Now().Add(-2 * time.Second)
	query, steps, err = browserLogin(d.ca, config.AuthCodeURL(randomHex(16),
		oauth2.S256ChallengeOption(rightVerifier), oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("max_age", "0"), oauth2.SetAuthURLParam("prompt", "login")),
		d.redirectURI(), user, password)
	if !report.check(id+".login.reauth", err == nil && query.Get("code") != "", "forced fresh login (prompt=login, max_age=0) returns a code (steps %s, %v)", steps, errText(err)) {
		return facts, false
	}
	token, err := config.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(rightVerifier))
	if !report.check(id+".pkce.exchange", err == nil, "the token endpoint accepts the right verifier and the client secret (%v)", errText(err)) {
		return facts, false
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	report.secret(rawIDToken)
	report.secret(token.AccessToken)
	report.secret(token.RefreshToken)
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if !report.check(id+".idtoken.verify", err == nil, "the ID token verifies against the provider keys, issuer and audience (%v)", errText(err)) {
		return facts, false
	}
	facts.subject = idToken.Subject
	facts.nonceOK = idToken.Nonce == nonce
	report.check(id+".nonce", facts.nonceOK, "the ID token returns the request nonce")
	var claims struct {
		AuthTime json.RawMessage `json:"auth_time"`
	}
	_ = idToken.Claims(&claims)
	facts.hasAuthTime = len(claims.AuthTime) > 0
	report.check(id+".auth_time.present", facts.hasAuthTime, "the ID token has the auth_time claim that PM needs for a re-login and for the credential step-up")
	if facts.hasAuthTime {
		seconds, parseErr := strconv.ParseFloat(strings.Trim(string(claims.AuthTime), `"`), 64)
		facts.authTimeOK = parseErr == nil && time.Unix(int64(seconds), 0).After(requested.Truncate(time.Second).Add(-time.Second))
		report.check(id+".auth_time.fresh", facts.authTimeOK, "auth_time is a number and not older than the forced login request")
	}
	return facts, true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	text := err.Error()
	if len(text) > 200 {
		text = text[:200]
	}
	return report.scrub(fmt.Sprint(text))
}
