package lksdk

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestAppendParseRoundTrip_TimestampOnly(t *testing.T) {
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	meta := FrameMetadata{UserTimestamp: 1_700_000_000_000_000}

	result := appendPacketTrailer(payload, meta)

	if !bytes.HasPrefix(result, payload) {
		t.Fatal("original payload not preserved as prefix")
	}
	if !bytes.HasSuffix(result, []byte(packetTrailerMagic)) {
		t.Fatal("magic bytes missing at end")
	}
	if len(result) != len(payload)+packetTrailerMinSize {
		t.Fatalf("expected len %d, got %d", len(payload)+packetTrailerMinSize, len(result))
	}

	got, ok := parsePacketTrailer(result)
	if !ok {
		t.Fatal("parsePacketTrailer returned false")
	}
	if got.UserTimestamp != meta.UserTimestamp {
		t.Fatalf("timestamp mismatch: got %d, want %d", got.UserTimestamp, meta.UserTimestamp)
	}
	if got.FrameId != 0 {
		t.Fatalf("expected frame_id 0, got %d", got.FrameId)
	}
}

func TestAppendParseRoundTrip_WithFrameId(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	meta := FrameMetadata{UserTimestamp: 42, FrameId: 12345}

	result := appendPacketTrailer(payload, meta)

	if len(result) != len(payload)+packetTrailerMaxSize {
		t.Fatalf("expected len %d, got %d", len(payload)+packetTrailerMaxSize, len(result))
	}

	got, ok := parsePacketTrailer(result)
	if !ok {
		t.Fatal("parsePacketTrailer returned false")
	}
	if got.UserTimestamp != 42 {
		t.Fatalf("timestamp mismatch: got %d, want 42", got.UserTimestamp)
	}
	if got.FrameId != 12345 {
		t.Fatalf("frame_id mismatch: got %d, want 12345", got.FrameId)
	}
}

func TestAppendParseRoundTrip_ZeroTimestampWithFrameId(t *testing.T) {
	// With UserTimestamp==0 the timestamp TLV is omitted, but a non-zero
	// FrameId still produces a valid trailer.
	payload := []byte{0xFF, 0xFF}
	meta := FrameMetadata{UserTimestamp: 0, FrameId: 99}

	result := appendPacketTrailer(payload, meta)
	got, ok := parsePacketTrailer(result)
	if !ok {
		t.Fatal("parsePacketTrailer returned false for zero timestamp with frame_id")
	}
	if got.UserTimestamp != 0 {
		t.Fatalf("expected timestamp 0, got %d", got.UserTimestamp)
	}
	if got.FrameId != 99 {
		t.Fatalf("expected frame_id 99, got %d", got.FrameId)
	}
}

func TestAppendPacketTrailer_AllZero_NoTrailer(t *testing.T) {
	// With both UserTimestamp==0 and FrameId==0 the input must be
	// returned unchanged (no trailer appended).
	payload := []byte{0x11, 0x22, 0x33}
	result := appendPacketTrailer(payload, FrameMetadata{})

	if !bytes.Equal(result, payload) {
		t.Fatalf("expected unchanged payload, got %x", result)
	}
	if _, ok := parsePacketTrailer(result); ok {
		t.Fatal("parsePacketTrailer should fail on untrailered data")
	}
}

func TestAppendParseRoundTrip_MaxTimestamp(t *testing.T) {
	payload := []byte{0xAA}
	meta := FrameMetadata{UserTimestamp: ^uint64(0)}

	result := appendPacketTrailer(payload, meta)
	got, ok := parsePacketTrailer(result)
	if !ok {
		t.Fatal("parsePacketTrailer returned false")
	}
	if got.UserTimestamp != ^uint64(0) {
		t.Fatalf("timestamp mismatch: got %d, want %d", got.UserTimestamp, ^uint64(0))
	}
}

func TestAppendParseRoundTrip_MaxFrameId(t *testing.T) {
	payload := []byte{0x00}
	meta := FrameMetadata{UserTimestamp: 1, FrameId: 0xFFFFFFFF}

	result := appendPacketTrailer(payload, meta)
	got, ok := parsePacketTrailer(result)
	if !ok {
		t.Fatal("parsePacketTrailer returned false")
	}
	if got.FrameId != 0xFFFFFFFF {
		t.Fatalf("frame_id mismatch: got %d, want %d", got.FrameId, uint32(0xFFFFFFFF))
	}
}

