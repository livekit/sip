# LiveKit SIP Outbound call test

This CLI runs a E2E test for SIP outbound calls.

## Prerequisites

- Two LK servers with SIP: `LK1` (outbound) and `LK2` (inbound). Can be the same server.
- Two SIP phone numbers: `NUM1` (outbound) and `NUM2` (inbound).
- SIP Trunks associated with those numbers: `TR1` (for `NUM1`) and `TR2` (for `NUM2`).
- SIP Dispatch Rule for inbound: `DR2` (for `TR2` and `NUM2`). With optional `PIN`.
- Two LK rooms: `RM1` (on `LK1`) and `RM2` (on `LK2`).

## Twilio configuration

### Trunk config for TR1

```json
{
    "outbound_number": "<NUM1>",
    "outbound_address": "<project>.pstn.twilio.com",
    "outbound_username": "<Twilio username>",
    "outbound_password": "<Twilio password>"
}
```

### Trunk config for TR2

```json
{
    "inbound_addresses": [],
    "inbound_numbers": ["<NUM1>"],
    "outbound_number": "<NUM2>"
}
```

### Dispatch Rule config for DR2

```json
{
    "trunk_ids": ["<TR2>"],
    "rule": {
        "dispatch_rule_direct": {
            "room_name": "<RM2>",
            "pin": "<PIN>"
        }
    }
}
```

## LiveKit Cloud configuration

### Trunk config for TR1

```json
{
    "outbound_number": "<NUM1>",
    "outbound_address": "<project-id>.sip.livekit.cloud",
    "outbound_username": "<username>",
    "outbound_password": "<password>"
}
```

### Trunk config for TR2

```json
{
    "inbound_addresses": [],
    "inbound_numbers": ["<NUM1>"],
    "inbound_username": "<username>",
    "inbound_password": "<password>",
    "outbound_number": "<NUM2>"
}
```

### Dispatch Rule config for DR2

```json
{
    "trunk_ids": ["<TR2>"],
    "rule": {
        "dispatch_rule_direct": {
            "room_name": "<RM2>",
            "pin": "<PIN>"
        }
    }
}
```

## Running the test

```shell
go build .

./lktest-sip-outbound \
  --ws-out $LK1_URL --key-out $LK1_KEY --secret-out $LK1_SECRET \
  --ws-in $LK2_URL --key-in $LK2_KEY --secret-in $LK2_SECRET \
  --trunk-out $TR1 --number-out $NUM1 --room-out $RM1 \
  --number-in $NUM2 --room-in $RM2  --room-pin $PIN
```