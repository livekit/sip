# Opus Codec Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Opus opt-in, fix panic on init failure, wire the config flag, and add
unit tests — without adding any YAGNI abstractions.

**Architecture:** The CGo-only codec registration in `media_codecs_opus.go` gains a
`Disabled:true` flag and a `SetOpusEnabled()` function; `service.go` calls it during
`Start()`; `config.go` adds the `EnableOpus bool` field. Two new test files cover
`resolveSDPName`, `codecSet`, SDP offer content, and codec selection preference.

**Tech Stack:** Go 1.26, `github.com/livekit/media-sdk` (msdk), `github.com/livekit/media-sdk/sdp`,
`github.com/livekit/media-sdk/g711`, `github.com/livekit/media-sdk/g722`,
`github.com/stretchr/testify/require`, CGo (`//go:build cgo`)

## Global Constraints

- All test files that import `media-sdk/opus` must carry `//go:build cgo` (requires `libopus` at build time).
- Build tag: `//go:build cgo` on `media_codecs_opus.go` and its test file.
- `resolveSDPName` and `codecSet` tests in `media_codecs_test.go` do NOT need the `cgo` build tag.
- `SetOpusEnabled` must update both `defaultCodecs` AND `msdk.GlobalCodecs` (via `msdk.CodecSetEnabled`).
- No new packages; all changes in `pkg/sip/` and `pkg/config/`.
- `OpusSDPName = "opus/48000/2"` is the canonical constant (already in `media_codecs.go`).

---

### Task 1: Add `EnableOpus` to config

**Files:**

- Modify: `pkg/config/config.go` (around line 107, near the `Codecs` field)

**Interfaces:**

- Produces: `Config.EnableOpus bool` — consumed by Task 3

- [ ] **Step 1: Add the field**

  Open `pkg/config/config.go`. After the `Codecs` field (line ~107), add:

  ```go
  EnableOpus bool `yaml:"enable_opus"`
  ```

- [ ] **Step 2: Verify build**

  ```bash
  go build -tags nocgo ./pkg/config/...
  ```

  Expected: no output (success).

- [ ] **Step 3: Commit**

  ```bash
  git add pkg/config/config.go
  git commit -m "feat: add EnableOpus config flag"
  ```

---

### Task 2: Fix `media_codecs_opus.go` — opt-in + no panic

**Files:**

- Modify: `pkg/sip/media_codecs_opus.go`

**Interfaces:**

- Consumes: `OpusSDPName` from `pkg/sip/media_codecs.go` (already `const OpusSDPName = "opus/48000/2"`)
- Produces:
  - `SetOpusEnabled(enabled bool)` — consumed by Task 3
  - Codec registered with `Disabled: true` — keeps Opus out of all codec sets until explicitly enabled

- [ ] **Step 1: Replace the file contents**

  The current file has two problems: it lacks `Disabled: true` (Opus is on by
  default) and it panics on init failure. Replace the entire file with:

  ```go
  // Copyright 2024 LiveKit, Inc.
  //
  // Licensed under the Apache License, Version 2.0 (the "License");
  // you may not use this file except in compliance with the License.
  // You may obtain a copy of the License at
  //
  //     http://www.apache.org/licenses/LICENSE-2.0
  //
  // Unless required by applicable law or agreed to in writing, software
  // distributed under the License is distributed on an "AS IS" BASIS,
  // WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  // See the License for the specific language governing permissions and
  // limitations under the License.

  //go:build cgo

  package sip

  import (
   msdk "github.com/livekit/media-sdk"
   "github.com/livekit/media-sdk/opus"
   "github.com/livekit/protocol/logger"
  )

  func init() {
   msdk.RegisterCodec(msdk.NewAudioCodec(msdk.CodecInfo{
    SDPName:      OpusSDPName,
    SampleRate:   48000,
    RTPClockRate: 48000,
    RTPIsStatic:  false,
    Priority:     10,
    Disabled:     true,
    FileExt:      "opus",
   }, opusDecode, opusEncode))
  }

  // SetOpusEnabled toggles Opus in both the per-call default codec set and the
  // global media-sdk codec set. Call once during Service.Start.
  func SetOpusEnabled(enabled bool) {
   defaultCodecs.SetEnabled(OpusSDPName, enabled)
   msdk.CodecSetEnabled(OpusSDPName, enabled)
  }

  func opusDecode(w msdk.PCM16Writer) msdk.WriteCloser[opus.Sample] {
   dec, err := opus.Decode(w, 1, logger.GetLogger())
   if err != nil {
    logger.GetLogger().Errorw("opus decode init failed", err)
    return nil
   }
   return dec
  }

  func opusEncode(w msdk.WriteCloser[opus.Sample]) msdk.PCM16Writer {
   enc, err := opus.Encode(w, 1, logger.GetLogger())
   if err != nil {
    logger.GetLogger().Errorw("opus encode init failed", err)
    return nil
   }
   return enc
  }
  ```

