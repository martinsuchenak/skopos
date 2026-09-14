package routes

import "github.com/martinsuchenak/skopos/internal/auth"

func testAuth(key string) *auth.Authenticator { return auth.NewAuthenticator(key, nil) }
