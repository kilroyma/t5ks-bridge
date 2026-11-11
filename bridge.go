package main

// The translation itself. A Kiosk Satellite claim (UDP) becomes a signed TECHO5 claim posted to every
// Show, and a TECHO5 claim posted to this bridge becomes a Kiosk Satellite claim on UDP. Each side then
// decides exactly as it already does, seeing the other side's devices as if they were its own kind.
//
// Like both originals, it fails open: anything that goes wrong here leaves every device answering the
// way it would without arbitration.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Peer is a TECHO5 device found over mDNS.
type Peer struct {
	Name    string
	Address string
	Port    int
	Hears   bool
}

// Config is what the bridge needs to know.
type Config struct {
	HouseWord string            // the TECHO5 house word, which signs every claim
	Name      string            // what the bridge is called among the TECHO5 devices
	OffsetDB  float64           // added to Kiosk Satellite scores on their way to TECHO5, taken off the other way
	Words     map[string]string // TECHO5 wake word id -> Kiosk Satellite phrase, where the default guess is wrong
}

// KSTransport carries Kiosk Satellite datagrams.
type KSTransport interface {
	Broadcast(data []byte)
	SendTo(data []byte, address string)
}

// t5Claim is a TECHO5 claim, as it goes on the wire.
type t5Claim struct {
	From  string  `json:"from"`
	Turn  string  `json:"turn"`
	Word  string  `json:"word,omitempty"`
	Score float64 `json:"score"`
}

const (
	ksKeep       = 2 * time.Second  // a Kiosk Satellite claim is older than any window after this
	ksPeerFor    = 30 * time.Minute // a kiosk not heard from in this long is no longer addressed
	ksHelloEvery = 5 * time.Minute
	t5PostWithin = 500 * time.Millisecond // the longest window on either side; a claim later than this is of no use
	standDownDB  = 999                    // what a Show that already answered claims to the kiosks, so they stand down
)

// Bridge holds the state of both sides.
type Bridge struct {
	cfg    Config
	log    *slog.Logger
	peers  func() []Peer
	ks     KSTransport
	client *http.Client
	after  func(d time.Duration, f func())
	now    func() time.Time

	ksID string // this bridge's own id among the kiosks, for its hellos

	mu     sync.Mutex
	kiosks map[string]time.Time // address -> when last heard
	seen   map[string]time.Time // "id:n" of claims already relayed -> when
	own    map[string]bool      // every id this bridge sends from, so its own broadcasts are not relayed back
}

// NewBridge makes a bridge. peers lists the TECHO5 devices; ks sends to the kiosks.
func NewBridge(cfg Config, log *slog.Logger, peers func() []Peer, ks KSTransport) *Bridge {
	b := &Bridge{
		cfg:   cfg,
		log:   log,
		peers: peers,
		ks:    ks,
		client: &http.Client{
			Timeout: t5PostWithin,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: t5PostWithin}).DialContext,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     10 * time.Minute,
			},
		},
		after:  func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		now:    time.Now,
		kiosks: map[string]time.Time{},
		seen:   map[string]time.Time{},
		own:    map[string]bool{},
	}
	b.ksID = "t5bridge" + randomHex(4)
	b.own[b.ksID] = true
	go t5Key(cfg.HouseWord) // slow on purpose; work it out before the first wake word
	return b
}

// --- wake word names ---------------------------------------------------------------------------

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// canonical is a wake word as one lowercase token: "Okay Nabu", "okay nabu" and "okay_nabu" are all
// "okay_nabu", which is how TECHO5 names it.
func canonical(word string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(word), "_"), "_")
}

// phraseFor is the Kiosk Satellite phrase for a TECHO5 wake word id.
func (b *Bridge) phraseFor(word string) string {
	c := canonical(word)
	for k, v := range b.cfg.Words {
		if canonical(k) == c {
			return normalizePhrase(v)
		}
	}
	return strings.ReplaceAll(c, "_", " ")
}

// wordFor is the TECHO5 wake word id for a Kiosk Satellite phrase.
func (b *Bridge) wordFor(phrase string) string {
	c := canonical(phrase)
	for k, v := range b.cfg.Words {
		if canonical(v) == c {
			return canonical(k)
		}
	}
	return c
}

