package lksdk

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"

	"github.com/frostbyte73/core"
	protocol "github.com/livekit/protocol/livekit"
)

type baseStreamInfo struct {
	Id         string
	MimeType   string
	Topic      string
	Timestamp  int64
	Size       *uint64
	Attributes map[string]string
}

// Info for byte streams
//   - Id is the id of the stream
//   - MimeType is the mime type of the stream, determined for SendFile if not provided
//   - Topic is the topic of the stream
//   - Timestamp is the timestamp of sending the stream
//   - Size is the total size of the stream, if provided
//   - Attributes are any additional attributes of the stream
//   - Name is the name of the file or stream, if provided
type ByteStreamInfo struct {
	*baseStreamInfo
	Name *string
}

// Info for text streams
//   - Id is the id of the stream
//   - MimeType is the mime type of the stream, always "text/plain" for text streams
//   - Topic is the topic of the stream
//   - Timestamp is the timestamp of sending the stream
//   - Size is the total size of the stream, if provided
//   - Attributes are any additional attributes of the stream
type TextStreamInfo struct {
	*baseStreamInfo
}

const (
	// default max chunk size for streams
	STREAM_CHUNK_SIZE = 15_000
)

// Options for publishing a text stream with mime type "text/plain"
//   - Topic is the topic of the stream
//   - DestinationIdentities is the list of identities that will receive the stream, empty for all participants
//   - StreamId is the id of the stream, generated if not provided
//   - ReplyToStreamId is the id of the stream to reply to, optional
//   - TotalSize is the total size of the stream, optional but calculated internally for SendText if not provided
//   - Attributes are any additional attributes of the stream
//   - OnProgress is a callback function that will be called when the stream is being written
//   - Attachments is the list of file paths to attach to the stream, optional
//   - AttachedStreamIds is the list of stream ids that are attached to this stream, mapped by index to attachments, optional, generated if not provided
type StreamTextOptions struct {
	Topic                 string
	DestinationIdentities []string
	StreamId              *string
	ReplyToStreamId       *string
	TotalSize             uint64
	Attributes            map[string]string
	OnProgress            func(progress float64)
	Attachments           []string
	AttachedStreamIds     []string
}

// Options for publishing a byte stream
//   - Topic is the topic of the stream
//   - MimeType is the mime type of the stream, determined for SendFile if not provided
//   - DestinationIdentities is the list of identities that will receive the stream, empty for all participants
//   - StreamId is the id of the stream, generated if not provided
//   - TotalSize is the total size of the stream, optional but calculated internally for SendFile
//   - Attributes are any additional attributes of the stream
//   - OnProgress is a callback function that will be called when the stream is being written
//   - FileName is the name of the file, optional
type StreamBytesOptions struct {
	Topic                 string
	MimeType              string
	DestinationIdentities []string
	StreamId              *string
	TotalSize             uint64
	Attributes            map[string]string
	OnProgress            func(progress float64)
	FileName              *string
}

// writeTask contains a list of chunks to be written to the stream
// and a callback function that will be called when the data provided is written to the stream
type writeTask struct {
	chunks [][]byte
	onDone *func()
}

type baseStreamWriter[T any] struct {
	engine                *RTCEngine
	streamId              string
	destinationIdentities []string
	totalSize             *uint64
	onProgress            func(progress float64)

	chunkIndex uint64
	closed     core.Fuse
	lock       sync.Mutex

	writeQueue chan writeTask
}

func newBaseStreamWriter[T any](engine *RTCEngine, header *protocol.DataStream_Header, streamId string, destinationIdentities []string, totalSize *uint64, onProgress func(progress float64)) *baseStreamWriter[T] {
	base := &baseStreamWriter[T]{
		engine:                engine,
		streamId:              streamId,
		destinationIdentities: destinationIdentities,
		totalSize:             totalSize,
		onProgress:            onProgress,
		writeQueue:            make(chan writeTask),
	}

	engine.publishStreamHeader(header, destinationIdentities)

	go base.processWriteQueue()
	return base
}