func TestAppendParseRoundTrip_EmptyPayload(t *testing.T) {
	meta := FrameMetadata{UserTimestamp: 7, FrameId: 3}

	result := appendPacketTrailer(nil, meta)
	got, ok := parsePacketTrailer(result)
	if !ok {
		t.Fatal("parsePacketTrailer returned false on empty payload")
	}
	if got.UserTimestamp != 7 || got.FrameId != 3 {
		t.Fatalf("metadata mismatch: got %+v", got)
	}
}

func TestAppendPacketTrailer_WireFormat_TimestampOnly(t *testing.T) {
	payload := []byte{0xDE, 0xAD}
	meta := FrameMetadata{UserTimestamp: 0x0102030405060708}

	result := appendPacketTrailer(payload, meta)

	// Expected: payload(2) + timestamp TLV(10) + envelope(5) = 17 bytes
	if len(result) != 17 {
		t.Fatalf("expected len 17, got %d", len(result))
	}

	// Original payload preserved
	if result[0] != 0xDE || result[1] != 0xAD {
		t.Fatalf("payload corrupted: %x", result[:2])
	}

	off := 2 // start of trailer

	// Timestamp TLV: tag
	if result[off] != byte(tagUserTimestamp)^0xFF {
		t.Fatalf("timestamp tag: got %02x, want %02x", result[off], byte(tagUserTimestamp)^0xFF)
	}
	// Timestamp TLV: length
	if result[off+1] != 8^0xFF {
		t.Fatalf("timestamp len: got %02x, want %02x", result[off+1], byte(8^0xFF))
	}
	// Timestamp TLV: value (each byte XORed with 0xFF)
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], meta.UserTimestamp)
	for i := 0; i < 8; i++ {
		want := tsBuf[i] ^ 0xFF
		if result[off+2+i] != want {
			t.Fatalf("timestamp byte %d: got %02x, want %02x", i, result[off+2+i], want)
		}
	}
	off += timestampTlvSize

	// Envelope: trailer_len XORed
	wantTrailerLen := byte(timestampTlvSize + trailerEnvelopeSize)
	if result[off] != wantTrailerLen^0xFF {
		t.Fatalf("trailer_len: got %02x, want %02x", result[off], wantTrailerLen^0xFF)
	}

	// Envelope: magic raw
	if string(result[off+1:]) != packetTrailerMagic {
		t.Fatalf("magic: got %q, want %q", result[off+1:], packetTrailerMagic)
	}
}

func TestAppendPacketTrailer_WireFormat_WithFrameId(t *testing.T) {
	payload := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	meta := FrameMetadata{UserTimestamp: 1000, FrameId: 0xAABBCCDD}

	result := appendPacketTrailer(payload, meta)

	// Expected: payload(4) + timestamp TLV(10) + frame_id TLV(6) + envelope(5) = 25
	if len(result) != 25 {
		t.Fatalf("expected len 25, got %d", len(result))
	}

	off := len(payload)

	// Timestamp TLV
	if result[off] != byte(tagUserTimestamp)^0xFF {
		t.Fatalf("timestamp tag: got %02x", result[off])
	}
	off += timestampTlvSize

	// Frame ID TLV: tag
	if result[off] != byte(tagFrameId)^0xFF {
		t.Fatalf("frame_id tag: got %02x, want %02x", result[off], byte(tagFrameId)^0xFF)
	}
	// Frame ID TLV: length
	if result[off+1] != 4^0xFF {
		t.Fatalf("frame_id len: got %02x, want %02x", result[off+1], byte(4^0xFF))
	}
	// Frame ID TLV: value
	var fidBuf [4]byte
	binary.BigEndian.PutUint32(fidBuf[:], meta.FrameId)
	for i := 0; i < 4; i++ {
		want := fidBuf[i] ^ 0xFF
		if result[off+2+i] != want {
			t.Fatalf("frame_id byte %d: got %02x, want %02x", i, result[off+2+i], want)
		}
	}
	off += frameIdTlvSize

	// Envelope: trailer_len
	wantTrailerLen := byte(timestampTlvSize + frameIdTlvSize + trailerEnvelopeSize)
	if result[off] != wantTrailerLen^0xFF {
		t.Fatalf("trailer_len: got %02x, want %02x", result[off], wantTrailerLen^0xFF)
	}
	if string(result[off+1:]) != packetTrailerMagic {
		t.Fatalf("magic: got %q", result[off+1:])
	}
}