- [ ] **Step 2: Verify build (CGo required)**

  ```bash
  go build ./pkg/sip/...
  ```

  Expected: no output. If `libopus` is not installed locally, skip and rely on
  Docker CI — the Dockerfile already installs `libopus-dev`.

- [ ] **Step 3: Commit**

  ```bash
  git add pkg/sip/media_codecs_opus.go
  git commit -m "fix: make Opus opt-in and replace panic with error log"
  ```

---

### Task 3: Wire `EnableOpus` in `service.go`

**Files:**

- Modify: `pkg/sip/service.go` (inside `Start()`, around line 202)

**Interfaces:**

- Consumes: `Config.EnableOpus bool` (Task 1), `SetOpusEnabled(bool)` (Task 2)

- [ ] **Step 1: Call `SetOpusEnabled` in `Start()`**

  In `pkg/sip/service.go`, inside `func (s *Service) Start() error`, add this
  call immediately after the existing codec log loop (after line ~211,
  after `msdk.CodecsSetEnabled(s.conf.Codecs)`):

  ```go
  SetOpusEnabled(s.conf.EnableOpus)
  ```

- [ ] **Step 2: Verify build**

  ```bash
  go build ./pkg/sip/...
  ```

  Expected: no output.

- [ ] **Step 3: Commit**

  ```bash
  git add pkg/sip/service.go
  git commit -m "feat: wire EnableOpus config flag to SetOpusEnabled"
  ```

---

### Task 4: Unit tests for `resolveSDPName` and `codecSet`

**Files:**

- Create: `pkg/sip/media_codecs_test.go`

**Interfaces:**

- Consumes: `resolveSDPName(string) string` and `codecSet(*livekit.SIPMediaConfig) (*msdk.CodecSet, error)` from `pkg/sip/media_codecs.go`
- No `cgo` build tag needed — these functions have no CGo dependency.

- [ ] **Step 1: Create the test file**

  ```go
  // Copyright 2024 LiveKit, Inc.
  //
  // Licensed under the Apache License, Version 2.0 (the "License");
  // you may not use this file except in compliance with the License.
  // You may obtain a copy of the License at
  //
  //     http://www.apache.org/licenses/LICENSE-2.0
  //
  // Unless required by applicable law or agreed to in writing, software
  // distributed under the License is distributed on an "AS IS" BASIS,
  // WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  // See the License for the specific language governing permissions and
  // limitations under the License.

  //go:build cgo

  package sip

  import (
   "testing"

   "github.com/stretchr/testify/require"

   "github.com/livekit/protocol/livekit"
  )

  func TestResolveSDPName(t *testing.T) {
   t.Run("two-part name resolves to three-part", func(t *testing.T) {
    got := resolveSDPName("opus/48000")
    require.Equal(t, OpusSDPName, got)
   })
   t.Run("exact three-part match returns empty", func(t *testing.T) {
    got := resolveSDPName("opus/48000/2")
    require.Empty(t, got)
   })
   t.Run("unknown codec returns empty", func(t *testing.T) {
    got := resolveSDPName("unknown/8000")
    require.Empty(t, got)
   })
  }

  func TestCodecSetWithOpus(t *testing.T) {
   SetOpusEnabled(true)
   t.Cleanup(func() { SetOpusEnabled(false) })

   m := &livekit.SIPMediaConfig{
    OnlyListedCodecs: true,
    Codecs: []*livekit.SIPCodecOptions{
     {Name: "opus", Rate: 48000},
    },
   }
   s, err := codecSet(m)
   require.NoError(t, err)
   require.True(t, s.IsEnabledByName(OpusSDPName),
    "codecSet should enable opus/48000/2 when opus/48000 is listed")
  }
  ```

  > Note: `//go:build cgo` is required because `SetOpusEnabled` is defined in
  > `media_codecs_opus.go` which is CGo-only. Without this tag the test file
  > would fail to compile in non-CGo builds.