// processes write queue asynchronously
func (w *baseStreamWriter[T]) processWriteQueue() {
	for {
		select {
		case task := <-w.writeQueue:
			w.writeStreamBytes(task.chunks, task.onDone)
		case <-w.closed.Watch():
			// Drain any pending tasks - close sends a trailer and no data should be sent after that
			for {
				select {
				case <-w.writeQueue:
				default:
					return
				}
			}
		}
	}
}

// Write data to the stream, data can be a byte slice or a string
// depending on the type of the stream writer
// onDone is a callback function that will be called when the data provided is written to the stream
func (w *baseStreamWriter[T]) Write(data T, onDone *func()) {
	if w.closed.IsBroken() {
		return
	}

	var task writeTask
	switch v := any(data).(type) {
	case []byte:
		task = writeTask{
			chunks: chunkBytes(v),
			onDone: onDone,
		}
	case string:
		task = writeTask{
			chunks: chunkUtf8String(v),
			onDone: onDone,
		}
	default:
		return
	}

	select {
	case w.writeQueue <- task:
	case <-w.closed.Watch():
	}
}

// Close the stream, this will send a stream trailer to notify the receiver that the stream is closed
func (w *baseStreamWriter[T]) Close() {
	w.closed.Once(func() {
		w.lock.Lock()
		w.engine.publishStreamTrailer(w.streamId, w.destinationIdentities)
		w.lock.Unlock()
	})
}

// writes a list of chunks to the stream
func (w *baseStreamWriter[T]) writeStreamBytes(chunks [][]byte, onDone *func()) {
	w.lock.Lock()
	chunkIndex := w.chunkIndex

	for i := 0; i < len(chunks) && !w.closed.IsBroken(); i++ {
		chunk := chunks[i]

		w.engine.waitForBufferStatusLow(protocol.DataPacket_RELIABLE)

		w.engine.publishStreamChunk(&protocol.DataStream_Chunk{
			StreamId:   w.streamId,
			Content:    chunk,
			ChunkIndex: chunkIndex,
		}, w.destinationIdentities)

		if w.onProgress != nil && w.totalSize != nil {
			progress := float64(len(chunk)) / float64(*w.totalSize)
			w.onProgress(progress)
		}

		chunkIndex++
	}

	w.chunkIndex = chunkIndex
	w.lock.Unlock()

	if onDone != nil {
		(*onDone)()
	}
}

// TextStreamWriter is a writer type for text streams
type TextStreamWriter struct {
	*baseStreamWriter[string]
	Info TextStreamInfo
}

// create a new text stream writer
func newTextStreamWriter(info TextStreamInfo, header *protocol.DataStream_Header, e *RTCEngine, destinationIdentities []string, onProgress func(progress float64)) *TextStreamWriter {
	return &TextStreamWriter{
		baseStreamWriter: newBaseStreamWriter[string](e, header, info.Id, destinationIdentities, info.Size, onProgress),
		Info:             info,
	}
}

// ByteStreamWriter is a writer type for byte streams
type ByteStreamWriter struct {
	*baseStreamWriter[[]byte]
	Info ByteStreamInfo
}

// create a new byte stream writer
func newByteStreamWriter(info ByteStreamInfo, header *protocol.DataStream_Header, e *RTCEngine, destinationIdentities []string, onProgress func(progress float64)) *ByteStreamWriter {
	return &ByteStreamWriter{
		baseStreamWriter: newBaseStreamWriter[[]byte](e, header, info.Id, destinationIdentities, info.Size, onProgress),
		Info:             info,
	}
}

type baseStreamReader struct {
	readBuffer    bytes.Buffer
	totalByteSize *uint64
	bytesReceived int

	closed atomic.Bool
	lock   sync.Mutex
	cond   *sync.Cond

	onProgress *func(progress float64)
}

