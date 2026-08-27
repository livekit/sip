package lksdk

type Private[T any] struct {
	v T
}

func MakePrivate[T any](v T) Private[T] {
	return Private[T]{v}
}
