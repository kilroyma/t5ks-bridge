// t5ks-bridge lets TECHO5 devices and Kiosk Satellite kiosks settle which one answers a wake word they
// all heard, by translating each side's arbitration claims into the other's.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfg := Config{
		HouseWord: os.Getenv("HOUSE_WORD"),
		Name:      envOr("BRIDGE_NAME", "Portal"),
		OffsetDB:  envFloat(log, "OFFSET_DB", 0),
		Words:     parseWords(os.Getenv("WORD_MAP")),
	}
	if cfg.HouseWord == "" {
		log.Error("HOUSE_WORD is required: the house word set on your TECHO5 devices")
		os.Exit(2)
	}
	httpPort := int(envFloat(log, "HTTP_PORT", 7330))
	ksPort := int(envFloat(log, "KS_PORT", 2330))
	broadcast := envOr("KS_BROADCAST", "255.255.255.255")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	udp, err := ListenUDP(ksPort, broadcast, log)
	if err != nil {
		log.Error("cannot open the kiosks' UDP port", "port", ksPort, "err", err)
		os.Exit(1)
	}
	defer udp.Close()

	ip := net.ParseIP(os.Getenv("ADVERTISE_IP"))
	if ip == nil {
		ip = outboundIP()
	}
	if ip == nil || ip.To4() == nil {
		log.Error("cannot tell which address to advertise: set ADVERTISE_IP to this host's LAN IPv4 address")
		os.Exit(2)
	}
	dir := NewDirectory(cfg.Name, httpPort, ip.To4(), log)
	b := NewBridge(cfg, log, dir.Peers, udp)

	mux := http.NewServeMux()
	mux.HandleFunc("/nearest", b.ServeNearest)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(httpPort)))
	if err != nil {
		log.Error("cannot open the HTTP port", "port", httpPort, "err", err)
		os.Exit(1)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()
	go udp.Serve(b.HandleKS)
	go func() {
		if err := dir.Run(ctx); err != nil {
			log.Error("cannot open mDNS (UDP 5353): run with host networking", "err", err)
			stop()
		}
	}()
	go func() {
		b.SayHello()
		t := time.NewTicker(ksHelloEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.SayHello()
			}
		}
	}()

	log.Info("bridge running", "name", cfg.Name, "http_port", httpPort, "ks_port", ksPort, "offset_db", cfg.OffsetDB)
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	log.Info("bridge stopped")
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envFloat(log *slog.Logger, key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Error("not a number", "key", key, "value", v)
		os.Exit(2)
	}
	return f
}

// parseWords reads WORD_MAP: "okay_nabu=okay nabu;hey_jarvis=hey jarvis".
func parseWords(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(pair, "=")
		if ok && strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
