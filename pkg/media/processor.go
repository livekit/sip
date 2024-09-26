package media

type Processor[T any] interface {
	SetWriter(writer WriteCloser[T])
	WriteCloser[T]
}

type PCM16Processor = Processor[PCM16Sample]
