//go:build darwin && arm64 && bingonative

package debugger

// DarwinTaskPortSendRefs reports the Mach send-right user-reference count on the
// tracee's cached task port (and whether it has been acquired yet). It is a
// darwin-only test hook: the port-hygiene regression test in test/integration
// runs against the public Debugger surface and cannot otherwise reach the
// unexported backend. A count that grows once per stop signals the Mach
// exception-receive path is leaking a task send right, which eventually hits
// KERN_UREFS_OVERFLOW and wedges Wait (see backend_darwin_arm64.go). Returns
// (0, false) for a non-engine Debugger or before the task port is acquired.
func DarwinTaskPortSendRefs(d Debugger) (int, bool) {
	e, ok := d.(*engine)
	if !ok {
		return 0, false
	}
	b, ok := e.backend.(*darwinBackend)
	if !ok {
		return 0, false
	}
	return b.TaskPortSendRefs()
}
