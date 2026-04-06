package middleware

type Err interface {
	GetErrors() <-chan error
}
