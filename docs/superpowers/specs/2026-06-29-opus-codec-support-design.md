# Opus Codec Support - Design Spec

**Date:** 2026-06-29
**Branch:** feature/opus-codec-support
**Status:** Approved

## Goal

Complete the Opus codec integration started on this branch so it is safe to
ship: opt-in by default, consistent error handling, and tested.

## Context

Commit `16aff0d` added:

- `media_codecs_opus.go` - CGo-only codec registration via `msdk.RegisterCodec`
- `OpusSDPName = "opus/48000/2"` added to `defaultCodecs` (always enabled)
- `resolveSDPName` - maps `name/rate` to full 3-part SDP names
- Test fix for 3-part SDP name parsing in `media_port_test.go`

**Gaps vs. upstream PR [#712](https://github.com/livekit/sip/pull/712):**

| Gap | This design |
| --- | --- |
| Opus enabled by default (breaks existing deployments) | `Disabled:true` + `enable_opus` flag |
| `panic` on codec init failure | error-log + return nil, fail call not process |
| No `resolveSDPName` unit test | Add `media_codecs_test.go` |
| No SDP negotiation tests | Add `media_codecs_opus_test.go` |
| README silent on Opus | Add opt-in note |

PR 712 is ~1415 lines; this design targets ~150 lines by dropping configurable
bitrate/complexity/FEC, channel-adaptive decoder, and discard writers (all YAGNI
until a real peer needs them).

## Architecture

No new packages. All changes in `pkg/sip/` and `pkg/config/`.

```text
pkg/config/config.go          + EnableOpus bool
pkg/sip/media_codecs.go       (no change)
pkg/sip/media_codecs_opus.go  + Disabled:true, SetOpusEnabled(), fix panic
pkg/sip/service.go            + SetOpusEnabled(conf.EnableOpus) in Start()
pkg/sip/media_codecs_test.go  new: resolveSDPName + codecSet tests
pkg/sip/media_codecs_opus_test.go  new: SDP offer + selection tests
README.md                     + Opus opt-in note
```

## Data Flow

### Startup

```text
NewConfig() → conf.EnableOpus (default false)
Service.Start() → SetOpusEnabled(conf.EnableOpus)
  → defaultCodecs.SetEnabled("opus/48000/2", enabled)
  → msdk.CodecSetEnabled("opus/48000/2", enabled)
```

When `EnableOpus` is false (default), Opus never enters any codec set - zero
behavior change for existing deployments.

### Inbound call

```text
SDP offer arrives → mp.SetOffer(offerData, mconf.Codecs, ...)
  → codecSet() resolves "opus/48000/2" if listed
  → SDP answer selects by priority (Opus=10 > G722=-5 > G711=-10/-20)
  → media pipeline: RTP → opusDecode → PCM16 → mixer
```

### Outbound call

```text
mp.NewOffer(mconf.Codecs, ...) → SDP offer includes Opus if enabled
  → peer answer selects codec → opusEncode if Opus negotiated
```

## Error Handling

**Codec init failure** (`opusDecode` / `opusEncode`):

- Replace `panic("opus ... init: " + err.Error())` with `log.Errorw(...); return nil`
- Consistent with rest of codebase; fails the call setup, not the process

**`resolveSDPName` no match:**

- Returns `""` - `codecSet` skips the extra `SetEnabled` call
- No silent data loss; caller already set `name/rate` directly

**`SetOpusEnabled(false)` at startup (default):**

- Opus absent from all codec sets - SDP offers/answers unchanged

## Testing

All tests CGo-gated (same as `TestMediaPort`). No new dependencies.

### `media_codecs_test.go` (new)

- `TestResolveSDPName`: `"opus/48000"` → `"opus/48000/2"`, exact match returns `""`,
  no match returns `""`
- `TestCodecSetWithOpus`: `codecSet` on `SIPMediaConfig` listing `opus/48000` enables
  `opus/48000/2`

### `media_codecs_opus_test.go` (new)

- `TestOpusRegistered`: codec present in `msdk.Codecs()`, dynamic PT, 48 kHz clock
- `TestOpusDisabledByDefault`: `defaultCodecs` excludes Opus before `SetOpusEnabled(true)`
- `TestOpusInSDPOffer`: after `SetOpusEnabled(true)`, offer contains `opus/48000/2`
- `TestOpusPreferredOverG722`: `sdp.SelectAudio` picks Opus when both offered

## Deliberately Out of Scope

- Configurable bitrate/complexity/FEC - no confirmed peer requirement
- Channel-adaptive decoder (`opus_packet_get_nb_channels`) - SIP telephony is mono
- `a=fmtp` attributes - deferred until a specific peer needs them
- `discard*Writer` types - fail loudly (error log + nil) is better for voice calls
- `GlobalCodecs`/`defaultCodecs` reconciliation - separate concern, separate PR
