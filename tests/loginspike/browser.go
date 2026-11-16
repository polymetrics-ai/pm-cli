package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	formPattern  = regexp.MustCompile(`(?is)<form\b([^>]*)>(.*?)</form>`)
	inputPattern = regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	attrPattern  = regexp.MustCompile(`(?is)([a-z_:-]+)\s*=\s*("([^"]*)"|'([^']*)')`)
)

func attributes(tag string) map[string]string {
	values := map[string]string{}
	for _, match := range attrPattern.FindAllStringSubmatch(tag, -1) {
		value := match[3]
		if match[4] != "" {
			value = match[4]
		}
		values[strings.ToLower(match[1])] = html.UnescapeString(value)
	}
	return values
}

// loginForm finds the first form that has a password input. It returns the
// absolute form action and the hidden fields.
func loginForm(base *url.URL, body []byte) (string, url.Values, []string, error) {
	for _, form := range formPattern.FindAllSubmatch(body, -1) {
		if !bytes.Contains(bytes.ToLower(form[2]), []byte(`type="password"`)) &&
			!bytes.Contains(bytes.ToLower(form[2]), []byte(`name="password"`)) {
			continue
		}
		action := attributes(string(form[1]))["action"]
		target, err := base.Parse(action)
		if err != nil || action == "" {
			target = base
		}
		hidden := url.Values{}
		names := []string{}
		for _, input := range inputPattern.FindAllStringSubmatch(string(form[2]), -1) {
			fields := attributes(input[1])
			if fields["name"] == "" {
				continue
			}
			names = append(names, fields["name"])
			if strings.EqualFold(fields["type"], "hidden") {
				hidden.Set(fields["name"], fields["value"])
			}
		}
		return target.String(), hidden, names, nil
	}
	return "", nil, nil, errors.New("no login form with a password field")
}

