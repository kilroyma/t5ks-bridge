package main

// Signing and checking TECHO5 "nearest device" claims.
//
// Copied from TECHO5's echod/internal/feature/nearest/auth.go (MIT, (c) HuskerMinion), so a claim this
// bridge signs is exactly one a TECHO5 device signs, and the other way round. Keep it in step with
// upstream: if TECHO5 moves past "v2", the Shows will refuse this bridge's claims (it fails open:
// everything answers as it did without arbitration) until this file is updated.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	t5AuthHeader     = "X-Techo5-Nearest"
	t5AnsweredHeader = "X-Techo5-Nearest-Answered"
	t5Version        = "v2"
	t5KeySalt        = "techo5-nearest v2"
	t5KeyRounds      = 100_000
	t5Skew           = 5 * time.Second
	t5NoncesKept     = 1024
	t5MaxBody        = 1 << 12
)

var t5Keys struct {
	sync.Mutex
	word string
	key  []byte
}

// t5Key is the HMAC key for a house word (PBKDF2, slow by design), worked out once.
func t5Key(word string) []byte {
	t5Keys.Lock()
	defer t5Keys.Unlock()
	if t5Keys.key != nil && t5Keys.word == word {
		return t5Keys.key
	}
	k, err := pbkdf2.Key(sha256.New, word, []byte(t5KeySalt), t5KeyRounds, sha256.Size)
	if err != nil {
		k = nil
	}
	t5Keys.word, t5Keys.key = word, k
	return k
}

func t5MAC(word, to, ts, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, t5Key(word))
	m.Write([]byte(t5Version + "\n" + ts + "\n" + nonce + "\n" + strings.ToLower(to) + "\n" + hex.EncodeToString(sum[:]) + "\n"))
	return m.Sum(nil)
}

// t5Sign is the header for a claim about to be sent to the device called to.
func t5Sign(word, to string, body []byte, now time.Time) string {
	var n [12]byte
	_, _ = rand.Read(n[:])
	ts, nonce := strconv.FormatInt(now.Unix(), 10), hex.EncodeToString(n[:])
	return t5Version + " " + ts + " " + nonce + " " + hex.EncodeToString(t5MAC(word, to, ts, nonce, body))
}

var (
	t5Started   = time.Now()
	errUnsigned = errors.New("not signed by this house")
	errStale    = errors.New("signed at a time too far from this clock")
	errReplayed = errors.New("a claim already taken")
)

func t5StartedWall(now time.Time) time.Time {
	return now.Add(-time.Since(t5Started)).Truncate(time.Second)
}

func t5LooksSigned(header string, now time.Time) error {
	parts := strings.Fields(header)
	if len(parts) != 4 || parts[0] != t5Version || len(parts[3]) != 2*sha256.Size {
		return errUnsigned
	}
	sec, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errUnsigned
	}
	at := time.Unix(sec, 0)
	if d := now.Sub(at); d > t5Skew || d < -t5Skew || at.Before(t5StartedWall(now)) {
		return errStale
	}
	return nil
}

// t5Verify checks a claim's header against the house word, as a claim for the device called me.
func t5Verify(word, me, header string, body []byte, now time.Time) error {
	if word == "" {
		return errUnsigned
	}
	if err := t5LooksSigned(header, now); err != nil {
		return err
	}
	parts := strings.Fields(header)
	got, err := hex.DecodeString(parts[3])
	if err != nil || !hmac.Equal(got, t5MAC(word, me, parts[1], parts[2], body)) {
		return errUnsigned
	}
	if !t5Fresh(parts[2], now) {
		return errReplayed
	}
	return nil
}

var t5Nonces struct {
	sync.Mutex
	seen map[string]time.Time
}

func t5Fresh(nonce string, now time.Time) bool {
	t5Nonces.Lock()
	defer t5Nonces.Unlock()
	if t5Nonces.seen == nil {
		t5Nonces.seen = map[string]time.Time{}
	}
	if _, ok := t5Nonces.seen[nonce]; ok {
		return false
	}
	if len(t5Nonces.seen) >= t5NoncesKept {
		for k, at := range t5Nonces.seen {
			if now.Sub(at) > 2*t5Skew {
				delete(t5Nonces.seen, k)
			}
		}
		if len(t5Nonces.seen) >= t5NoncesKept {
			return false
		}
	}
	t5Nonces.seen[nonce] = now
	return true
}

func t5ReadBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, t5MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > t5MaxBody {
		return nil, errors.New("too long for a claim")
	}
	return b, nil
}

// t5AnsweredMAC proves a "answered here already" refusal comes from a device of this house.
func t5AnsweredMAC(word, nonce, from, by string) string {
	m := hmac.New(sha256.New, t5Key(word))
	m.Write([]byte("answered\n" + nonce + "\n" + strings.ToLower(from) + "\n" + strings.ToLower(by) + "\n"))
	return hex.EncodeToString(m.Sum(nil))
}

func t5NonceOf(header string) string {
	if parts := strings.Fields(header); len(parts) == 4 {
		return parts[2]
	}
	return ""
}

func t5AnsweredBy(word, header, proof, from, by string) bool {
	nonce := t5NonceOf(header)
	if word == "" || nonce == "" || proof == "" {
		return false
	}
	return hmac.Equal([]byte(proof), []byte(t5AnsweredMAC(word, nonce, from, by)))
}
