# LiveKit SIP threat model

`github.com/livekit/sip` is a Go service that bridges telephone calls into
LiveKit rooms. It listens for SIP signalling on UDP, TCP, and optionally TLS,
from the public telephone network and SIP trunks. It negotiates RTP or SRTP
audio for each call and joins the call to a room as a participant. It also
places outbound calls and transfers calls with SIP REFER when an API caller asks
it to. Audio codecs, SDP, RTP, and DTMF decoding come from `livekit/media-sdk`,
which calls libopus and libsoxr through cgo.

## Scope and trust boundaries

Audit the Go on `main`. Prioritise:

- Inbound signalling in `pkg/sip/`: `server.go` (listeners and the handlers for
  OPTIONS, INVITE, ACK, BYE, and NOTIFY; any other method gets 405 from
  `OnNoRoute`) and `inbound.go`: `processInvite`, `newInbound`,
  `handleInviteAuth` (digest authentication), `handleInvite`, `pinPrompt`, and
  re-INVITE handling.
- In-dialog matching: `GetLocalTagUAS` in `types.go` and the `byLocalTag`
  lookups that route ACK, BYE, NOTIFY, and re-INVITE to an existing call.
- Outbound calls and transfers: `client.go` (`buildRequestURI`,
  `buildFromHeader`, `buildToHeader`, `buildRawURI`, `buildValuesURI`),
  `outbound.go` (`Invite`, `attemptInvite`, the auth retry loop, Record-Route
  handling, and `watchCancelledInvite`), and `protocol.go` (`NewReferRequest`,
  `handleNotify` and the sipfrag status it carries, `ParseReasonHeader`).
- Header and attribute mapping in `types.go`: `HeadersToAttrs` (SIP headers
  into participant attributes) and `AttrsToHeaders` (participant attributes
  into SIP headers).
- Media: `media_port.go` (RTP socket, remote address, and SDP offer/answer and
  renegotiation), `media_pipeline.go`, and `media.go`.
- TLS: `tls.go` (`ConfigureTLS`), the TLS setup in `service.go`, and
  `pkg/config/tls.go`.
- The RPC handlers in `pkg/sip/service.go` and `pkg/service/`.

Out of scope: `test/` (end-to-end harnesses and SIP clients), `magefile.go`,
`build/`, generated code (`pkg/stats/healthstatus_string.go`), and `res/` (embedded
audio prompts).

Untrusted input:

- Every SIP request and response that reaches a listening port: INVITE,
  re-INVITE, ACK, BYE, NOTIFY, OPTIONS, and CANCEL, with all headers and SDP
  bodies, including `Proxy-Authorization`, Call-ID, From and To tags, Via,
  Contact, Record-Route, and Reason. Any host that can reach the port can send
  them; nothing is authenticated until digest authentication passes or the
  trunk lookup accepts the source.
- Responses from peers on outbound calls and transfers: provisional and final
  responses, SDP answers, `WWW-Authenticate` and `Proxy-Authenticate`
  challenges, Record-Route and Contact, and the NOTIFY messages that report a
  transfer's progress.
- RTP, RTCP, and RFC 4733 DTMF from callers, including the PIN digits a caller
  enters. The RTP sockets are unconnected, so any host that can reach a media
  port can send packets to it.

Trusted: the operator's config file, the API key and secret, Redis and the
psrpc bus, and the LiveKit server. The service does not match trunks or
evaluate dispatch rules itself. For each INVITE it sends the From and To users
and hosts and the source IP to the LiveKit server (`GetAuthCredentials` and
`DispatchCall` in `pkg/service/psrpc.go`), and acts on the `AuthResult` and
`DispatchResult` that come back. The service's job is to report those call
facts faithfully, to enforce every result, and to run digest authentication
itself when the result is `AuthPassword`.

Outbound call and transfer requests arrive over psrpc from the LiveKit server
on behalf of an API caller who holds the API secret. Treat the caller as
trusted to place calls. Numbers, URIs, display names, `Headers`, and
`TransferTo` still flow onto the wire, so treat them as data that must not
change the structure of the SIP message they land in. The LiveKit protocol
library validates these requests before they reach this service; report a
field it leaves unchecked that this service writes into a message.

SIP message parsing is done by the `github.com/livekit/sipgo` fork. A crash or
unbounded allocation reachable through this service's listeners is in scope
here. Codec, SDP, RTP, SRTP, jitter buffer, and DTMF parser bugs belong to
`livekit/media-sdk`, which has its own threat model, unless this service
misuses those APIs. LiveKit server (`livekit/livekit`) is enrolled separately,
and the protocol library (`livekit/protocol`) has its own threat model; report
bugs in trunk matching and dispatch-rule evaluation there.

## Security properties

- **Admission:** a call reaches a room only after the LiveKit server matched a
  trunk and a dispatch rule for it, digest authentication passed when the trunk
  requires it, and the PIN matched when the rule requires one. Every non-accept
  `AuthResult` and `DispatchResult` must end the call. A malformed or repeated
  INVITE must not skip a check that its first attempt would hit.
