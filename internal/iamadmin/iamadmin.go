// Package iamadmin drives a cluster IAM's administration surface (mycel_iam 0.2, cluster mode) as a signed-in account
// administrator, the way its own admin pages do: sign in through the realm's identity provider, read the account, its
// sites and its service accounts, and post the same actions (create_service, add_service_key, grant). It also checks a
// service principal's private_key_jwt credential at the token endpoint. Dev profile only (SPEC.md Appendix C.13).
package iamadmin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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

const adminPath = "/m/mycel_iam/admin"

// Client is a signed-in session with one cluster IAM.
type Client struct {
	Origin  string // e.g. http://iam.localhost:8095
	Account string
	http    *http.Client
}

// localJar keeps Secure cookies of *.localhost over plain http, as browsers do (localhost is a secure context); the
// standard jar would drop them, and the IAM and Keycloak mark their session cookies Secure.
type localJar struct{ j *cookiejar.Jar }

func asSecure(u *url.URL) *url.URL {
	h := u.Hostname()
	if u.Scheme == "http" && (h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "127.0.0.1") {
		c := *u
		c.Scheme = "https"
		return &c
	}
	return u
}
func (l localJar) SetCookies(u *url.URL, cs []*http.Cookie) { l.j.SetCookies(asSecure(u), cs) }
func (l localJar) Cookies(u *url.URL) []*http.Cookie        { return l.j.Cookies(asSecure(u)) }

var formAction = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// SignIn opens the IAM admin pages and signs in at the identity provider's login form with user and password. It
// returns once the IAM has issued its session.
func SignIn(origin, user, password string) (*Client, error) {
	j, _ := cookiejar.New(nil)
	c := &Client{Origin: strings.TrimRight(origin, "/"), http: &http.Client{Jar: localJar{j}, Timeout: 30 * time.Second}}
	body, final, err := c.get(c.Origin+adminPath, "text/html")
	if err != nil {
		return nil, err
	}
	m := formAction.FindStringSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("no sign-in form at %s (is the identity provider up?)", final)
	}
	form := url.Values{"username": {user}, "password": {password}, "credentialId": {""}}
	resp, err := c.http.PostForm(html.UnescapeString(m[1]), form)
	if err != nil {
		return nil, fmt.Errorf("sign in: %w", err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if !strings.HasPrefix(resp.Request.URL.String(), c.Origin) {
		return nil, fmt.Errorf("sign in as %s was refused at %s (%d): %s", user, resp.Request.URL.Host, resp.StatusCode, pageText(string(b)))
	}
	var caps struct {
		AccountID string `json:"account_id"`
	}
	if err := c.getJSON("/capabilities", &caps); err != nil {
		return nil, err
	}
	if caps.AccountID == "" {
		return nil, fmt.Errorf("%s is signed in but administers no IAM account", user)
	}
	c.Account = caps.AccountID
	return c, nil
}

// Do sends a request with the session's cookies. Other apps of the cluster accept the IAM session, signing in to
// their host on first use through the redirects the client follows.
func (c *Client) Do(req *http.Request) (*http.Response, error) { return c.http.Do(req) }

func (c *Client) get(u, accept string) (string, string, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Accept", accept)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(b), resp.Request.URL.String(), nil
}

func (c *Client) getJSON(p string, out any) error {
	u := c.Origin + adminPath + p
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.Request.URL.Path != adminPath+strings.SplitN(p, "?", 2)[0] || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("GET %s: not signed in", p)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", p, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, out)
}

// Account is the parts of the admin account view the tool uses.
type Account struct {
	Account struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	} `json:"account"`
	People []struct {
		ID     string `json:"id"`
		Email  string `json:"email"`
		Status string `json:"status"`
	} `json:"people"`
	Sites []string `json:"sites"`
}

// Account reads the signed-in administrator's account.
func (c *Client) ReadAccount() (*Account, error) {
	var a Account
	return &a, c.getJSON("/account?a="+url.QueryEscape(c.Account), &a)
}

// Service is one service account.
type Service struct {
	ID       string `json:"id"`
	ClientID string `json:"client_id"`
	State    string `json:"state"`
	Keys     []struct {
		Thumbprint string `json:"thumbprint"`
	} `json:"keys"`
}

// Services lists the account's service accounts.
func (c *Client) Services() ([]Service, error) {
	var out struct {
		ServiceAccounts []Service `json:"service_accounts"`
	}
	return out.ServiceAccounts, c.getJSON("/services?a="+url.QueryEscape(c.Account), &out)
}

// Site is one application's access view: its catalog roles and who holds them.
type Site struct {
	Roles []struct {
		Code        string   `json:"code"`
		Permissions []string `json:"permissions"`
		People      []struct {
			ID string `json:"id"`
		} `json:"people"`
	} `json:"roles"`
}

