package clientdaemon

import "fmt"

// errCode tags an error with the HTTP status the socket handler
// should return. The handler unwraps via errCoded; non-coded errors
// fall back to 500.
type errCode struct {
	status int
	msg    string
}

func (e *errCode) Error() string { return e.msg }

type errCoded interface {
	error
	HTTPStatus() int
}

func (e *errCode) HTTPStatus() int { return e.status }

func errBadRequest(msg string) error { return &errCode{status: 400, msg: msg} }
func errNotFound(msg string) error   { return &errCode{status: 404, msg: msg} }
func errQueueFull(msg string) error  { return &errCode{status: 503, msg: msg} }
func errDraining(msg string) error   { return &errCode{status: 503, msg: msg} }

// _ silences the "fmt unused" warning during incremental wiring;
// downstream files use fmt.
var _ = fmt.Errorf
