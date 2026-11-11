# t5ks-bridge

Wake word arbitration across **TECHO5** devices and **Kiosk Satellite** kiosks. When a Show and the
Portal both hear "Okay Nabu", only the one you spoke to answers, as if they ran the same software.

Each side already settles this among its own devices: every device that hears the wake word says how
far it stood out over its room's noise, waits a moment, and the quieter ones go back to listening. The
bridge translates those claims between the two formats, so each side sees the other's devices as its own:

| | TECHO5 | Kiosk Satellite |
|---|---|---|
| Transport | HTTP POST to each device's `/nearest` | UDP broadcast on port 2330 |
| Finding each other | mDNS `_techo5._tcp` | "hello" packets |
| Security | HMAC keyed from the house word | none |
| Score | dB over the device's noise floor | dB over the device's noise floor |

**Tested with:** TECHO5 v1.6.x on three Echo Show 5 (2nd gen), and Kiosk Satellite 2026.10.x on a Meta
Portal 10" (2nd gen), with the bridge in Docker in a Proxmox LXC container. An unofficial community
project, not affiliated with either.

The bridge advertises itself to the Shows as a TECHO5 device (named `Portal` by default), and to the
kiosks as another kiosk. Neither app is modified. If the bridge is down or anything goes wrong, every
device answers exactly as it would without arbitration.

## Before you start

- **Same network segment.** The bridge, the Shows and the Portal must share one subnet: broadcast and
  mDNS don't cross VLANs or a separate IoT Wi-Fi network unless your router repeats them. If the
  devices are on the IoT Wi-Fi and the Docker host is on the main LAN, check that first.
- **TECHO5:** on each Show, a **house word** is set (setup page) and **Nearest device answers** is on
  (Home Assistant device page, Configuration).
- **Kiosk Satellite:** on the Portal, **Voice Satellite → Wake Word → Wake word arbitration** is on.
- **Same wake word** on the Shows and the Portal that should compete.

## Install

On a Docker host on the same network (needs host networking, so not inside Home Assistant OS):

1. `git clone https://github.com/kilroyma/t5ks-bridge.git && cd t5ks-bridge`
2. Edit `compose.yaml`: set `HOUSE_WORD` to your TECHO5 house word.
3. `docker compose up -d --build`
4. `docker logs -f t5ks-bridge` and check for:
   - `advertising to the TECHO5 devices ... address=<this host's LAN IP>` (if the IP is wrong, set
     `ADVERTISE_IP`)
   - `TECHO5 device found name=...` for each Show (within ~2 minutes)
   - `kiosk found address=...` for the Portal (within ~5 minutes, or at its first wake word)

## Match the windows

Each side waits a while for the other's claims. Set them the same, and generous enough for Wi-Fi:

- Each Show: **Nearest device window** → `400` ms (HA device page, Configuration)
- Portal: **Voice Satellite → Wake Word → Arbitration window** → `400` ms

## Calibrate

The two sides don't score on the same scale. A Meta Portal's microphone array cleans its audio before
Kiosk Satellite hears it, so it reports far higher numbers than an Echo Show: in testing, a Portal
standing ~5 m from the speaker still outscored an Echo Show the speaker was standing right in front
of (32.7 dB vs 13.9 dB). Without calibration the Portal wins almost everything.

1. Say the wake word standing right at the kiosk, right at the nearest Show, and halfway between.
2. Read `docker logs t5ks-bridge`. Each wake word shows every device's score:
   - `kiosk claim -> TECHO5 ... kiosk_db=37.5 sent_db=12.5` (the kiosk; `sent_db` is after `OFFSET_DB`)
   - `TECHO5 claim -> kiosks ... show="Living Room Echo" show_db=13.9` (a Show)
   - `show had already answered; telling the kiosks to stand down` means the Show won on timing, not
     loudness: the kiosk's claim arrived after the Show's window closed.
3. Set `OFFSET_DB` so each device wins clearly when you're beside it (compare `sent_db` with
   `show_db`), then `docker compose up -d`. Lower it if the kiosk answers from too far away; raise it
   if the kiosk loses when you're beside it.

**Reference:** Meta Portal 10" (2nd gen) against Echo Show 5 (2nd gen), ~5 m apart: `OFFSET_DB: "-25"`.
After that, each won by 3–10 dB when spoken to directly, and halfway was a near tie.

`OFFSET_DB` is usually easier than the devices' own offsets: it's one setting with no range limit. The
per-device offsets (the kiosk's **Arbitration offset**, each Show's **Nearest device offset** in Home
Assistant) still apply on top, for fine-tuning one device against the others.

**Check mic gain first.** If you turned a Show's microphone gain down earlier to stop it answering,
put it back to the default (20 dB) before calibrating, or it will lose arbitration everywhere.

**Kiosks detect later.** Kiosk Satellite reported the wake word 0.2–0.4 s after the Echo Shows did.
If a Show answers when you're clearly at the kiosk, raise every window (both sides) to 500 ms.

## Settings

| Variable | Default | |
|---|---|---|
| `HOUSE_WORD` | (required) | The TECHO5 house word |
| `BRIDGE_NAME` | `Portal` | The name the Shows know the bridge by |
| `OFFSET_DB` | `0` | Added to kiosk scores sent to the Shows, taken off Show scores sent to kiosks |
| `ADVERTISE_IP` | detected | This host's LAN IPv4 address |
| `WORD_MAP` | | Overrides for wake word names, e.g. `okay_nabu=ok nabu;hey_jarvis=hey jarvis` |
| `HTTP_PORT` | `7330` | Where the Shows send claims |
| `KS_PORT` | `2330` | Kiosk Satellite's port; don't change |
| `LOG_LEVEL` | `info` | `debug` for every packet |

Wake word names match automatically: TECHO5's `okay_nabu` is Kiosk Satellite's "Okay Nabu".

## Troubleshooting

| Log line | Fix |
|---|---|
| `show refused the claim: check HOUSE_WORD` | House word differs, or this host's clock is off by more than 5 s |
| `show is not taking claims: turn on Nearest device answers` | Turn it on for that Show, and set its house word |
| No `TECHO5 device found` | mDNS isn't reaching the host: same subnet? host networking? |
| No `kiosk found` | Arbitration off on the Portal, or broadcast blocked between Wi-Fi and LAN |
| Both still answer | Windows not matched, or one device detects much later than the other: raise both windows to 500 |

## Known quirks

- Because the bridge looks like a TECHO5 device, the Shows may list "Portal" as an intercom or
  announcement target. Those calls fail harmlessly (the bridge only answers `/nearest`).
- The signing code is copied from TECHO5 (`techo5auth.go`). If a TECHO5 release changes its claim
  format (it's `v2` today), the Shows will refuse the bridge's claims and everything falls back to
  answering as before, until the bridge is updated.

## Tests

`go test ./...` checks the bridge's signing against TECHO5's own code (copied verbatim into
`internal/upstream`), every packet against Kiosk Satellite's acceptance rules, both relay directions,
the "already answered" stand-down, replay and wrong-house refusals, and mDNS discovery.
