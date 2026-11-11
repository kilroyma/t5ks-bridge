// Package upstream is TECHO5's own claim signing, copied verbatim from
// echod/internal/feature/nearest/auth.go (MIT), used only by the tests to prove
// the bridge's copy signs and checks exactly as the Shows do.
package upstream

import "time"

const AuthHeader, AnsweredHeader = authHeader, answeredHeader

func Sign(word, to string, body []byte, now time.Time) string { return sign(word, to, body, now) }
func Verify(word, me, header string, body []byte, now time.Time) error {
	return verify(word, me, header, body, now)
}
func AnsweredMAC(word, nonce, from, by string) string { return answeredMAC(word, nonce, from, by) }
func NonceOf(header string) string                      { return nonceOf(header) }
func AnsweredBy(word, header, proof, from, by string) bool {
	return answeredBy(word, header, proof, from, by)
}