- **Digest authentication:** a response must verify only against the
  challenge this service issued for that call, with the trunk's credentials.
  A response for one call or trunk must not authenticate another.
- **Dialog isolation:** an in-dialog request (ACK, BYE, NOTIFY, re-INVITE)
  must affect only the dialog it belongs to. A request for one call must not
  end, re-route, or alter media for another call, trunk, or project.
- **Call isolation:** call state, media, DTMF, and participant attributes must
  stay with the call they came from. Credentials, room tokens, and project
  identifiers from one call must not reach another.
- **Media routing:** the RTP destination must change only through negotiated
  SDP, or through source latching when the operator enabled it. An off-path
  host must not redirect a call's outbound audio without that.
- **Robustness:** malformed SIP, SDP, sipfrag, Reason headers, RTP, and DTMF
  must not panic, deadlock, or leak goroutines, ports, or calls. One peer must
  not make the service allocate without bound, for example through many
  Call-IDs, un-ACKed INVITEs, auth challenges, or oversized headers mapped into
  attributes.
- **No header injection:** values from API requests, participant attributes,
  and inbound SIP headers that are copied into outbound messages must not add
  headers, start a new line, or change the request URI, From, To, or
  Refer-To.
- **TLS:** when SIP over TLS is configured, outbound connections must verify
  the peer's certificate chain against the system trust store, and the policy
  set by `min_version`, `max_version`, and `cipher_suites` must be enforced
  unless `warn_only` is set.
- **Secrets:** trunk passwords, PINs, digest responses, SRTP keys, and API
  secrets must not appear in logs, errors, call state, or participant
  attributes.

## Severity guidance

- **Critical:** joining a room without matching a trunk and dispatch rule, or
  without passing digest authentication or the PIN; reading or injecting
  another call's audio across trunks or projects.
- **High:** a remote crash of the service from one SIP message or RTP packet;
  ending, transferring, or re-routing another caller's call from an off-path
  host; SIP header injection from API or attribute fields; outbound TLS
  accepting a certificate chain that does not verify; trunk passwords or PINs
  in logs.
- **Medium:** unbounded memory, goroutine, or port use from one peer's
  traffic; leaked call state or ports that outlive a call; one caller's
  traffic denying service to other callers on the same node.
- **Low / not a finding:** behaviour that needs the API secret, the operator
  config, Redis, or a malicious LiveKit server; plain RTP being unauthenticated
  by design; source latching when the operator enabled `symmetric_rtp`, or
  enabled `ignore_local_addr_in_sdp` and the peer's SDP address is private; audio
  quality, jitter, or call-timing behaviour without a security impact.

## Testing

The checkout is at `/src`. Go dependencies are cached. `GOPROXY=off` and
`GOSUMDB=off` prevent downloads. The cgo libraries (libopus, libopusfile,
libsoxr) are installed. No external services are needed for the unit tests;
they bind SIP and RTP ports on local addresses.

Every `pkg/sip` test that starts the SIP service needs a non-loopback IPv4
address: sipgo resolves its own address from the host's interfaces and fails
with "server not connected to any network" when there is only `lo`. That covers
the `TestService_*`, `TestOutbound*`, `TestDigestAuth*`, and
`TestInboundSignalingEvents*` tests and the other tests in `signaling_test.go`
that call `NewServiceTest`: 40 of 116 in `pkg/sip`. The rest are unit tests and
pass offline.

`test/` holds end-to-end suites that need Docker, a live LiveKit server, or
credentials. They do not run here.

```sh
cd /src
SKIP='^(TestOutbound|TestService_|TestDigestAuth|TestSameCallIDForAuthFlow|TestInboundSignalingEvents|TestInbound(LateOffer|LateOfferDisabled|RoomIDReportedBeforeAnswer|RoomIDReportedOnAnsweredCall|CallStatusCode|ByeOverInviteConnection)$|TestCANCELSendsBothResponses$|TestReinvite$|TestRetransmission$|TestRouteSet$|TestTransfer$)'
# with only a loopback interface
go test -count=1 -timeout 30m -skip "$SKIP" ./pkg/...
go test -race -count=1 -timeout 30m -skip "$SKIP" ./pkg/sip/...
# with a non-loopback IPv4 interface
go test -count=1 -timeout 30m ./pkg/...
```

There are no fuzz tests. For parsers, add a `Fuzz` target in `pkg/sip`, for
example over `ParseReasonHeader`, `handleNotify`, `buildRawURI`, or
`HeadersToAttrs`, or, where a non-loopback IPv4 interface exists, drive
INVITEs through `NewServiceTest` in `signaling_test.go`.

## Reports

State the attacker's required access (none beyond reaching the SIP or RTP
port, a caller that matches a trunk, a trunk's digest credentials, a party
to the call, or an API caller), the entry point, and the demonstrated impact.
Provide a minimal reproducer, preferably a Go test in `pkg/sip` built on
`NewServiceTest`.
