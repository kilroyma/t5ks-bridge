package main

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func announcement(d *Directory) []byte {
	ptr, srv, txt, a := d.ours()
	return buildResponse([]record{ptr}, []record{srv, txt, a})
}

func TestBridgeFindsAShowFromItsAnnouncement(t *testing.T) {
	kitchen := NewDirectory("Kitchen", 6053, net.IPv4(192, 168, 1, 40), quietLog())
	bridge := NewDirectory("Portal", 7330, net.IPv4(192, 168, 1, 5), quietLog())
	m, err := parseMessage(announcement(kitchen))
	if err != nil {
		t.Fatal(err)
	}
	bridge.heard(m)
	peers := bridge.Peers()
	if len(peers) != 1 || peers[0] != (Peer{Name: "Kitchen", Address: "192.168.1.40", Port: 6053, Hears: true}) {
		t.Fatalf("peers %+v", peers)
	}
}

func TestBridgeIgnoresItself(t *testing.T) {
	bridge := NewDirectory("Portal", 7330, net.IPv4(192, 168, 1, 5), quietLog())
	m, _ := parseMessage(announcement(bridge))
	bridge.heard(m)
	if n := len(bridge.Peers()); n != 0 {
		t.Fatalf("found itself as a TECHO5 device (%d)", n)
	}
}

// compressed builds a response the way most mDNS libraries do, with names pointing back at earlier
// ones, and a Skylight-style "hears=0" device.
func compressed() []byte {
	b := []byte{0, 0, 0x84, 0, 0, 0, 0, 1, 0, 0, 0, 3}
	svcOff := len(b)
	b = putName(b, t5Service)
	b = binary.BigEndian.AppendUint16(b, typePTR)
	b = binary.BigEndian.AppendUint16(b, classIN)
	b = binary.BigEndian.AppendUint32(b, 4500)
	// PTR data: "Skylight" + pointer to the service name
	ptrData := append([]byte{8}, "Skylight"...)
	ptrData = append(ptrData, 0xC0|byte(svcOff>>8), byte(svcOff))
	b = binary.BigEndian.AppendUint16(b, uint16(len(ptrData)))
	instOff := len(b)
	b = append(b, ptrData...)
	ptrTo := func(off int) []byte { return []byte{0xC0 | byte(off>>8), byte(off)} }

	// SRV, name compressed to the instance
	b = append(b, ptrTo(instOff)...)
	b = binary.BigEndian.AppendUint16(b, typeSRV)
	b = binary.BigEndian.AppendUint16(b, 0x8001)
	b = binary.BigEndian.AppendUint32(b, 120)
	srv := []byte{0, 0, 0, 0}
	srv = binary.BigEndian.AppendUint16(srv, 6053)
	hostOff := len(b) + 2 + len(srv)
	srv = putName(srv, "skylight-cal.local.")
	b = binary.BigEndian.AppendUint16(b, uint16(len(srv)))
	b = append(b, srv...)

	// TXT
	b = append(b, ptrTo(instOff)...)
	b = binary.BigEndian.AppendUint16(b, typeTXT)
	b = binary.BigEndian.AppendUint16(b, 0x8001)
	b = binary.BigEndian.AppendUint32(b, 4500)
	var txt []byte
	for _, s := range []string{"name=Skylight Calendar", "hears=0"} {
		txt = append(txt, byte(len(s)))
		txt = append(txt, s...)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(txt)))
	b = append(b, txt...)

	// A, name compressed to the SRV target
	b = append(b, ptrTo(hostOff)...)
	b = binary.BigEndian.AppendUint16(b, typeA)
	b = binary.BigEndian.AppendUint16(b, 0x8001)
	b = binary.BigEndian.AppendUint32(b, 120)
	b = binary.BigEndian.AppendUint16(b, 4)
	return append(b, 192, 168, 1, 30)
}

func TestCompressedResponseAndNoMicrophone(t *testing.T) {
	bridge := NewDirectory("Portal", 7330, net.IPv4(192, 168, 1, 5), quietLog())
	m, err := parseMessage(compressed())
	if err != nil {
		t.Fatal(err)
	}
	bridge.heard(m)
	peers := bridge.Peers()
	want := Peer{Name: "Skylight Calendar", Address: "192.168.1.30", Port: 6053, Hears: false}
	if len(peers) != 1 || peers[0] != want {
		t.Fatalf("peers %+v, want %+v", peers, want)
	}
}

func TestBridgeAnswersALook(t *testing.T) {
	bridge := NewDirectory("Portal", 7330, net.IPv4(192, 168, 1, 5), quietLog())
	type out struct {
		b  []byte
		to string
	}
	var sent []out
	bridge.out = func(b []byte, to *net.UDPAddr) { sent = append(sent, out{b, to.String()}) }

	q, _ := parseMessage(buildQuery(strings.ToUpper(t5Service), typePTR))
	bridge.answer(q, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 40), Port: 5353})
	if len(sent) != 1 || sent[0].to != "224.0.0.251:5353" {
		t.Fatalf("sent %d replies (%v), want one to the group", len(sent), sent)
	}
	// What a Show hears back is enough to find the bridge.
	show := NewDirectory("Kitchen", 6053, net.IPv4(192, 168, 1, 40), quietLog())
	m, err := parseMessage(sent[0].b)
	if err != nil {
		t.Fatal(err)
	}
	show.heard(m)
	if p := show.Peers(); len(p) != 1 || p[0] != (Peer{Name: "Portal", Address: "192.168.1.5", Port: 7330, Hears: true}) {
		t.Fatalf("the Show found %+v", p)
	}

	// A one-shot query from another port also gets the answer addressed to it.
	sent = nil
	bridge.answer(q, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 40), Port: 41000})
	if len(sent) != 2 || sent[1].to != "192.168.1.40:41000" {
		t.Fatalf("one-shot query: sent %v", sent)
	}

	// Questions about anything else go unanswered.
	sent = nil
	other, _ := parseMessage(buildQuery("_esphomelib._tcp.local.", typePTR))
	bridge.answer(other, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 40), Port: 5353})
	if len(sent) != 0 {
		t.Fatal("answered a question about another service")
	}
}

func TestMalformedPacketsDoNotPanic(t *testing.T) {
	good := compressed()
	for i := range good {
		_, _ = parseMessage(good[:i])
	}
	loop := []byte{0, 0, 0x84, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 12, 0, 1, 0, 1}
	if _, err := parseMessage(loop); err == nil {
		t.Fatal("a name pointing at itself parsed")
	}
}
