package auth

import "github.com/theechofive/maiak/backend/internal/platform/crypto"

// DummyParams exposes the dummy hash parameters to the integration tests.
func (s *Service) DummyParams() crypto.Argon2Params { return s.dummy.Params }

// SetAfterVerify replaces the pause point between the password verification and the session
// transaction of a login.
func (s *Service) SetAfterVerify(f func()) { s.afterVerify = f }
