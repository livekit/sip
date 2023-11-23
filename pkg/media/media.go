package media

type Writer[T any] interface {
	WriteSample(sample T) error
}

type WriterFunc[T any] func(in T) error

func (fnc WriterFunc[T]) WriteSample(in T) error {
	return fnc(in)
}
