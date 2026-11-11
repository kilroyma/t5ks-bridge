package main

// The kiosks' UDP port.

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// --- Kiosk Satellite: UDP ----------------------------------------------------------------------

// UDP is the kiosks' port, open for broadcast.
type UDP struct {
	conn      *net.UDPConn
	port      int
	broadcast *net.UDPAddr
	log       *slog.Logger
}

// ListenUDP opens the port the kiosks claim on, sharing it if anything else has it open (as the kiosks do).
func ListenUDP(port int, broadcast string, log *slog.Logger) (*UDP, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); serr != nil {
				return
			}
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return &UDP{
		conn:      pc.(*net.UDPConn),
		port:      port,
		broadcast: &net.UDPAddr{IP: net.ParseIP(broadcast), Port: port},
		log:       log,
	}, nil
}

// Serve hands every datagram to handle until the connection closes.
func (u *UDP) Serve(handle func(data []byte, from string)) {
	buf := make([]byte, 2048)
	for {
		n, addr, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			if strings.Contains(err.Error(), "use of closed") {
				return
			}
			u.log.Debug("udp read", "err", err)
			continue
		}
		data := slices.Clone(buf[:n])
		handle(data, addr.IP.String())
	}
}

func (u *UDP) Broadcast(data []byte) {
	if _, err := u.conn.WriteToUDP(data, u.broadcast); err != nil {
		u.log.Debug("udp broadcast", "err", err)
	}
}

func (u *UDP) SendTo(data []byte, address string) {
	ip := net.ParseIP(address)
	if ip == nil {
		return
	}
	if _, err := u.conn.WriteToUDP(data, &net.UDPAddr{IP: ip, Port: u.port}); err != nil {
		u.log.Debug("udp send", "to", address, "err", err)
	}
}

func (u *UDP) Close() error { return u.conn.Close() }