func newBaseStreamReader(totalByteSize *uint64) *baseStreamReader {
	baseReader := &baseStreamReader{
		bytesReceived: 0,
	}
	if totalByteSize != nil {
		baseReader.totalByteSize = totalByteSize
	}
	baseReader.cond = sync.NewCond(&baseReader.lock)
	return baseReader
}

// writes a chunk to the read buffer
func (r *baseStreamReader) enqueue(chunk *protocol.DataStream_Chunk) {
	if r.closed.Load() {
		return
	}
	// write seems to handle growing the buffer if needed
	r.lock.Lock()
	r.readBuffer.Write(chunk.Content)
	r.cond.Broadcast()
	r.lock.Unlock()
}

// OnProgress sets the callback function that will be called when the stream is being read
// only called if TotalSize of the stream is set
func (r *baseStreamReader) OnProgress(onProgress *func(progress float64)) {
	r.onProgress = onProgress
}

// calls the OnProgress callback if the total size of the stream is set
func (r *baseStreamReader) maybeCallOnProgress(n int) {
	r.bytesReceived += n

	if r.totalByteSize != nil && r.onProgress != nil {
		currentProgress := float64(r.bytesReceived) / float64(*r.totalByteSize)
		(*r.onProgress)(currentProgress)
	}
}

// waits for a write to the stream
func (r *baseStreamReader) waitForData() {
	// if stream is closed, the methods will return io.EOF automatically
	for r.readBuffer.Len() == 0 && !r.closed.Load() {
		r.cond.Wait()
	}
}

// handles the EOF error before the stream is closed
func (r *baseStreamReader) handleEOFBeforeStreamClosed(err error) error {
	if err == io.EOF {
		if r.closed.Load() {
			return io.EOF
		} else {
			return nil
		}
	}
	return err
}

// Read reads the next len(p) bytes from the stream or until the stream buffer is drained.
// The return value is the number of bytes read.
// If the buffer has no data to return, it will wait for a write to the stream or return io.EOF if the stream is closed.
func (r *baseStreamReader) Read(bytes []byte) (int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.waitForData()

	n, err := r.readBuffer.Read(bytes)
	r.maybeCallOnProgress(n)
	return n, r.handleEOFBeforeStreamClosed(err)
}

// ReadByte reads and returns the next byte from the buffer.
// If no byte is available, it will wait for a write to the stream or return io.EOF if the stream is closed.
func (r *baseStreamReader) ReadByte() (byte, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.waitForData()

	n, err := r.readBuffer.ReadByte()
	r.maybeCallOnProgress(1)
	return n, r.handleEOFBeforeStreamClosed(err)
}

// ReadBytes reads until the first occurrence of delim in the input, returning a slice containing the data up to and including the delimiter.
// If ReadBytes encounters an error before finding a delimiter, it returns the data read before EOF, but does not return EOF until the stream is closed.
// ReadBytes returns err != nil if and only if the returned data does not end in delim.
func (r *baseStreamReader) ReadBytes(delim byte) ([]byte, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.waitForData()

	n, err := r.readBuffer.ReadBytes(delim)
	r.maybeCallOnProgress(len(n))
	return n, r.handleEOFBeforeStreamClosed(err)
}

func (r *baseStreamReader) close() {
	if !r.closed.Load() {
		r.closed.Store(true)

		r.lock.Lock()
		r.cond.Broadcast()
		r.lock.Unlock()
	}
}

type TextStreamReader struct {
	*baseStreamReader
	Info TextStreamInfo
}

func NewTextStreamReader(info TextStreamInfo, totalChunkCount *uint64) *TextStreamReader {
	return &TextStreamReader{
		baseStreamReader: newBaseStreamReader(totalChunkCount),
		Info:             info,
	}
}

