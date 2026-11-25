package session

import "fmt"

// Error codes returned to MCP clients.
const (
	CodeInvalidArgument = "invalid_argument"
	CodeNotFound        = "not_found"
	CodeLimitReached    = "limit_reached"
	CodeSpawnFailed     = "spawn_failed"
	CodeNotAllowed      = "not_allowed"
	CodeBusy            = "busy"
	CodeSessionExited   = "session_exited"
	CodeUnsupported     = "unsupported"
	CodeInternal        = "internal"
)

// Error is a coded error. Its text is "<code>: <message>".
type Error struct {
	Code    string
	Message string
	// Result carries any remaining output for session_exited errors.
	Result *Result
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) withResult(r *Result) *Error { e.Result = r; return e }

// Errorf builds a coded error.
func Errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}