// normalizePhrase is Kiosk Satellite's own: trimmed, lowercase, single spaces.
func normalizePhrase(p string) string {
	return strings.Join(strings.Fields(strings.ToLower(p)), " ")
}

// --- Kiosk Satellite side ----------------------------------------------------------------------

// ksIDFor is the id a TECHO5 device goes by among the kiosks: the same every time, so a kiosk's log
// reads the same device the same way.
func (b *Bridge) ksIDFor(name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(name)))
	id := "t5" + hex.EncodeToString(sum[:7])
	b.mu.Lock()
	b.own[id] = true
	b.mu.Unlock()
	return id
}

func (b *Bridge) ksHello() []byte {
	data, _ := json.Marshal(map[string]any{"ks": "wake", "v": 1, "hello": true, "id": b.ksID})
	return data
}

func ksClaimPacket(id, phrase string, energy float64) []byte {
	data, _ := json.Marshal(map[string]any{
		"ks": "wake", "v": 1, "id": id,
		"n": mrand.IntN(1 << 30),
		"p": normalizePhrase(phrase),
		"e": round1(energy),
	})
	return data
}

// sendKS sends a claim the way a kiosk does: to the whole subnet and to every kiosk heard from, three
// times, since a broadcast is never acknowledged.
func (b *Bridge) sendKS(packet []byte) {
	send := func() {
		b.ks.Broadcast(packet)
		for _, addr := range b.kioskAddresses() {
			b.ks.SendTo(packet, addr)
		}
	}
	send()
	b.after(15*time.Millisecond, send)
	b.after(30*time.Millisecond, send)
}

// SayHello tells the kiosks the bridge is here, so they address their claims to it.
func (b *Bridge) SayHello() { b.ks.Broadcast(b.ksHello()) }

func (b *Bridge) kioskAddresses() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := make([]string, 0, len(b.kiosks))
	for addr, at := range b.kiosks {
		if now.Sub(at) > ksPeerFor {
			delete(b.kiosks, addr)
			continue
		}
		out = append(out, addr)
	}
	return out
}

// learn remembers a kiosk, and answers whether it was new.
func (b *Bridge) learn(addr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, known := b.kiosks[addr]
	b.kiosks[addr] = b.now()
	return !known
}

// HandleKS takes one datagram from the kiosks' port.
func (b *Bridge) HandleKS(data []byte, from string) {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	id, _ := msg["id"].(string)
	if msg["ks"] != "wake" || msg["v"] != float64(1) || id == "" || len(id) > 64 {
		return
	}
	b.mu.Lock()
	mine := b.own[id]
	b.mu.Unlock()
	if mine {
		return // this bridge hearing its own broadcast
	}
	if msg["hello"] == true {
		if b.learn(from) {
			b.log.Info("kiosk found", "address", from)
			b.ks.SendTo(b.ksHello(), from)
		}
		return
	}
	phrase, ok1 := msg["p"].(string)
	energy, ok2 := msg["e"].(float64)
	n, ok3 := msg["n"].(float64)
	if !ok1 || !ok2 || !ok3 || n != math.Trunc(n) || math.IsInf(energy, 0) || math.IsNaN(energy) {
		return
	}
	if b.learn(from) {
		b.log.Info("kiosk found", "address", from)
	}
	// Each claim arrives several times (broadcast and addressed, three rounds); relay it once.
	key := fmt.Sprintf("%s:%d", id, int64(n))
	b.mu.Lock()
	now := b.now()
	for k, at := range b.seen {
		if now.Sub(at) > ksKeep {
			delete(b.seen, k)
		}
	}
	_, dup := b.seen[key]
	b.seen[key] = now
	b.mu.Unlock()
	if dup {
		return
	}
	go b.relayToT5(normalizePhrase(phrase), energy, from)
}

// --- TECHO5 side ---------------------------------------------------------------------------------

