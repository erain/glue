package auth

import "errors"

// ErrNoAuthFile is returned by LoadTokens when no auth.json was found
// at any of the configured locations. Callers should print a clear
// message instructing the user to run "codex login".
var ErrNoAuthFile = errors.New("auth: no codex auth.json found (run `codex login` first)")

// ErrMalformedAuthFile is returned when auth.json is missing required
// fields (specifically the tokens object).
var ErrMalformedAuthFile = errors.New("auth: auth.json missing tokens object")

// ErrRefreshPermanent is returned by Refresh when the upstream
// classifies the refresh-token as no-longer-valid. The user must run
// `codex login` again. Errors wrapping this sentinel are also
// permanent.
var ErrRefreshPermanent = errors.New("auth: refresh token no longer valid")

// IsPermanentRefreshFailure reports whether err is or wraps
// ErrRefreshPermanent.
func IsPermanentRefreshFailure(err error) bool {
	return errors.Is(err, ErrRefreshPermanent)
}
