// Package osecure provides simple login service based on OAuth client.
package osecure

import (
	"golang.org/x/oauth2"
)

type TokenVerifier struct {
	IntrospectTokenFunc IntrospectTokenFunc
	GetPermissionsFunc  GetPermissionsFunc
}

type IntrospectTokenFunc func(accessToken string) (subject string, audience string, expireAt int64, extra map[string]interface{}, err error)
type GetPermissionsFunc func(subject string, audience string, token *oauth2.Token) (permissions []string, err error)
