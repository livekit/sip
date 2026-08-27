package oggreader

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	pageHeaderTypeBeginningOfStream = 0x02
	pageHeaderSignature             = "OggS"

	idPageSignature = "OpusHead"

	pageHeaderLen       = 27
	idPagePayloadLength = 19
)

var (
	errNilStream                 = errors.New("stream is nil")
	errBadIDPageSignature        = errors.New("bad header signature")
	errBadIDPageType             = errors.New("wrong header, expected beginning of stream")
	errBadIDPageLength           = errors.New("payload for id page must be 19 bytes")
	errBadIDPagePayloadSignature = errors.New("bad payload signature")
	errShortPageHeader           = errors.New("not enough data for payload header")
	errChecksumMismatch          = errors.New("expected and actual checksum do not match")
)

// This is a modified version of the oggreader package from github.com/pion/webrtc
// The original Pion package can only read OGG pages, and the payload can only be used if the page contains only one segment.
// And this specific segment must only contains one Opus Packet.
//
// RTP Packet can contains only one Opus Packet.
// https://www.rfc-editor.org/rfc/rfc7587#section-4.2
type OggReader struct {
	stream io.Reader

	page    *OggPage // current page
	segment uint8
	offset  int

	checksumTable *[256]uint32
	doChecksum    bool
}

// OggHeader is the metadata from the first two pages
// in the file (ID and Comment)
//
// https://tools.ietf.org/html/rfc7845.html#section-3
type OggHeader struct {
	ChannelMap uint8
	Channels   uint8
	OutputGain uint16
	PreSkip    uint16
	SampleRate uint32
	Version    uint8
}

// OggPageHeader is the metadata for a Page
// Pages are the fundamental unit of multiplexing in an Ogg stream
//
// https://tools.ietf.org/html/rfc7845.html#section-1
type OggPage struct {
	GranulePosition uint64

	sig           [4]byte
	version       uint8
	headerType    uint8
	serial        uint32
	index         uint32
	segmentsTable []uint8 // len(segmentsTable) == segmentsCount
	payload       []byte
}

// NewWith returns a new Ogg reader and Ogg header
// with an io.Reader input
func NewOggReader(in io.Reader) (*OggReader, *OggHeader, error) {
	return newWith(in /* doChecksum */, true)
}

func newWith(in io.Reader, doChecksum bool) (*OggReader, *OggHeader, error) {
	if in == nil {
		return nil, nil, errNilStream
	}

	reader := &OggReader{
		stream:        in,
		checksumTable: generateChecksumTable(),
		doChecksum:    doChecksum,
	}

	header, err := reader.readHeaders()
	if err != nil {
		return nil, nil, err
	}

	// ignore
	_, _ = reader.readPage()

	return reader, header, nil
}

func (o *OggReader) readHeaders() (*OggHeader, error) {
	page, err := o.readPage()
	if err != nil {
		return nil, err
	}

	header := &OggHeader{}
	if string(page.sig[:]) != pageHeaderSignature {
		return nil, errBadIDPageSignature
	}

	if page.headerType != pageHeaderTypeBeginningOfStream {
		return nil, errBadIDPageType
	}

	if len(page.payload) != idPagePayloadLength {
		return nil, errBadIDPageLength
	}

	if s := string(page.payload[:8]); s != idPageSignature {
		return nil, errBadIDPagePayloadSignature
	}

	header.Version = page.payload[8]
	header.Channels = page.payload[9]
	header.PreSkip = binary.LittleEndian.Uint16(page.payload[10:12])
	header.SampleRate = binary.LittleEndian.Uint32(page.payload[12:16])
	header.OutputGain = binary.LittleEndian.Uint16(page.payload[16:18])
	header.ChannelMap = page.payload[18]

	return header, nil
}

func (o *OggReader) readPage() (*OggPage, error) {
	h := make([]byte, pageHeaderLen)

	n, err := io.ReadFull(o.stream, h)
	if err != nil {
		return nil, err
	} else if n < len(h) {
		return nil, errShortPageHeader
	}

	page := &OggPage{
		sig: [4]byte{h[0], h[1], h[2], h[3]},
	}

	page.version = h[4]
	page.headerType = h[5]
	page.GranulePosition = binary.LittleEndian.Uint64(h[6 : 6+8])
	page.serial = binary.LittleEndian.Uint32(h[14 : 14+4])
	page.index = binary.LittleEndian.Uint32(h[18 : 18+4])

	segmentsCount := h[26]
	segmentsTable := make([]byte, segmentsCount)
	if _, err = io.ReadFull(o.stream, segmentsTable); err != nil {
		return nil, err
	}

	payloadSize := 0
	for _, s := range segmentsTable {
		payloadSize += int(s)
	}

	payload := make([]byte, payloadSize)
	if _, err = io.ReadFull(o.stream, payload); err != nil {
		return nil, err
	}

	if o.doChecksum {
		var checksum uint32
		updateChecksum := func(v byte) {
			checksum = (checksum << 8) ^ o.checksumTable[byte(checksum>>24)^v]
		}

		for index := range h {
			// Don't include expected checksum in our generation
			if index > 21 && index < 26 {
				updateChecksum(0)
				continue
			}

			updateChecksum(h[index])
		}
		for _, s := range segmentsTable {
			updateChecksum(s)
		}
		for index := range payload {
			updateChecksum(payload[index])
		}

		if binary.LittleEndian.Uint32(h[22:22+4]) != checksum {
			return nil, errChecksumMismatch
		}
	}

	page.payload = payload
	page.segmentsTable = segmentsTable

	return page, nil
}

// Pages can contain up to a second of audio data, which is much too large for an RTP packet.
// Instead of returning the entire page, this will return one sample at a time.
func (o *OggReader) ReadPacket() ([]byte, error) {
	page := o.page
	if page == nil {
		var err error
		o.page, err = o.readPage()
		if err != nil {
			return nil, err
		}

		page = o.page
		o.offset = 0
		o.segment = 0
	}

	// Calculate the size of the packet
	packetSize := 0
	for {
		segmentSize := page.segmentsTable[o.segment]
		packetSize += int(segmentSize)

		o.segment++
		if o.segment == uint8(len(page.segmentsTable)) {
			o.page = nil
			break
		}

		if segmentSize != 255 {
			break
		}
	}

	// Read the packet
	packet := make([]byte, packetSize)
	copy(packet, page.payload[o.offset:o.offset+packetSize])
	o.offset += packetSize

	return packet, nil
}

func generateChecksumTable() *[256]uint32 {
	var table [256]uint32
	const poly = 0x04c11db7

	for i := range table {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if (r & 0x80000000) != 0 {
				r = (r << 1) ^ poly
			} else {
				r <<= 1
			}
			table[i] = r & 0xffffffff
		}
	}
	return &table
}