// browserLogin acts as the browser at the identity provider. It opens the
// authorization URL with a new cookie jar, fills the provider's own login form
// with the per-job user and password, and stops at the redirect to the PM
// callback. It returns the callback query (code and state, or an error).
func browserLogin(ca *authority, authorizationURL, callbackPrefix, user, password string) (url.Values, string, error) {
	jar, _ := cookiejar.New(nil)
	client := ca.client(jar, true)
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if strings.HasPrefix(request.URL.String(), callbackPrefix) {
			return http.ErrUseLastResponse
		}
		if len(via) > 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	steps := []string{}
	response, err := client.Get(authorizationURL)
	for attempt := 0; attempt < 3; attempt++ {
		if err != nil {
			return nil, strings.Join(steps, ">"), fmt.Errorf("identity provider request failed: %s", report.scrub(err.Error()))
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		_ = response.Body.Close()
		location := response.Header.Get("Location")
		steps = append(steps, fmt.Sprintf("%d", response.StatusCode))
		if response.StatusCode >= 300 && response.StatusCode < 400 && strings.HasPrefix(location, callbackPrefix) {
			target, parseErr := url.Parse(location)
			if parseErr != nil {
				return nil, strings.Join(steps, ">"), errors.New("callback redirect is not a URL")
			}
			query := target.Query()
			for _, name := range []string{"code", "state", "session_state"} {
				report.secret(query.Get(name))
			}
			return query, strings.Join(steps, ">"), nil
		}
		if response.StatusCode != http.StatusOK {
			return nil, strings.Join(steps, ">"), fmt.Errorf("unexpected identity provider status %d", response.StatusCode)
		}
		action, fields, names, formErr := loginForm(response.Request.URL, body)
		if formErr != nil {
			return nil, strings.Join(steps, ">"), formErr
		}
		filled := false
		for _, name := range names {
			switch name {
			case "username", "login", "email":
				fields.Set(name, user)
				filled = true
			case "password":
				fields.Set(name, password)
			}
		}
		if !filled {
			return nil, strings.Join(steps, ">"), fmt.Errorf("login form has no user field (fields: %s)", strings.Join(names, ","))
		}
		steps = append(steps, "form")
		response, err = client.PostForm(action, fields)
	}
	return nil, strings.Join(steps, ">"), errors.New("no callback redirect after the login form")
}

// pmBrowser is the browser side of one PM deployment: a cookie jar on the PM
// origin and the public origin for the Origin header.
type pmBrowser struct {
	origin string
	client *http.Client
	jar    *cookiejar.Jar
}

func newPMBrowser(d *deployment) *pmBrowser {
	jar, _ := cookiejar.New(nil)
	return &pmBrowser{origin: d.origin, client: d.ca.client(jar, false), jar: jar}
}

type pmResponse struct {
	status int
	body   []byte
}

// problemCode returns the PM problem code of an error response, or "".
func (r pmResponse) problemCode() string {
	var problem struct {
		Code  string `json:"code"`
		Title string `json:"title"`
	}
	if json.Unmarshal(r.body, &problem) == nil {
		return problem.Code
	}
	return ""
}

func (b *pmBrowser) post(path string, body any, headers map[string]string) (pmResponse, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return pmResponse{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.origin+path, bytes.NewReader(encoded))
	clear(encoded)
	if err != nil {
		return pmResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", b.origin)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return pmResponse{}, fmt.Errorf("PM request failed: %s", report.scrub(err.Error()))
	}
	defer response.Body.Close()
	contents, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return pmResponse{status: response.StatusCode, body: contents}, nil
}

func (b *pmBrowser) cookie(name string) string {
	target, _ := url.Parse(b.origin)
	for _, cookie := range b.jar.Cookies(target) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

// login runs the PM browser login: start the challenge, log in at the
// identity provider and post the callback code and state to PM.
func (b *pmBrowser) login(d *deployment, user, password string, reauthenticate bool) (string, error) {
	start, err := b.post("/v1/sessions/browser/challenges", map[string]any{"reauthenticate": reauthenticate}, nil)
	if err != nil {
		return "", err
	}
	if start.status != http.StatusCreated {
		return "", fmt.Errorf("challenge status %d code %q", start.status, start.problemCode())
	}
	var challenge struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if json.Unmarshal(start.body, &challenge) != nil || challenge.AuthorizationURL == "" {
		return "", errors.New("challenge has no authorization URL")
	}
	query, steps, err := browserLogin(d.ca, challenge.AuthorizationURL, d.redirectURI(), user, password)
	if err != nil {
		return steps, err
	}
	if query.Get("error") != "" {
		return steps, fmt.Errorf("identity provider returned error %q", query.Get("error"))
	}
	complete, err := b.post("/v1/sessions/browser", map[string]string{"code": query.Get("code"), "state": query.Get("state")}, nil)
	if err != nil {
		return steps, err
	}
	if complete.status != http.StatusNoContent {
		return steps, fmt.Errorf("callback status %d code %q", complete.status, complete.problemCode())
	}
	report.secret(b.cookie("__Host-pm_session"))
	report.secret(b.cookie("__Host-pm_csrf"))
	if b.cookie("__Host-pm_session") == "" || b.cookie("__Host-pm_csrf") == "" {
		return steps, errors.New("PM set no session or CSRF cookie")
	}
	return steps, nil
}

func (b *pmBrowser) approveDevice(challengeID, userCode string) (pmResponse, error) {
	return b.post("/v1/sessions/device/approve",
		map[string]string{"challenge_id": challengeID, "user_code": userCode},
		map[string]string{"X-CSRF-Token": b.cookie("__Host-pm_csrf")})
}

// storeCredential is the request that the browser credential page
// (/admin/credentials, credentials.js) sends. The value goes only into the
// JSON body, base64-encoded, over TLS.
func (b *pmBrowser) storeCredential(workspaceID, connectionID string, value []byte) (pmResponse, error) {
	body := map[string]any{
		"credential_value": base64.StdEncoding.EncodeToString(value),
		"scopes":           []string{"issues:read"},
	}
	path := "/admin/v1/workspaces/" + url.PathEscape(workspaceID) + "/connections/" + url.PathEscape(connectionID) + "/credentials"
	return b.post(path, body, map[string]string{
		"X-CSRF-Token":    b.cookie("__Host-pm_csrf"),
		"Idempotency-Key": "login-spike-" + randomHex(8),
	})
}
