package middleware

import "errors"

// errorAs is a generic wrapper so tests can call errors.As with a typed target
// without repeating the pointer dance.
func errorAs[T any](err error, target *T) bool {
	return errors.As(err, target)
}