// ReadRune reads and returns the next UTF-8-encoded Unicode code point from the buffer.
// If no bytes are available, it will wait for a write to the stream or return io.EOF if the stream is closed.
// If the bytes are an erroneous UTF-8 encoding, it consumes one byte and returns U+FFFD, 1.
func (r *TextStreamReader) ReadRune() (rune, int, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.waitForData()

	n, size, err := r.readBuffer.ReadRune()
	r.maybeCallOnProgress(size)
	return n, size, r.handleEOFBeforeStreamClosed(err)
}

// ReadString reads until the first occurrence of delim in the input, returning a string containing the data up to and including the delimiter.
// If ReadString encounters an error before finding a delimiter, it returns the data read before EOF, but does not return EOF until the stream is closed.
// ReadString returns err != nil if and only if the returned data does not end in delim.
func (r *TextStreamReader) ReadString(delim byte) (string, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.waitForData()

	n, err := r.readBuffer.ReadString(delim)
	r.maybeCallOnProgress(len(n))
	return n, r.handleEOFBeforeStreamClosed(err)
}

// ReadAll reads all the data from the stream and returns it as a string.
// This will block until the stream is closed.
func (r *TextStreamReader) ReadAll() string {
	r.lock.Lock()
	defer r.lock.Unlock()

	// wait for the stream to be closed
	for !r.closed.Load() {
		r.cond.Wait()
	}

	// Now that the stream is closed, read all data
	n := r.readBuffer.String()
	return n
}

type ByteStreamReader struct {
	*baseStreamReader
	Info ByteStreamInfo
}

func NewByteStreamReader(info ByteStreamInfo, totalChunkCount *uint64) *ByteStreamReader {
	return &ByteStreamReader{
		baseStreamReader: newBaseStreamReader(totalChunkCount),
		Info:             info,
	}
}

// ReadAll reads all the data from the stream and returns it as a byte slice.
// This will block until the stream is closed.
func (r *ByteStreamReader) ReadAll() []byte {
	r.lock.Lock()
	defer r.lock.Unlock()

	// wait for the stream to be closed
	for !r.closed.Load() {
		r.cond.Wait()
	}

	// Now that the stream is closed, read all data
	n := r.readBuffer.Bytes()
	r.maybeCallOnProgress(len(n))
	return n
}

// TextStreamHandler is a function that will be called when a text stream is received.
// It will be called with the stream reader and the participant identity that sent the stream.
type TextStreamHandler func(reader *TextStreamReader, participantIdentity string)

// ByteStreamHandler is a function that will be called when a byte stream is received.
// It will be called with the stream reader and the participant identity that sent the stream.
type ByteStreamHandler func(reader *ByteStreamReader, participantIdentity string)

// ---------------------------------------------------------

// Helper function to chunk a utf8 string into chunks of a maximum size of STREAM_CHUNK_SIZE
func chunkUtf8String(s string) [][]byte {
	chunks := [][]byte{}
	stringBytes := []byte(s)

	for len(stringBytes) > STREAM_CHUNK_SIZE {
		k := STREAM_CHUNK_SIZE

		for k > 0 {
			dataByte := stringBytes[k]
			// check if the byte is not a continuation byte
			if (dataByte & 0xc0) != 0x80 {
				break
			}
			k--
		}

		// valid utf8 sequence found, add it to the chunks
		chunks = append(chunks, stringBytes[:k])
		stringBytes = stringBytes[k:]
	}

	if len(stringBytes) > 0 {
		chunks = append(chunks, stringBytes)
	}

	return chunks
}

// Helper function to chunk a byte slice into chunks of a maximum size of STREAM_CHUNK_SIZE
func chunkBytes(data []byte) [][]byte {
	chunks := [][]byte{}

	for len(data) > STREAM_CHUNK_SIZE {
		chunks = append(chunks, data[:STREAM_CHUNK_SIZE])
		data = data[STREAM_CHUNK_SIZE:]
	}

	if len(data) > 0 {
		chunks = append(chunks, data)
	}

	return chunks
}
