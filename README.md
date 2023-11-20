<!--BEGIN_BANNER_IMAGE-->
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="/.github/banner_dark.png">
  <source media="(prefers-color-scheme: light)" srcset="/.github/banner_light.png">
  <img style="width:100%;" alt="The LiveKit icon, the name of the repository and some sample code in the background." src="https://raw.githubusercontent.com/livekit/sip/main/.github/banner_light.png">
</picture>
<!--END_BANNER_IMAGE-->

# LiveKit SIP

<!--BEGIN_DESCRIPTION-->
WebRTC is proving to be a versatile and scalable transport protocol both for media ingestion and delivery. However, not all devices support WebRTC. SIP provides a way to bring SIP traffic into a LiveKit room.
<!--END_DESCRIPTION-->

## Capabilities

SIP is meant to be a full featured SIP bridge, not all features are implemented yet. Currently, the following features are supported:
- Dialing Out (Sending INVITEs)
- Dialing In (Accepting INVITEs)
- Digest Authentication
- Touch Tone (Sending and Reading DTMF)

If you need something added, we would love to hear from you!

## Documentation

### Workflow

To accept SIP traffic into your running SIP service, the workflow goes like this:

* create an SIP Trunk with `CreateSIPTrunk` API (to livekit-server)
* create an SIP Dispatch Rule with `CreateSIPDispatchRule` API (to livekit-server)
* SIP service receives a call
* SIP service connects to the LiveKit room and SIP caller is a participant

### Service Architecture

The SIP service and the LiveKit server communicate over Redis. Redis is also used as storage for the SIP session state. The SIP service must also expose a public IP address for remote SIP peers to connect to.

### Config

The SIP service takes a YAML config file:

```yaml
# required fields
api_key: livekit server api key. LIVEKIT_API_KEY env can be used instead
api_secret: livekit server api secret. LIVEKIT_API_SECRET env can be used instead
ws_url: livekit server websocket url. LIVEKIT_WS_URL env can be used instead
redis:
  address: must be the same redis address used by your livekit server
  username: redis username
  password: redis password
  db: redis db

# optional fields
health_port: if used, will open an http port for health checks
prometheus_port: port used to collect prometheus metrics. Used for autoscaling
log_level: debug, info, warn, or error (default info)
sip_port: port to listen and send SIP traffic (default 5060)
rtp_port: port to listen and send RTP traffic (default 10000-20000)
```

The config file can be added to a mounted volume with its location passed in the SIP_CONFIG_FILE env var, or its body can be passed in the SIP_CONFIG_BODY env var.

### Using the SIP service

#### Creating Bridge and Dispatch Rule

Accepting SIP traffic requires two resources be created. First a `SIP Bridge`, then a `SIP Dispatch Rule`.

