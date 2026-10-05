package auth

import (
	"crypto/subtle"

	"golang.org/x/crypto/argon2"
)

// dummyPassword is an obviously fake value used only to give unknown-user
// lookups the same Argon2id cost as a real verification. It authenticates
// nothing; DummyVerify discards its result.
const dummyPassword = "vibeshell-dummy-password-not-a-real-secret"

// Verifier verifies passwords against encoded Argon2id hashes in constant
// time with respect to the hash contents and performs equal dummy work for
// unknown users so a missing username does not visibly skip the expensive
// step (PLAN 4.2).
type Verifier struct {
	dummy EncodedHash
}

// NewVerifier builds a Verifier whose dummy hash costs the same as hashes
// with params and salt of the same lengths.
func NewVerifier(params Params, salt []byte) (*Verifier, error) {
	dummy, err := NewEncodedHash([]byte(dummyPassword), params, salt)
	if err != nil {
		return nil, err
	}
	return &Verifier{dummy: dummy}, nil
}

// Verify reports whether password matches the encoded hash. The derived key
// is compared in constant time; the Argon2id cost dominates the comparison.
func (v *Verifier) Verify(encoded EncodedHash, password []byte) bool {
	derived := argon2.IDKey(password, encoded.Salt,
		encoded.Params.Iterations, encoded.Params.MemoryKiB, encoded.Params.Parallelism,
		uint32(len(encoded.Hash)))
	return subtle.ConstantTimeCompare(derived, encoded.Hash) == 1
}

// DummyVerify runs the same work as Verify for an unknown user and returns
// nothing: callers must treat every unknown-user attempt as a failure
// regardless of the password supplied.
func (v *Verifier) DummyVerify(password []byte) {
	v.Verify(v.dummy, password)
}