func TestAppendPacketTrailer_FrameIdZero_Omitted(t *testing.T) {
	meta := FrameMetadata{UserTimestamp: 1, FrameId: 0}
	result := appendPacketTrailer(nil, meta)

	// With FrameId==0 the frame_id TLV must be absent.
	if len(result) != packetTrailerMinSize {
		t.Fatalf("expected len %d (no frame_id TLV), got %d", packetTrailerMinSize, len(result))
	}

	// The only tag present should be tagUserTimestamp.
	if result[0]^0xFF != tagUserTimestamp {
		t.Fatalf("first tag: got %02x, want timestamp tag", result[0]^0xFF)
	}
}

func TestAppendPacketTrailer_DoesNotMutateInput(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04}
	frozen := make([]byte, len(original))
	copy(frozen, original)

	_ = appendPacketTrailer(original, FrameMetadata{UserTimestamp: 42, FrameId: 7})

	if !bytes.Equal(original, frozen) {
		t.Fatalf("input slice was mutated: got %x, want %x", original, frozen)
	}
}

func TestAppendPacketTrailer_NilPayload(t *testing.T) {
	result := appendPacketTrailer(nil, FrameMetadata{UserTimestamp: 99})
	if len(result) != packetTrailerMinSize {
		t.Fatalf("expected len %d, got %d", packetTrailerMinSize, len(result))
	}
	if string(result[len(result)-4:]) != packetTrailerMagic {
		t.Fatal("magic missing")
	}
}

func TestStripPacketTrailer(t *testing.T) {
	payload := []byte{0x10, 0x20, 0x30, 0x40}
	meta := FrameMetadata{UserTimestamp: 100, FrameId: 200}

	result := appendPacketTrailer(payload, meta)
	stripped := stripPacketTrailer(result)

	if !bytes.Equal(stripped, payload) {
		t.Fatalf("stripped data mismatch: got %x, want %x", stripped, payload)
	}
}

func TestStripPacketTrailer_NoTrailer(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03}
	stripped := stripPacketTrailer(data)
	if !bytes.Equal(stripped, data) {
		t.Fatal("stripPacketTrailer should return original data when no trailer")
	}
}

func TestParsePacketTrailer_NoMagic(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	_, ok := parsePacketTrailer(data)
	if ok {
		t.Fatal("expected false for data without magic")
	}
}

func TestParsePacketTrailer_TooShort(t *testing.T) {
	_, ok := parsePacketTrailer([]byte{0x01})
	if ok {
		t.Fatal("expected false for data shorter than envelope")
	}

	_, ok = parsePacketTrailer(nil)
	if ok {
		t.Fatal("expected false for nil data")
	}
}

func TestParsePacketTrailer_BadTrailerLen(t *testing.T) {
	// Craft a buffer that has magic but an invalid trailer_len (too large).
	buf := []byte{0x00, 0x00, 0x00}       // padding
	buf = append(buf, byte(0)^byte(0xFF)) // trailer_len = 0 (below minimum)
	buf = append(buf, packetTrailerMagic...)

	_, ok := parsePacketTrailer(buf)
	if ok {
		t.Fatal("expected false for trailer_len below minimum")
	}
}

func TestNoNALStartCodes(t *testing.T) {
	// Verify the trailer never contains 0x000001 or 0x00000001 sequences,
	// which would confuse H.264/H.265 parsers.
	testCases := []FrameMetadata{
		{UserTimestamp: 0, FrameId: 0},
		{UserTimestamp: 0, FrameId: 1},
		{UserTimestamp: 1, FrameId: 0},
		{UserTimestamp: 0x00000001, FrameId: 0x00000001},
		{UserTimestamp: 0x0000000100000001, FrameId: 0},
		{UserTimestamp: ^uint64(0)},
	}

	for _, meta := range testCases {
		result := appendPacketTrailer(nil, meta)
		for i := 0; i+2 < len(result); i++ {
			if result[i] == 0x00 && result[i+1] == 0x00 {
				if i+2 < len(result) && result[i+2] == 0x01 {
					t.Fatalf("found 3-byte start code at offset %d for meta %+v: %x", i, meta, result)
				}
				if i+3 < len(result) && result[i+2] == 0x00 && result[i+3] == 0x01 {
					t.Fatalf("found 4-byte start code at offset %d for meta %+v: %x", i, meta, result)
				}
			}
		}
	}
}

