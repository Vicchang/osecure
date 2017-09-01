// Package osecure provides simple login service based on OAuth client.
package osecure

import (
	"time"

	"golang.org/x/oauth2"
)

type TokenVerifier struct {
	IntrospectTokenFunc IntrospectTokenFunc
	GetPermissionsFunc  GetPermissionsFunc
	//IsSubjectGrantedFunc IsSubjectGrantedFunc
}

type IntrospectTokenFunc func(accessToken string) (subject string, token *oauth2.Token, err error)
type GetPermissionsFunc func(subject string, token *oauth2.Token) (permissions []string, err error)

//type IsSubjectGrantedFunc func(subject string) (bool, error)

func MakeToken(tokenType string, accessToken string, expireAt int64) *oauth2.Token {
	return &oauth2.Token{
		AccessToken: accessToken,
		TokenType:   tokenType,
		Expiry:      time.Unix(expireAt, 0),
	}
}

func MakeBearerToken(accessToken string, expireAt int64) *oauth2.Token {
	return MakeToken("Bearer", accessToken, expireAt)
}