- [ ] **Step 2: Run the tests**

  ```bash
  go test ./pkg/sip/... -run "TestResolveSDPName|TestCodecSetWithOpus" -v
  ```

  Expected:

  ```text
  --- PASS: TestResolveSDPName/two-part_name_resolves_to_three-part
  --- PASS: TestResolveSDPName/exact_three-part_match_returns_empty
  --- PASS: TestResolveSDPName/unknown_codec_returns_empty
  --- PASS: TestCodecSetWithOpus
  ```

- [ ] **Step 3: Commit**

  ```bash
  git add pkg/sip/media_codecs_test.go
  git commit -m "test: add resolveSDPName and codecSet unit tests"
  ```

---

### Task 5: SDP negotiation tests for Opus

**Files:**

- Create: `pkg/sip/media_codecs_opus_test.go`

**Interfaces:**

- Consumes: `SetOpusEnabled(bool)` (Task 2), `OpusSDPName` constant, `defaultCodecs`
- Uses: `msdk`, `sdp.OfferMediaWith`, `sdp.SelectAudio`, `sdp.CodecByNameWith`, `g711`, `g722`

- [ ] **Step 1: Create the test file**

  ```go
  // Copyright 2024 LiveKit, Inc.
  //
  // Licensed under the Apache License, Version 2.0 (the "License");
  // you may not use this file except in compliance with the License.
  // You may obtain a copy of the License at
  //
  //     http://www.apache.org/licenses/LICENSE-2.0
  //
  // Unless required by applicable law or agreed to in writing, software
  // distributed under the License is distributed on an "AS IS" BASIS,
  // WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  // See the License for the specific language governing permissions and
  // limitations under the License.

  //go:build cgo

  package sip

  import (
   "strings"
   "testing"

   "github.com/stretchr/testify/require"

   msdk "github.com/livekit/media-sdk"
   "github.com/livekit/media-sdk/g711"
   "github.com/livekit/media-sdk/g722"
   "github.com/livekit/media-sdk/sdp"
  )

  // enableOpusForTest turns Opus on for a test and restores disabled state after.
  func enableOpusForTest(t *testing.T) {
   t.Helper()
   SetOpusEnabled(true)
   t.Cleanup(func() { SetOpusEnabled(false) })
  }

  // TestOpusDisabledByDefault verifies that without calling SetOpusEnabled,
  // Opus is absent from defaultCodecs — so existing deployments are unaffected.
  func TestOpusDisabledByDefault(t *testing.T) {
   c := sdp.CodecByNameWith(defaultCodecs, OpusSDPName)
   require.Nil(t, c, "opus must not appear in defaultCodecs by default")
  }

  // TestOpusRegistered verifies the codec is present in msdk.Codecs(), uses a
  // dynamic payload type, and runs at the correct 48 kHz clock rate.
  func TestOpusRegistered(t *testing.T) {
   enableOpusForTest(t)

   c := sdp.CodecByNameWith(defaultCodecs, OpusSDPName)
   require.NotNil(t, c, "opus codec must be present in defaultCodecs when enabled")

   _, ok := c.(msdk.AudioCodec)
   require.True(t, ok, "opus codec must implement AudioCodec")

   info := c.Info()
   require.Equal(t, OpusSDPName, info.SDPName)
   require.Equal(t, 48000, info.SampleRate)
   require.Equal(t, 48000, info.RTPClockRate)
   require.False(t, info.RTPIsStatic, "opus must use a dynamic payload type")
  }

  // TestOpusInSDPOffer verifies that after enabling Opus, an SDP offer contains
  // an rtpmap line advertising opus/48000/2.
  func TestOpusInSDPOffer(t *testing.T) {
   enableOpusForTest(t)

   _, md, err := sdp.OfferMediaWith(defaultCodecs, 12345, sdp.EncryptionNone)
   require.NoError(t, err)

   var found bool
   for _, a := range md.Attributes {
    if a.Key == "rtpmap" && strings.Contains(strings.ToLower(a.Value), "opus/48000/2") {
     found = true
     break
    }
   }
   require.True(t, found, "SDP offer should contain an rtpmap line for opus/48000/2")
  }

  // TestOpusPreferredOverG722 verifies codec selection picks Opus (priority 10)
  // over G722 (priority -5) and G711 (priority -10/-20) when all are offered.
  func TestOpusPreferredOverG722(t *testing.T) {
   enableOpusForTest(t)

   opusC, ok := sdp.CodecByNameWith(defaultCodecs, OpusSDPName).(msdk.AudioCodec)
   require.True(t, ok, "opus must be an AudioCodec")

   ulawC, ok := sdp.CodecByNameWith(defaultCodecs, g711.ULawSDPName).(msdk.AudioCodec)
   require.True(t, ok, "PCMU must be an AudioCodec")

   g722C, ok := sdp.CodecByNameWith(defaultCodecs, g722.SDPName).(msdk.AudioCodec)
   require.True(t, ok, "G722 must be an AudioCodec")

   desc := sdp.MediaDesc{
    Codecs: []sdp.CodecInfo{
     {Type: 0, Codec: ulawC},
     {Type: 9, Codec: g722C},
     {Type: 111, Codec: opusC},
    },
   }
   got, err := sdp.SelectAudio(desc, false)
   require.NoError(t, err)
   require.Equal(t, OpusSDPName, got.Codec.Info().SDPName,
    "Opus should win priority-based codec selection")
   require.Equal(t, byte(111), got.Type,
    "peer-assigned payload type 111 must be honored")
  }
  ```

