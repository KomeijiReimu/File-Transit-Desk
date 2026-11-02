package server

type requestCleanupKey uint8

const (
	admissionCleanupKey requestCleanupKey = iota
	downloadCleanupKey
)

// fasthttp closes user values after handling the response, including send errors.
// This tracks server-side response processing, not client acknowledgement, and
// leaves the original body stream (including its WriteTo fast path) untouched.
type requestCleanup func()

func (finish requestCleanup) Close() error {
	finish()
	return nil
}