These resources can be created with any of the server SDKs or with the [livekit-cli](https://github.com/livekit/livekit-cli). The syntax with the livekit-cli is as follow:

The `SIP Bridge` is used to authenticate incoming traffic. Typically you will create a `SIP Bridge` to map to your different
SIP providers and their IP Ranges/Authentication details.

```shell
livekit-cli create-sip-bridge \
  --request <path to SIP Trunk creation request JSON file>
```

The SIP Bridge request creation JSON file uses the following syntax:

```json
{
    "inbound_addresses": Array of IP Address or CIDRs where SIP INVITEs will be accepted from
    "outbound_address": IP Address that SIP INVITEs will be sent too
    "outbound_number": When making an outbound call on this SIP Trunk what Phone Number should be used
    "inbound_numbers_regex": Phone numbers this SIP Trunk will serve. If Empty it will serve all incoming calls,
    "inbound_user": Username for Authentication of inbound calls, no Authentication if empty,
    "inbound_password": Password for Authentication of inbound calls, no Authentication if empty,
    "outbound_user": Username for Authentication of outbound calls, no Authentication if empty,
    "outbound_password": Password for Authentication of outbound calls, no Authentication if empty
}
```

On success, `livekit-cli` will return the unique id for the SIP Trunk.

Next a `SIP Dispatch Rule` needs to be created. A `SIP Dispatch Rule` determines what LiveKit room an incoming call should be directed into. You can direct calls into
different rooms depending on the metadata of the call. Things like who is calling, who they called and what pin did they enter.

```shell
livekit-cli create-sip-dispatch-rule \
  --request <path to SIP Distpach Rule creation request JSON file>
```

The SIP Bridge request creation JSON file uses the following syntax:

```json
{
    "rule": // What rule to use to dispatch this call, see the next section for rules
    "trunk_ids": // Array of SIP Trunk IDs that are accepted for this rule. If empty all Trunks are accepted
    "hide_phone_number": // If true hide the phone number when joining the LiveKit room
}
```

At this time we support one rule `dispatchRuleDirect`. This can be set like so

```
	"rule": {
		"dispatchRuleDirect": {
			"roomName":"my-new-room"
		}
	}
```


On success, `livekit-cli` will return the unique id for the SIP Dispatch Rule.

### Running locally

#### Running natively

The SIP service can be run natively on any platform supported by libpous.

##### Prerequisites

The SIP service is built in Go. Go >= 1.18 is needed. The SIP services uses [libopus](https://opus-codec.org/) and must be installed externally:

For Debian

```
sudo apt-get install pkg-config libopus-dev libopusfile-dev
```

For Mac

```
brew install pkg-config opus opusfile

```

For more instructions see [hraban/opus' README](https://github.com/hraban/opus#build--installation)

##### Building

Build the SIP service by running:

```shell
mage build
````

##### Running the service

To run against a local LiveKit server, a redis server must be running locally. All servers must be configured to communicate over localhost. Create a file named `config.yaml` with the following content:

```yaml
log_level: debug
api_key: <your-api-key>
api_secret: <your-api-secret>
ws_url: ws://localhost:7880
redis:
  address: localhost:6379
```

```shell
sip --config=config.yaml
```

#### Running with Docker

To run against a local LiveKit server, a Redis server must be running locally. The SIP service must be instructed to connect to LiveKit server and Redis on the host. The host network is accessible from within the container on IP:
- host.docker.internal on MacOS and Windows
- 172.17.0.1 on linux

Create a file named `config.yaml` with the following content:

```yaml
log_level: debug
api_key: <your-api-key>
api_secret: <your-api-secret>
ws_url: ws://host.docker.internal:7880 (or ws://172.17.0.1:7880 on linux)
redis:
  address: host.docker.internal:6379 (or 172.17.0.1:6379 on linux)
```

The container must be run with host networking enabled. SIP by default uses UDP port 1000 -> 2000 and 5060, this large range of ports is hard for docker to handle at this time.

Then to run the service:

```shell
docker run --rm \
    -e SIP_CONFIG_BODY="`cat config.yaml`" \
    --network host \
    livekit/sip
```



<!--BEGIN_REPO_NAV-->
<br/><table>
<thead><tr><th colspan="2">LiveKit Ecosystem</th></tr></thead>
<tbody>
<tr><td>Real-time SDKs</td><td><a href="https://github.com/livekit/components-js">React Components</a> · <a href="https://github.com/livekit/client-sdk-js">JavaScript</a> · <a href="https://github.com/livekit/client-sdk-swift">iOS/macOS</a> · <a href="https://github.com/livekit/client-sdk-android">Android</a> · <a href="https://github.com/livekit/client-sdk-flutter">Flutter</a> · <a href="https://github.com/livekit/client-sdk-react-native">React Native</a> · <a href="https://github.com/livekit/client-sdk-rust">Rust</a> · <a href="https://github.com/livekit/client-sdk-python">Python</a> · <a href="https://github.com/livekit/client-sdk-unity-web">Unity (web)</a> · <a href="https://github.com/livekit/client-sdk-unity">Unity (beta)</a></td></tr><tr></tr>
<tr><td>Server APIs</td><td><a href="https://github.com/livekit/server-sdk-js">Node.js</a> · <a href="https://github.com/livekit/server-sdk-go">Golang</a> · <a href="https://github.com/livekit/server-sdk-ruby">Ruby</a> · <a href="https://github.com/livekit/server-sdk-kotlin">Java/Kotlin</a> · <a href="https://github.com/livekit/client-sdk-python">Python</a> · <a href="https://github.com/livekit/client-sdk-rust">Rust</a> · <a href="https://github.com/agence104/livekit-server-sdk-php">PHP (community)</a></td></tr><tr></tr>
<tr><td>Agents Frameworks</td><td><a href="https://github.com/livekit/python-agents">Python</a></td></tr><tr></tr>
<tr><td>Services</td><td><a href="https://github.com/livekit/livekit">Livekit server</a> · <a href="https://github.com/livekit/egress">Egress</a> · <a href="https://github.com/livekit/ingress">Ingress</a> · <b>SIP</b></td></tr><tr></tr>
<tr><td>Resources</td><td><a href="https://docs.livekit.io">Docs</a> · <a href="https://github.com/livekit-examples">Example apps</a> · <a href="https://livekit.io/cloud">Cloud</a> · <a href="https://docs.livekit.io/oss/deployment">Self-hosting</a> · <a href="https://github.com/livekit/livekit-cli">CLI</a></td></tr>
</tbody>
</table>
<!--END_REPO_NAV-->