// relayToT5 posts a kiosk's claim to every Show. A Show that had already answered says so, with
// proof; the kiosks are then told it heard the word best, so the kiosk stands down too.
func (b *Bridge) relayToT5(phrase string, energy float64, from string) {
	c := t5Claim{From: b.cfg.Name, Turn: randomHex(8), Word: b.wordFor(phrase), Score: round1(energy + b.cfg.OffsetDB)}
	var peers []Peer
	for _, p := range b.peers() {
		if p.Hears && !strings.EqualFold(p.Name, b.cfg.Name) {
			peers = append(peers, p)
		}
	}
	b.log.Info("kiosk claim -> TECHO5", "kiosk", from, "word", c.Word, "kiosk_db", round1(energy),
		"sent_db", c.Score, "shows", len(peers))
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.postT5(p, c) {
				b.log.Info("show had already answered; telling the kiosks to stand down", "show", p.Name, "word", c.Word)
				b.sendKS(ksClaimPacket(b.ksIDFor(p.Name), phrase, standDownDB))
			}
		}()
	}
	wg.Wait()
}

// postT5 sends a claim to one Show, and answers whether it said, with proof, that it had answered.
func (b *Bridge) postT5(p Peer, c t5Claim) (answeredThere bool) {
	body, err := json.Marshal(c)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), t5PostWithin)
	defer cancel()
	url := "http://" + net.JoinHostPort(p.Address, fmt.Sprint(p.Port)) + "/nearest"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	header := t5Sign(b.cfg.HouseWord, p.Name, body, b.now())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(t5AuthHeader, header)
	resp, err := b.client.Do(req)
	if err != nil {
		b.log.Debug("claim not delivered", "to", p.Name, "err", err)
		return false
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusConflict:
		if t5AnsweredBy(b.cfg.HouseWord, header, resp.Header.Get(t5AnsweredHeader), c.From, p.Name) {
			return true
		}
		b.log.Warn("show said it answered, without proof; ignoring", "show", p.Name)
	case resp.StatusCode == http.StatusForbidden:
		b.log.Warn("show refused the claim: check HOUSE_WORD matches the Shows' house word, and the clocks", "show", p.Name)
	case resp.StatusCode == http.StatusNotFound:
		b.log.Warn("show is not taking claims: turn on Nearest device answers on it", "show", p.Name)
	case resp.StatusCode >= 300:
		b.log.Debug("claim refused", "to", p.Name, "status", resp.Status)
	}
	return false
}

// ServeNearest takes a claim a Show posts to the bridge, and passes it on to the kiosks.
func (b *Bridge) ServeNearest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post a claim", http.StatusMethodNotAllowed)
		return
	}
	now := b.now()
	if err := t5LooksSigned(r.Header.Get(t5AuthHeader), now); err != nil {
		b.log.Warn("a claim that does not check out", "from", r.RemoteAddr, "err", err)
		http.Error(w, "not this house", http.StatusForbidden)
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(2 * time.Second))
	body, err := t5ReadBody(r)
	if err != nil {
		http.Error(w, "not a claim", http.StatusBadRequest)
		return
	}
	if err := t5Verify(b.cfg.HouseWord, b.cfg.Name, r.Header.Get(t5AuthHeader), body, now); err != nil {
		b.log.Warn("a claim that does not check out: check HOUSE_WORD", "from", r.RemoteAddr, "err", err)
		http.Error(w, "not this house", http.StatusForbidden)
		return
	}
	var c t5Claim
	if err := json.Unmarshal(body, &c); err != nil || c.From == "" {
		http.Error(w, "not a claim", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	if strings.EqualFold(c.From, b.cfg.Name) || c.Word == "" {
		return
	}
	phrase := b.phraseFor(c.Word)
	energy := c.Score - b.cfg.OffsetDB
	b.log.Info("TECHO5 claim -> kiosks", "show", c.From, "word", c.Word, "phrase", phrase,
		"show_db", round1(c.Score), "sent_db", round1(energy), "kiosks", len(b.kioskAddresses()))
	b.sendKS(ksClaimPacket(b.ksIDFor(c.From), phrase, energy))
}

// --- small things --------------------------------------------------------------------------------

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
