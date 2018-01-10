// Package osecure provides simple login service based on OAuth client.
package osecure

import (
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"
	"net/http"
	"sort"
	"time"

	"strings"
)

var (
	ErrorInvalidSession                   = errors.New("invalid session")
	ErrorInvalidAuthorizationHeaderFormat = errors.New("invalid authorization header format")
	ErrorUnsupportedAuthorizationType     = errors.New("unsupported authorization type")
	ErrorInvalidAudience                  = errors.New("invalid audience (a.k.a. client ID)")
	ErrorInvalidSubject                   = errors.New("invalid subject (a.k.a. user ID)")
)

var (
	SessionExpireTime    = 86400
	PermissionExpireTime = 600
)

func init() {
	//gob.Register(&time.Time{})
	gob.Register(&AuthSessionCookieData{})
}

type AuthSessionData struct {
	Subject  string //
	Audience string //
	*AuthSessionCookieData
}

type AuthSessionCookieData struct {
	//Subject             string
	//Audience            string
	Token               *oauth2.Token
	Permissions         []string
	PermissionsExpireAt time.Time
}

//func newAuthSessionCookieData(subject string, audience string, token *oauth2.Token) *AuthSessionCookieData {
func newAuthSessionCookieData(token *oauth2.Token) *AuthSessionCookieData {
	if token.Expiry.IsZero() {
		token.Expiry = time.Now().Add(time.Duration(SessionExpireTime) * time.Second)
	}
	return &AuthSessionCookieData{
		//Subject:             subject,
		//Audience:            audience,
		Token:               token,
		Permissions:         []string{},
		PermissionsExpireAt: time.Time{}, // Zero time
	}
}

func (cookieData *AuthSessionCookieData) isTokenExpired() bool {
	return cookieData.Token.Expiry.Before(time.Now())
}

func (cookieData *AuthSessionCookieData) isPermissionsExpired() bool {
	return cookieData.PermissionsExpireAt.Before(time.Now())
}

// CookieConfig is a config of github.com/gorilla/securecookie. Recommended
// configurations are base64 of 64 bytes key for SigningKey, and base64 of 32
// bytes key for EncryptionKey.
type CookieConfig struct {
	SigningKey    string `yaml:"signing_key" env:"skey"`
	EncryptionKey string `yaml:"encryption_key" env:"ekey"`
}

// OAuthConfig is a config of osecure.
type OAuthConfig struct {
	ClientID                 string   `yaml:"client_id" env:"client_id"`
	ClientSecret             string   `yaml:"client_secret" env:"client_secret"`
	Scopes                   []string `yaml:"scopes" env:"scopes"`
	AuthURL                  string   `yaml:"auth_url" env:"auth_url"`
	TokenURL                 string   `yaml:"token_url" env:"token_url"`
	ServerTokenURL           string   `yaml:"server_token_url" env:"server_token_url"`
	ServerTokenEncryptionKey string   `yaml:"server_token_encryption_key" env:"server_token_encryption_key"`
}

type OAuthSession struct {
	name                     string
	cookieStore              *sessions.CookieStore
	client                   *oauth2.Config
	tokenVerifier            *TokenVerifier
	serverTokenURL           string
	serverTokenEncryptionKey []byte
}

// NewOAuthSession creates osecure session.
func NewOAuthSession(name string, cookieConf *CookieConfig, oauthConf *OAuthConfig, tokenVerifier *TokenVerifier, callbackURL string) *OAuthSession {
	client := &oauth2.Config{
		ClientID:     oauthConf.ClientID,
		ClientSecret: oauthConf.ClientSecret,
		Scopes:       oauthConf.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  oauthConf.AuthURL,
			TokenURL: oauthConf.TokenURL,
		},
		RedirectURL: callbackURL,
	}

	serverTokenEncryptionKey, err := hex.DecodeString(oauthConf.ServerTokenEncryptionKey)
	if err != nil {
		panic(err)
	}

	return &OAuthSession{
		name:                     name,
		cookieStore:              newCookieStore(cookieConf),
		client:                   client,
		tokenVerifier:            tokenVerifier,
		serverTokenURL:           oauthConf.ServerTokenURL,
		serverTokenEncryptionKey: serverTokenEncryptionKey,
	}
}

// Secured is a http middleware to check if the current user has logged in.
func (s *OAuthSession) Secured(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthorized(w, r) {
			s.startOAuth(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}
}

// ExpireSession is a http function to log out the user.
func (s *OAuthSession) ExpireSession(redirect string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.expireAuthCookie(w, r)
		http.Redirect(w, r, redirect, 303)
	}
}