// ReadSite reads one application's access view.
func (c *Client) ReadSite(app string) (*Site, error) {
	var s Site
	return &s, c.getJSON("/site?a="+url.QueryEscape(c.Account)+"&s="+url.QueryEscape(app), &s)
}

// ErrRefused is an action the IAM answered with a refusal page.
var ErrRefused = errors.New("refused")

// Act posts one administrative action, as the admin pages do: form fields, a fresh operation id and the account's
// current revision. site names the application for a site action ("" for an account action).
func (c *Client) Act(site, op string, fields url.Values) (string, error) {
	a, err := c.ReadAccount()
	if err != nil {
		return "", err
	}
	form := url.Values{}
	for k, v := range fields {
		form[k] = v
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	form.Set("op", op)
	form.Set("operation_id", "op_"+hex.EncodeToString(nonce))
	form.Set("expected_account_version", a.Account.Revision)
	target := c.Origin + adminPath + "/account?a=" + url.QueryEscape(c.Account)
	if site != "" {
		target = c.Origin + adminPath + "/site?a=" + url.QueryEscape(c.Account) + "&s=" + url.QueryEscape(site)
	}
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Origin", c.Origin) // the admin pages' same-origin check
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := pageText(string(b))
	if resp.StatusCode != http.StatusOK || strings.Contains(text, "Request refused") {
		return text, fmt.Errorf("%s: %w (%d): %s", op, ErrRefused, resp.StatusCode, text)
	}
	return text, nil
}

var tags = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)

// pageText is an admin page's readable text, without the title, navigation and footer noise.
func pageText(b string) string {
	t := strings.Join(strings.Fields(html.UnescapeString(tags.ReplaceAllString(b, " "))), " ")
	if i := strings.Index(t, " Signed in as "); i > 0 {
		t = t[:i]
	}
	if len(t) > 300 {
		t = t[:300]
	}
	return t
}

// ---- keys and the token endpoint ----

// NewKey returns a new P-256 private key as PKCS#8 PEM.
func NewKey() ([]byte, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, errors.New("private key is not PEM")
	}
	if k, err := x509.ParseECPrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	v, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	k, ok := v.(*ecdsa.PrivateKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("private key is not P-256")
	}
	return k, nil
}

// Thumbprint is the key's RFC 7638 JWK thumbprint (base64url SHA-256 of the canonical P-256 JWK), as the IAM lists
// a service account's keys and as the worker names its key (kid).
func Thumbprint(keyPEM []byte) (string, error) {
	k, err := parseKey(keyPEM)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	x, y := enc.EncodeToString(k.X.FillBytes(make([]byte, 32))), enc.EncodeToString(k.Y.FillBytes(make([]byte, 32)))
	sum := sha256.Sum256([]byte(fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)))
	return enc.EncodeToString(sum[:]), nil
}

// PublicPEM is the public half of a PEM private key, as the IAM's create_service and add_service_key take it.
func PublicPEM(keyPEM []byte) (string, error) {
	k, err := parseKey(keyPEM)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))), nil
}

// TokenCheck asks the IAM token endpoint (tokenBase, e.g. http://iam.localhost:8095) for a service token for audience
// with a private_key_jwt client assertion whose audience is issuer's token endpoint. It returns nil when a token is
// issued, and the endpoint's error code otherwise (for example invalid_client or invalid_target).
func TokenCheck(tokenBase, issuer, clientID, audience string, keyPEM []byte) error {
	k, err := parseKey(keyPEM)
	if err != nil {
		return err
	}
	enc := base64.RawURLEncoding
	x, y := enc.EncodeToString(k.X.FillBytes(make([]byte, 32))), enc.EncodeToString(k.Y.FillBytes(make([]byte, 32)))
	sum := sha256.Sum256([]byte(fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)))
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	now := time.Now()
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": enc.EncodeToString(sum[:])})
	claims, _ := json.Marshal(map[string]any{"iss": clientID, "sub": clientID, "aud": strings.TrimRight(issuer, "/") + "/m/mycel_iam/oauth/token",
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": hex.EncodeToString(nonce)})
	input := enc.EncodeToString(hdr) + "." + enc.EncodeToString(claims)
	d := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, k, d[:])
	if err != nil {
		return err
	}
	assertion := input + "." + enc.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"}, "client_assertion": {assertion}, "audience": {audience}}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).PostForm(strings.TrimRight(tokenBase, "/")+"/m/mycel_iam/oauth/token", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode == http.StatusOK && out.AccessToken != "" {
		return nil
	}
	return fmt.Errorf("token endpoint answered %d %s", resp.StatusCode, or(out.Error, "without a token"))
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
