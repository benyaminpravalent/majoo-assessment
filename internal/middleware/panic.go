package middleware

import (
	"fmt"
	"runtime"
)

// stackTraceBytes bounds the captured stack. Deep stacks past this point are
// almost always framework frames that add nothing to the diagnosis.
const stackTraceBytes = 8 << 10

// stackTrace captures the current goroutine's stack for the panic log.
func stackTrace() string {
	buf := make([]byte, stackTraceBytes)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// errFromPanic turns a recovered value into an error so it can be wrapped and
// carried by apierr.Internal.
func errFromPanic(rec any) error {
	if err, ok := rec.(error); ok {
		return fmt.Errorf("panic: %w", err)
	}
	return fmt.Errorf("panic: %v", rec)
}