func (s *OAuthSession) isAuthorized(w http.ResponseWriter, r *http.Request) bool {
	data, isTokenFromAuthorizationHeader, err := s.getAuthSessionDataFromRequest(r)
	if err != nil {
		return false
	}
	if data == nil || data.isTokenExpired() {
		return false
	}

	if isTokenFromAuthorizationHeader {
		err = s.issueAuthCookie(w, r, data.AuthSessionCookieData)
		if err != nil {
			return false
		}
	}

	return true
}

// HasPermission checks if the current user has such permission.
func (s *OAuthSession) HasPermission(w http.ResponseWriter, r *http.Request, permission string) bool {
	perms, err := s.GetPermissions(w, r)
	if err != nil {
		return false
	}

	id := sort.SearchStrings(perms, permission)
	result := id < len(perms) && perms[id] == permission

	return result
}

// GetPermissions lists the permissions of the current user and client.
func (s *OAuthSession) GetPermissions(w http.ResponseWriter, r *http.Request) ([]string, error) {
	data, isTokenFromAuthorizationHeader, err := s.getAuthSessionDataFromRequest(r)
	if err != nil {
		return nil, err
	}
	if data == nil || data.isTokenExpired() {
		return nil, ErrorInvalidSession
	}

	isPermissionUpdated, err := s.ensurePermUpdated(w, r, data)
	if err != nil {
		return nil, err
	}

	if isTokenFromAuthorizationHeader || isPermissionUpdated {
		err = s.issueAuthCookie(w, r, data.AuthSessionCookieData)
		if err != nil {
			return nil, err
		}
	}

	return data.Permissions, nil
}

func (s *OAuthSession) ensurePermUpdated(w http.ResponseWriter, r *http.Request, data *AuthSessionData) (bool, error) {
	if !data.isPermissionsExpired() {
		return false, nil
	}

	permissions, err := s.tokenVerifier.GetPermissionsFunc(data.Subject, data.Audience, data.Token)
	if err != nil {
		return false, err
	}

	data.Permissions = permissions
	data.PermissionsExpireAt = time.Now().Add(time.Duration(PermissionExpireTime) * time.Second)

	// Sort the string, as sort.SearchStrings needs sorted []string.
	sort.Strings(data.Permissions)

	return true, nil
}

func (s *OAuthSession) GetSessionData(w http.ResponseWriter, r *http.Request) (*AuthSessionData, error) {
	data, _, err := s.getAuthSessionDataFromRequest(r)
	if err != nil {
		return nil, err
	}
	if data == nil || data.isTokenExpired() {
		return nil, ErrorInvalidSession
	}

	return data, nil
}

func (s *OAuthSession) getAuthSessionDataFromRequest(r *http.Request) (*AuthSessionData, bool, error) {
	var accessToken string
	var isTokenFromAuthorizationHeader bool

	cookieData := s.retrieveAuthCookie(r)
	if cookieData == nil || cookieData.isTokenExpired() {
		var err error
		accessToken, err = s.getBearerToken(r)
		if err != nil {
			return nil, false, err
		}

		isTokenFromAuthorizationHeader = true
	} else {
		accessToken = cookieData.Token.AccessToken
		isTokenFromAuthorizationHeader = false
	}

	subject, audience, expireAt, extra, err := s.tokenVerifier.IntrospectTokenFunc(accessToken)
	if err != nil {
		return nil, false, err
	}

	if isTokenFromAuthorizationHeader {
		token := makeBearerToken(accessToken, expireAt).WithExtra(extra)
		cookieData = newAuthSessionCookieData(token)
	}

	data := &AuthSessionData{
		Subject:               subject,
		Audience:              audience,
		AuthSessionCookieData: cookieData,
	}

	if data.Audience != s.client.ClientID {
		return nil, false, ErrorInvalidAudience
	}

	return data, isTokenFromAuthorizationHeader, nil
}