func TestCrossCompatWithRustAppendTrailer(t *testing.T) {
	// Build a trailer byte-for-byte matching what the C++/Rust AppendTrailer
	// produces, then verify our Go parser can decode it.
	userTs := uint64(1_700_000_000_000_000)
	frameId := uint32(42)

	var trailer []byte

	// TLV: timestamp (tag=0x01 ^ 0xFF, len=8 ^ 0xFF, 8-byte BE value ^ 0xFF)
	trailer = append(trailer, 0x01^0xFF)
	trailer = append(trailer, 8^0xFF)
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], userTs)
	for _, b := range tsBuf {
		trailer = append(trailer, b^0xFF)
	}

	// TLV: frame_id (tag=0x02 ^ 0xFF, len=4 ^ 0xFF, 4-byte BE value ^ 0xFF)
	trailer = append(trailer, 0x02^0xFF)
	trailer = append(trailer, 4^0xFF)
	var fidBuf [4]byte
	binary.BigEndian.PutUint32(fidBuf[:], frameId)
	for _, b := range fidBuf {
		trailer = append(trailer, b^0xFF)
	}

	// Envelope: trailer_len ^ 0xFF, then raw magic
	totalTrailerLen := byte(len(trailer) + trailerEnvelopeSize)
	trailer = append(trailer, totalTrailerLen^0xFF)
	trailer = append(trailer, packetTrailerMagic...)

	payload := []byte{0xDE, 0xAD}
	data := append(payload, trailer...)

	got, ok := parsePacketTrailer(data)
	if !ok {
		t.Fatal("parsePacketTrailer failed on hand-crafted Rust-compatible trailer")
	}
	if got.UserTimestamp != userTs {
		t.Fatalf("timestamp mismatch: got %d, want %d", got.UserTimestamp, userTs)
	}
	if got.FrameId != frameId {
		t.Fatalf("frame_id mismatch: got %d, want %d", got.FrameId, frameId)
	}

	stripped := stripPacketTrailer(data)
	if !bytes.Equal(stripped, payload) {
		t.Fatalf("stripped mismatch: got %x, want %x", stripped, payload)
	}
}

func TestParseSkipsUnknownTags(t *testing.T) {
	// Build a trailer with an unknown tag (0x99) inserted between
	// the timestamp and frame_id TLVs. The parser should skip it.
	var trailer []byte

	// TLV: timestamp
	trailer = append(trailer, 0x01^0xFF, 8^0xFF)
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], 777)
	for _, b := range tsBuf {
		trailer = append(trailer, b^0xFF)
	}

	// TLV: unknown tag 0x99, len=2, value=0xAA,0xBB
	trailer = append(trailer, 0x99^0xFF, 2^0xFF, 0xAA^0xFF, 0xBB^0xFF)

	// TLV: frame_id
	trailer = append(trailer, 0x02^0xFF, 4^0xFF)
	var fidBuf [4]byte
	binary.BigEndian.PutUint32(fidBuf[:], 55)
	for _, b := range fidBuf {
		trailer = append(trailer, b^0xFF)
	}

	totalLen := byte(len(trailer) + trailerEnvelopeSize)
	trailer = append(trailer, totalLen^0xFF)
	trailer = append(trailer, packetTrailerMagic...)

	data := append([]byte{0x00}, trailer...)

	got, ok := parsePacketTrailer(data)
	if !ok {
		t.Fatal("parsePacketTrailer should succeed with unknown tags")
	}
	if got.UserTimestamp != 777 {
		t.Fatalf("timestamp mismatch: got %d, want 777", got.UserTimestamp)
	}
	if got.FrameId != 55 {
		t.Fatalf("frame_id mismatch: got %d, want 55", got.FrameId)
	}
}
