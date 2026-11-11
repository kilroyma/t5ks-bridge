package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"t5ks-bridge/internal/upstream"
)

const word = "correct horse battery"

// --- TECHO5's own code agrees with the bridge's copy -----------------------------------------------

func TestSigningMatchesTECHO5(t *testing.T) {
	body := []byte(`{"from":"Portal","turn":"ab","word":"okay_nabu","score":12.3}`)
	now := time.Now()

	if err := upstream.Verify(word, "Kitchen", t5Sign(word, "Kitchen", body, now), body, now); err != nil {
		t.Fatalf("a Show would refuse the bridge's claim: %v", err)
	}
	if err := t5Verify(word, "Portal", upstream.Sign(word, "Portal", body, now), body, now); err != nil {
		t.Fatalf("the bridge would refuse a Show's claim: %v", err)
	}
	if err := upstream.Verify("wrong word", "Kitchen", t5Sign(word, "Kitchen", body, now), body, now); err == nil {
		t.Fatal("a claim signed with another house word was taken")
	}
	if err := upstream.Verify(word, "Living Room", t5Sign(word, "Kitchen", body, now), body, now); err == nil {
		t.Fatal("a claim for one Show was taken by another")
	}
	h := t5Sign(word, "Kitchen", body, now)
	proof := upstream.AnsweredMAC(word, upstream.NonceOf(h), "Portal", "Kitchen")
	if !t5AnsweredBy(word, h, proof, "Portal", "Kitchen") {
		t.Fatal("the bridge would not believe a Show's proof that it answered")
	}
	if t5AnsweredBy(word, h, strings.Repeat("0", len(proof)), "Portal", "Kitchen") {
		t.Fatal("the bridge believed a forged proof")
	}
}

// --- fakes ---------------------------------------------------------------------------------------

type sent struct {
	data []byte
	to   string // "" for broadcast
}

type fakeKS struct {
	mu   sync.Mutex
	sent []sent
}

func (f *fakeKS) Broadcast(data []byte) {
	f.mu.Lock()
	f.sent = append(f.sent, sent{data: data})
	f.mu.Unlock()
}

func (f *fakeKS) SendTo(data []byte, address string) {
	f.mu.Lock()
	f.sent = append(f.sent, sent{data: data, to: address})
	f.mu.Unlock()
}

func (f *fakeKS) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

// ksAccepts is Kiosk Satellite's own check on an incoming packet (wake_arbitration.dart, _onPacket),
// including that v and n are JSON integers, which Dart's jsonDecode needs for its `is int` tests.
func ksAccepts(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var msg map[string]any
	if err := dec.Decode(&msg); err != nil {
		t.Fatalf("not JSON: %s", data)
	}
	isInt := func(v any) bool {
		n, ok := v.(json.Number)
		return ok && !strings.ContainsAny(string(n), ".eE")
	}
	id, _ := msg["id"].(string)
	if msg["ks"] != "wake" || msg["v"] != json.Number("1") || id == "" || len(id) > 64 {
		t.Fatalf("a kiosk would drop this packet: %s", data)
	}
	if msg["hello"] == true {
		return msg
	}
	if _, ok := msg["p"].(string); !ok || !isInt(msg["n"]) {
		t.Fatalf("a kiosk would drop this claim: %s", data)
	}
	if _, ok := msg["e"].(json.Number); !ok {
		t.Fatalf("a kiosk would drop this claim's energy: %s", data)
	}
	return msg
}

// fakeShow is a TECHO5 device's /nearest, checking claims with TECHO5's own code.
type fakeShow struct {
	name     string
	answered bool
	srv      *httptest.Server

	mu     sync.Mutex
	claims []t5Claim
}

func newShow(t *testing.T, name string, answered bool) *fakeShow {
	s := &fakeShow{name: name, answered: answered}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h := r.Header.Get(upstream.AuthHeader)
		if err := upstream.Verify(word, s.name, h, body, time.Now()); err != nil {
			http.Error(w, "not this house", http.StatusForbidden)
			return
		}
		var c t5Claim
		_ = json.Unmarshal(body, &c)
		s.mu.Lock()
		s.claims = append(s.claims, c)
		s.mu.Unlock()
		if s.answered {
			w.Header().Set(upstream.AnsweredHeader, upstream.AnsweredMAC(word, upstream.NonceOf(h), c.From, s.name))
			http.Error(w, "answered here already", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeShow) peer() Peer {
	u, _ := url.Parse(s.srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	return Peer{Name: s.name, Address: host, Port: p, Hears: true}
}

func (s *fakeShow) got() []t5Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]t5Claim(nil), s.claims...)
}