- [ ] **Step 2: Run the tests**

  ```bash
  go test ./pkg/sip/... -run "TestOpus" -v
  ```

  Expected:

  ```text
  --- PASS: TestOpusDisabledByDefault
  --- PASS: TestOpusRegistered
  --- PASS: TestOpusInSDPOffer
  --- PASS: TestOpusPreferredOverG722
  ```

- [ ] **Step 3: Commit**

  ```bash
  git add pkg/sip/media_codecs_opus_test.go
  git commit -m "test: add SDP negotiation tests for Opus codec"
  ```

---

### Task 6: Update README

**Files:**

- Modify: `README.md`

**Interfaces:** None — documentation only.

- [ ] **Step 1: Add Opus note to the config section**

  Find the existing config reference block in `README.md` (around line 62,
  near the `sip_port` / `rtp_port` entries). Add after those lines:

  ```text
  enable_opus: offer the Opus codec for SIP media (default false, experimental)
  ```

  Then find the `### Using the SIP service` section and add a `#### Codecs`
  subsection before it:

  ```markdown
  #### Codecs

  PCMU, PCMA, G722, and DTMF are negotiated by default. Opus is **disabled by
  default** - set `enable_opus: true` to offer it. Validate interoperability with
  your SIP infrastructure before enabling in production.

  When enabled, Opus (`opus/48000/2`, 48 kHz mono) is preferred over G722 and
  G711. Peers that do not support Opus fall back to G722, then G711 transparently.
  ```

- [ ] **Step 2: Lint**

  ```bash
  npx markdownlint-cli --fix --disable MD013 -- README.md
  npx markdownlint-cli --disable MD013 -- README.md
  ```

  Expected: no errors after `--fix`.

- [ ] **Step 3: Commit**

  ```bash
  git add README.md
  git commit -m "docs: document enable_opus config flag"
  ```

---

## Self-Review

**Spec coverage:**

| Spec requirement | Task |
| --- | --- |
| `Disabled:true` + `enable_opus` flag | Task 1 + Task 2 |
| `SetOpusEnabled` wiring in `service.go` | Task 3 |
| Replace `panic` with error-log + return nil | Task 2 |
| `TestResolveSDPName` + `TestCodecSetWithOpus` | Task 4 |
| `TestOpusDisabledByDefault`, `TestOpusRegistered`, `TestOpusInSDPOffer`, `TestOpusPreferredOverG722` | Task 5 |
| README opt-in note | Task 6 |

**Placeholder scan:** None found.

**Type consistency:**

- `OpusSDPName` used in Tasks 2, 4, 5 — defined in `media_codecs.go` as `"opus/48000/2"`.
- `SetOpusEnabled(bool)` defined in Task 2, consumed in Tasks 3, 4, 5.
- `defaultCodecs` is `*msdk.CodecSet` package-level var in `media_codecs.go` — referenced in Tasks 4, 5.
- `sdp.CodecByNameWith`, `sdp.OfferMediaWith`, `sdp.SelectAudio`, `sdp.MediaDesc`, `sdp.CodecInfo` all from `github.com/livekit/media-sdk/sdp` — confirmed in module cache.
- `g711.ULawSDPName = "PCMU/8000"`, `g722.SDPName = "G722/8000"` — confirmed in module cache.
