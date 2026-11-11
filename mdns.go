package main

// Just enough mDNS (RFC 6762/6763) to be one of the TECHO5 devices: answer when they look for
// "_techo5._tcp.local." and find them by looking ourselves. No dependencies, so the image stays small.

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	t5Service   = "_techo5._tcp.local."
	mdnsTTL     = 120
	browseFor   = 3 * time.Second
	browseEvery = 2 * time.Minute
	forgetAfter = 3*browseEvery + browseFor

	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
	typeANY = 255
	classIN = 1
)

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// --- wire format -------------------------------------------------------------------------------

type question struct {
	name    string
	typ     uint16
	unicast bool
}

type record struct {
	name   string
	typ    uint16
	ttl    uint32
	ptr    string   // PTR
	target string   // SRV
	port   uint16   // SRV
	txt    []string // TXT
	ip     net.IP   // A
}

type message struct {
	response  bool
	questions []question
	records   []record // answers, authority and additional, all together
}

var errShort = errors.New("short dns message")

func readName(msg []byte, off int) (string, int, error) {
	var labels []string
	end := -1
	for jumps := 0; ; {
		if off >= len(msg) {
			return "", 0, errShort
		}
		l := int(msg[off])
		switch {
		case l == 0:
			off++
			if end < 0 {
				end = off
			}
			return strings.Join(labels, ".") + ".", end, nil
		case l&0xC0 == 0xC0:
			if off+1 >= len(msg) {
				return "", 0, errShort
			}
			if jumps++; jumps > 64 {
				return "", 0, errors.New("dns name loops")
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
		default:
			if off+1+l > len(msg) {
				return "", 0, errShort
			}
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
}

func parseMessage(msg []byte) (message, error) {
	var m message
	if len(msg) < 12 {
		return m, errShort
	}
	m.response = msg[2]&0x80 != 0
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	rrs := int(binary.BigEndian.Uint16(msg[6:])) + int(binary.BigEndian.Uint16(msg[8:])) + int(binary.BigEndian.Uint16(msg[10:]))
	off := 12
	for range qd {
		name, next, err := readName(msg, off)
		if err != nil || next+4 > len(msg) {
			return m, errShort
		}
		class := binary.BigEndian.Uint16(msg[next+2:])
		m.questions = append(m.questions, question{name: name, typ: binary.BigEndian.Uint16(msg[next:]), unicast: class&0x8000 != 0})
		off = next + 4
	}
	for range rrs {
		name, next, err := readName(msg, off)
		if err != nil || next+10 > len(msg) {
			return m, errShort
		}
		r := record{name: name, typ: binary.BigEndian.Uint16(msg[next:]), ttl: binary.BigEndian.Uint32(msg[next+4:])}
		rdlen := int(binary.BigEndian.Uint16(msg[next+8:]))
		data := next + 10
		if data+rdlen > len(msg) {
			return m, errShort
		}
		switch r.typ {
		case typePTR:
			r.ptr, _, _ = readName(msg, data)
		case typeSRV:
			if rdlen >= 7 {
				r.port = binary.BigEndian.Uint16(msg[data+4:])
				r.target, _, _ = readName(msg, data+6)
			}
		case typeTXT:
			for p := data; p < data+rdlen; {
				l := int(msg[p])
				if p+1+l > data+rdlen {
					break
				}
				r.txt = append(r.txt, string(msg[p+1:p+1+l]))
				p += 1 + l
			}
		case typeA:
			if rdlen == 4 {
				r.ip = net.IPv4(msg[data], msg[data+1], msg[data+2], msg[data+3])
			}
		}
		m.records = append(m.records, r)
		off = data + rdlen
	}
	return m, nil
}

func putName(b []byte, name string) []byte {
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if l == "" {
			continue
		}
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

func putRecord(b []byte, r record, unique bool) []byte {
	b = putName(b, r.name)
	class := uint16(classIN)
	if unique {
		class |= 0x8000 // cache-flush: this host owns the record
	}
	b = binary.BigEndian.AppendUint16(b, r.typ)
	b = binary.BigEndian.AppendUint16(b, class)
	b = binary.BigEndian.AppendUint32(b, r.ttl)
	var data []byte
	switch r.typ {
	case typePTR:
		data = putName(nil, r.ptr)
	case typeSRV:
		data = binary.BigEndian.AppendUint16(data, 0) // priority
		data = binary.BigEndian.AppendUint16(data, 0) // weight
		data = binary.BigEndian.AppendUint16(data, r.port)
		data = putName(data, r.target)
	case typeTXT:
		for _, t := range r.txt {
			data = append(data, byte(len(t)))
			data = append(data, t...)
		}
	case typeA:
		data = append(data, r.ip.To4()...)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

// buildResponse is an authoritative answer with the rest of the service as additional records.
func buildResponse(answers, additional []record) []byte {
	b := []byte{0, 0, 0x84, 0, 0, 0}
	b = binary.BigEndian.AppendUint16(b, uint16(len(answers)))
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(additional)))
	for _, r := range answers {
		b = putRecord(b, r, r.typ != typePTR)
	}
	for _, r := range additional {
		b = putRecord(b, r, r.typ != typePTR)
	}
	return b
}

func buildQuery(name string, typ uint16) []byte {
	b := []byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	b = putName(b, name)
	b = binary.BigEndian.AppendUint16(b, typ)
	return binary.BigEndian.AppendUint16(b, classIN)
}

// --- the directory -------------------------------------------------------------------------------

// Directory advertises the bridge as a TECHO5 device and keeps the list of the real ones.
type Directory struct {
	name string
	port int
	ip   net.IP
	log  *slog.Logger

	instance, host string

	conn *net.UDPConn
	out  func(b []byte, to *net.UDPAddr) // replaces conn for sending, in tests

	mu    sync.Mutex
	by    map[string]knownPeer // by lowercase device name
	addrs map[string]net.IP    // host name -> address, from A records heard
}

type knownPeer struct {
	Peer
	at time.Time
}

var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9-]+`)

// NewDirectory advertises name on port, at ip.
func NewDirectory(name string, port int, ip net.IP, log *slog.Logger) *Directory {
	label := strings.Trim(labelUnsafe.ReplaceAllString(name, "-"), "-")
	if label == "" {
		label = "t5ks-bridge"
	}
	if len(label) > 40 {
		label = label[:40]
	}
	return &Directory{
		name: name, port: port, ip: ip, log: log,
		instance: label + "." + t5Service,
		host:     label + "-t5ks-bridge.local.",
		by:       map[string]knownPeer{},
		addrs:    map[string]net.IP{},
	}
}

// Run answers for the bridge and looks for the TECHO5 devices until ctx ends.
func (d *Directory) Run(ctx context.Context) error {
	conn, err := net.ListenMulticastUDP("udp4", nil, mdnsGroup)
	if err != nil {
		return err
	}
	d.conn = conn
	go func() { <-ctx.Done(); _ = conn.Close() }()
	go d.serve()

	d.log.Info("advertising to the TECHO5 devices", "name", d.name, "address", d.ip, "port", d.port)
	d.announce()
	time.AfterFunc(time.Second, d.announce)
	for {
		d.look()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(browseEvery):
		}
	}
}

func (d *Directory) ours() (ptr, srv, txt, a record) {
	ptr = record{name: t5Service, typ: typePTR, ttl: mdnsTTL, ptr: d.instance}
	srv = record{name: d.instance, typ: typeSRV, ttl: mdnsTTL, target: d.host, port: uint16(d.port)}
	txt = record{name: d.instance, typ: typeTXT, ttl: mdnsTTL, txt: []string{"name=" + d.name}}
	a = record{name: d.host, typ: typeA, ttl: mdnsTTL, ip: d.ip}
	return
}

func (d *Directory) announce() {
	ptr, srv, txt, a := d.ours()
	d.send(buildResponse([]record{ptr, srv, txt, a}, nil), mdnsGroup)
}

func (d *Directory) send(b []byte, to *net.UDPAddr) {
	if d.out != nil {
		d.out(b, to)
		return
	}
	if _, err := d.conn.WriteToUDP(b, to); err != nil {
		d.log.Debug("mdns send", "err", err)
	}
}

func (d *Directory) serve() {
	buf := make([]byte, 9000)
	for {
		n, from, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		m, err := parseMessage(buf[:n])
		if err != nil {
			continue
		}
		if m.response {
			d.heard(m)
		} else {
			d.answer(m, from)
		}
	}
}

// answer replies to a question about the TECHO5 service, this instance or this host.
func (d *Directory) answer(m message, from *net.UDPAddr) {
	ptr, srv, txt, a := d.ours()
	var answers, extra []record
	unicast := false
	for _, q := range m.questions {
		switch {
		case strings.EqualFold(q.name, t5Service) && (q.typ == typePTR || q.typ == typeANY):
			answers, extra = append(answers, ptr), []record{srv, txt, a}
		case strings.EqualFold(q.name, d.instance) && (q.typ == typeSRV || q.typ == typeTXT || q.typ == typeANY):
			answers, extra = append(answers, srv, txt), []record{a}
		case strings.EqualFold(q.name, d.host) && (q.typ == typeA || q.typ == typeANY):
			answers = append(answers, a)
		default:
			continue
		}
		unicast = unicast || q.unicast
	}
	if len(answers) == 0 {
		return
	}
	reply := buildResponse(answers, extra)
	d.send(reply, mdnsGroup)
	if unicast || from.Port != 5353 {
		d.send(reply, from) // a one-shot query from another port only hears a unicast answer
	}
}

// heard takes the TECHO5 devices out of any response on the network: the answers to our own looks,
// and the ones the devices give each other every couple of minutes.
func (d *Directory) heard(m message) {
	type found struct {
		target string
		port   uint16
		name   string
		hears  bool
		hasSRV bool
	}
	instances := map[string]*found{}
	get := func(name string) *found {
		k := strings.ToLower(name)
		if instances[k] == nil {
			instances[k] = &found{hears: true}
		}
		return instances[k]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range m.records {
		switch r.typ {
		case typeA:
			if r.ip != nil {
				d.addrs[strings.ToLower(r.name)] = r.ip
			}
		case typePTR:
			if strings.EqualFold(r.name, t5Service) {
				get(r.ptr)
			}
		case typeSRV:
			if strings.HasSuffix(strings.ToLower(r.name), t5Service) {
				f := get(r.name)
				f.target, f.port, f.hasSRV = r.target, r.port, true
			}
		case typeTXT:
			if strings.HasSuffix(strings.ToLower(r.name), t5Service) {
				f := get(r.name)
				f.name = txt(r.txt, "name=")
				if txt(r.txt, "hears=") == "0" {
					f.hears = false
				}
			}
		}
	}
	now := time.Now()
	for inst, f := range instances {
		if strings.EqualFold(inst, d.instance) || f.name == "" || !f.hasSRV || strings.EqualFold(f.name, d.name) {
			continue
		}
		ip := d.addrs[strings.ToLower(f.target)]
		if ip == nil {
			continue
		}
		k := strings.ToLower(f.name)
		p := Peer{Name: f.name, Address: ip.String(), Port: int(f.port), Hears: f.hears}
		if old, ok := d.by[k]; !ok || old.Peer != p {
			d.log.Info("TECHO5 device found", "name", p.Name, "address", p.Address, "port", p.Port, "hears", p.Hears)
		}
		d.by[k] = knownPeer{Peer: p, at: now}
	}
}

// look asks for the TECHO5 devices, and forgets the ones that stopped answering.
func (d *Directory) look() {
	d.send(buildQuery(t5Service, typePTR), mdnsGroup)
	time.Sleep(browseFor)
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, p := range d.by {
		if time.Since(p.at) > forgetAfter {
			delete(d.by, k)
			d.log.Info("TECHO5 device gone quiet", "name", p.Name)
		}
	}
}

// Peers is the TECHO5 devices known now.
func (d *Directory) Peers() []Peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Peer, 0, len(d.by))
	for _, p := range d.by {
		out = append(out, p.Peer)
	}
	slices.SortFunc(out, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func txt(records []string, key string) string {
	for _, t := range records {
		if v, ok := strings.CutPrefix(t, key); ok {
			return v
		}
	}
	return ""
}

// outboundIP is the address this host reaches the LAN from, to advertise.
func outboundIP() net.IP {
	c, err := net.Dial("udp4", "224.0.0.251:5353")
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}