/*
func (s *OAuthSession) getAuthSessionDataFromRequest(r *http.Request) (*AuthSessionData, bool, error) {
	var isTokenFromAuthorizationHeader bool

	cookieData := s.retrieveAuthCookie(r)
	if cookieData == nil || cookieData.isTokenExpired() {
		subject, audience, token, err := s.getAndIntrospectBearerToken(r)
		if err != nil {
			return nil, false, err
		}

		cookieData = newAuthSessionCookieData(subject, audience, token)

		isTokenFromAuthorizationHeader = true
	} else {
		isTokenFromAuthorizationHeader = false
	}

	data := &AuthSessionData{
		AuthSessionCookieData: cookieData,
	}

	if data.Audience != s.client.ClientID {
		return nil, false, ErrorInvalidAudience
	}

	return data, isTokenFromAuthorizationHeader, nil
}

func (s *OAuthSession) getAndIntrospectBearerToken(r *http.Request) (subject string, audience string, token *oauth2.Token, err error) {
	var bearerToken string
	bearerToken, err = s.getBearerToken(r)
	if err != nil {
		return
	}

	var expireAt int64
	var extra map[string]interface{}
	subject, audience, expireAt, extra, err = s.tokenVerifier.IntrospectTokenFunc(bearerToken)
	if err != nil {
		return
	}

	token = makeBearerToken(bearerToken, expireAt).WithExtra(extra)
	return
}
*/

func (s *OAuthSession) startOAuth(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.client.AuthCodeURL(r.RequestURI), 303)
}

// CallbackView is a http handler for the authentication redirection of the
// auth server.
func (s *OAuthSession) CallbackView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	cont := q.Get("state")

	token, err := s.client.Exchange(oauth2.NoContext, code)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// TODO: how to get subject (account ID) when using exchange code only?
	/*subject, audience, _, _, err := s.tokenVerifier.IntrospectTokenFunc(token.AccessToken)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}*/

	//err = s.issueAuthCookie(w, r, newAuthSessionCookieData(subject, audience, token))
	err = s.issueAuthCookie(w, r, newAuthSessionCookieData(token))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	http.Redirect(w, r, cont, 303)
}

func makeToken(tokenType string, accessToken string, expireAt int64) *oauth2.Token {
	return &oauth2.Token{
		AccessToken: accessToken,
		TokenType:   tokenType,
		Expiry:      time.Unix(expireAt, 0),
	}
}

func makeBearerToken(accessToken string, expireAt int64) *oauth2.Token {
	return makeToken("Bearer", accessToken, expireAt)
}

func (s *OAuthSession) getBearerToken(r *http.Request) (string, error) {
	authorizationHeaderValue := r.Header.Get("Authorization")

	authorizationData := strings.SplitN(authorizationHeaderValue, " ", 2)
	if len(authorizationData) != 2 {
		return "", ErrorInvalidAuthorizationHeaderFormat
	}

	tokenType := authorizationData[0]
	if !strings.EqualFold(tokenType, "bearer") {
		return "", ErrorUnsupportedAuthorizationType
	}

	bearerToken := authorizationData[1]
	return bearerToken, nil
}

func (s *OAuthSession) retrieveAuthCookie(r *http.Request) *AuthSessionCookieData {
	session, err := s.cookieStore.Get(r, s.name)
	if err != nil {
		return nil
	}

	v, found := session.Values["data"]
	if !found {
		return nil
	}

	cookieData, ok := v.(*AuthSessionCookieData)
	if !ok {
		return nil
	}

	return cookieData
}

func (s *OAuthSession) issueAuthCookie(w http.ResponseWriter, r *http.Request, cookieData *AuthSessionCookieData) error {
	session, err := s.cookieStore.New(r, s.name)
	if err != nil {
		return err
	}
	session.Values["data"] = cookieData
	err = session.Save(r, w)
	return err
}

func (s *OAuthSession) expireAuthCookie(w http.ResponseWriter, r *http.Request) {
	session, err := s.cookieStore.Get(r, s.name)
	if err != nil {
		panic(err)
	}
	delete(session.Values, "data")
	session.Options.MaxAge = -1
	session.Save(r, w)
}

func newCookieStore(conf *CookieConfig) *sessions.CookieStore {

	var signingKey, encryptionKey []byte
	var err error

	if conf != nil {
		signingKey, err = base64.StdEncoding.DecodeString(conf.SigningKey)
		if err != nil {
			panic(err)
		}

		encryptionKey, err = base64.StdEncoding.DecodeString(conf.EncryptionKey)
		if err != nil {
			panic(err)
		}
	} else {
		signingKey = securecookie.GenerateRandomKey(64)
		encryptionKey = securecookie.GenerateRandomKey(32)
	}

	return sessions.NewCookieStore(signingKey, encryptionKey)
}
