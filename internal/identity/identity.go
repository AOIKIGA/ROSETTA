// Package identity defines local data namespaces, not authentication.
package identity

import (
	"fmt"
	"regexp"
)

const DefaultUser = "defaultuser"
const DefaultSession = "main"

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Scope struct{ User, Session string }

func Resolve(user, session string) (Scope, error) {
	if user == "" {
		user = DefaultUser
	}
	if session == "" {
		session = DefaultSession
	}
	if !ValidName(user) || !ValidName(session) {
		return Scope{}, fmt.Errorf("user and session must contain 1-64 ASCII letters/digits/_/-")
	}
	return Scope{user, session}, nil
}

func ValidName(name string) bool { return namePattern.MatchString(name) }