func newTestBridge(t *testing.T, offset float64, shows ...*fakeShow) (*Bridge, *fakeKS) {
	ks := &fakeKS{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := NewBridge(Config{HouseWord: word, Name: "Portal", OffsetDB: offset}, log, func() []Peer {
		var out []Peer
		for _, s := range shows {
			out = append(out, s.peer())
		}
		return out
	}, ks)
	b.after = func(_ time.Duration, f func()) { f() }
	return b, ks
}

// kioskClaim is a claim exactly as Kiosk Satellite's contend() builds it.
func kioskClaim(id string, n int, phrase string, energy float64) []byte {
	data, _ := json.Marshal(map[string]any{"ks": "wake", "v": 1, "id": id, "n": n, "p": phrase, "e": energy})
	return data
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- kiosk -> Shows ------------------------------------------------------------------------------

func TestKioskClaimReachesEveryShow(t *testing.T) {
	kitchen, living := newShow(t, "Kitchen", false), newShow(t, "Living Room", false)
	b, ks := newTestBridge(t, 2, kitchen, living)

	pkt := kioskClaim("9f3a11c0de2b7e44", 12345, "okay nabu", 12.3)
	for range 6 { // three rounds, broadcast and addressed
		b.HandleKS(pkt, "192.168.1.50")
	}
	waitFor(t, "both Shows to get the claim", func() bool { return len(kitchen.got()) == 1 && len(living.got()) == 1 })
	time.Sleep(50 * time.Millisecond)

	for _, s := range []*fakeShow{kitchen, living} {
		got := s.got()
		if len(got) != 1 {
			t.Fatalf("%s got %d claims for one wake word, want 1", s.name, len(got))
		}
		c := got[0]
		if c.From != "Portal" || c.Word != "okay_nabu" || c.Score != 14.3 || c.Turn == "" {
			t.Fatalf("%s got %+v", s.name, c)
		}
	}
	for _, p := range ks.all() {
		if m := ksAccepts(t, p.data); m["hello"] != true && m["e"] == json.Number("999") {
			t.Fatal("told the kiosk to stand down though no Show had answered")
		}
	}
}

func TestShowThatAnsweredMakesKioskStandDown(t *testing.T) {
	kitchen := newShow(t, "Kitchen", true)
	b, ks := newTestBridge(t, 0, kitchen)
	b.HandleKS(kioskClaim("9f3a11c0de2b7e44", 7, "Okay Nabu", 20), "192.168.1.50")

	var standDown []map[string]any
	waitFor(t, "a stand-down claim to the kiosks", func() bool {
		standDown = nil
		for _, p := range ks.all() {
			if m := ksAccepts(t, p.data); m["e"] == json.Number("999") {
				standDown = append(standDown, m)
			}
		}
		return len(standDown) >= 3
	})
	m := standDown[0]
	if m["p"] != "okay nabu" || m["id"] != b.ksIDFor("Kitchen") {
		t.Fatalf("stand-down claim %v", m)
	}
}

func TestOwnBroadcastNotRelayed(t *testing.T) {
	kitchen := newShow(t, "Kitchen", false)
	b, _ := newTestBridge(t, 0, kitchen)
	b.HandleKS(ksClaimPacket(b.ksIDFor("Living Room"), "okay nabu", 10), "192.168.1.60")
	b.HandleKS(b.ksHello(), "192.168.1.60")
	time.Sleep(100 * time.Millisecond)
	if n := len(kitchen.got()); n != 0 {
		t.Fatalf("the bridge relayed its own broadcast back to the Shows (%d claims)", n)
	}
}

func TestHelloIsAnsweredOnce(t *testing.T) {
	b, ks := newTestBridge(t, 0)
	hello, _ := json.Marshal(map[string]any{"ks": "wake", "v": 1, "hello": true, "id": "aa11bb22cc33dd44"})
	b.HandleKS(hello, "192.168.1.50")
	b.HandleKS(hello, "192.168.1.50")
	var replies int
	for _, p := range ks.all() {
		if p.to == "192.168.1.50" && ksAccepts(t, p.data)["hello"] == true {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("answered a new kiosk's hello %d times, want 1", replies)
	}
}

// --- Show -> kiosks ------------------------------------------------------------------------------

func postClaim(t *testing.T, b *Bridge, signWith string, c t5Claim) int {
	t.Helper()
	body, _ := json.Marshal(c)
	req := httptest.NewRequest(http.MethodPost, "/nearest", bytes.NewReader(body))
	req.Header.Set(upstream.AuthHeader, upstream.Sign(signWith, "Portal", body, time.Now()))
	w := httptest.NewRecorder()
	b.ServeNearest(w, req)
	return w.Code
}

func TestShowClaimReachesKiosks(t *testing.T) {
	b, ks := newTestBridge(t, 2)
	hello, _ := json.Marshal(map[string]any{"ks": "wake", "v": 1, "hello": true, "id": "aa11bb22cc33dd44"})
	b.HandleKS(hello, "192.168.1.50")
	before := len(ks.all())

	if code := postClaim(t, b, word, t5Claim{From: "Kitchen", Turn: "1", Word: "okay_nabu", Score: 15.7}); code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", code)
	}
	var broadcast, addressed int
	for _, p := range ks.all()[before:] {
		m := ksAccepts(t, p.data)
		if m["p"] != "okay nabu" || m["e"] != json.Number("13.7") || m["id"] != b.ksIDFor("Kitchen") {
			t.Fatalf("kiosk claim %v", m)
		}
		if p.to == "" {
			broadcast++
		} else if p.to == "192.168.1.50" {
			addressed++
		}
	}
	if broadcast != 3 || addressed != 3 {
		t.Fatalf("sent %d broadcast and %d addressed, want 3 and 3, as a kiosk does", broadcast, addressed)
	}
}

func TestClaimFromAnotherHouseRefused(t *testing.T) {
	b, ks := newTestBridge(t, 0)
	if code := postClaim(t, b, "not our word", t5Claim{From: "Kitchen", Turn: "1", Word: "okay_nabu", Score: 30}); code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", code)
	}
	if n := len(ks.all()); n != 0 {
		t.Fatalf("a refused claim still reached the kiosks (%d packets)", n)
	}
}

func TestReplayedClaimRefused(t *testing.T) {
	b, _ := newTestBridge(t, 0)
	body, _ := json.Marshal(t5Claim{From: "Kitchen", Turn: "1", Word: "okay_nabu", Score: 10})
	h := upstream.Sign(word, "Portal", body, time.Now())
	for i, want := range []int{http.StatusNoContent, http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodPost, "/nearest", bytes.NewReader(body))
		req.Header.Set(upstream.AuthHeader, h)
		w := httptest.NewRecorder()
		b.ServeNearest(w, req)
		if w.Code != want {
			t.Fatalf("send %d: status %d, want %d", i+1, w.Code, want)
		}
	}
}

// --- names ---------------------------------------------------------------------------------------

func TestWakeWordNames(t *testing.T) {
	b := &Bridge{cfg: Config{Words: map[string]string{"okay_nabu": "Ok Nabu"}}}
	if got := b.phraseFor("okay_nabu"); got != "ok nabu" {
		t.Fatalf("phraseFor(okay_nabu) = %q", got)
	}
	if got := b.wordFor("OK  nabu"); got != "okay_nabu" {
		t.Fatalf("wordFor(OK  nabu) = %q", got)
	}
	b = &Bridge{}
	for t5, ks := range map[string]string{"okay_nabu": "okay nabu", "hey_jarvis": "hey jarvis", "alexa": "alexa"} {
		if got := b.phraseFor(t5); got != ks {
			t.Fatalf("phraseFor(%s) = %q, want %q", t5, got, ks)
		}
		if got := b.wordFor(ks); got != t5 {
			t.Fatalf("wordFor(%s) = %q, want %q", ks, got, t5)
		}
	}
}
