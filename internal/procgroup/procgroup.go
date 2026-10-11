// Package procgroup starts a child so that it, and everything it goes on to
// start, can be stopped together.
//
// Two callers need exactly this and used to carry a private copy each: the
// subprocess driver, which must not leave a session's tools running after the
// session closes, and the module launcher, which must not leave a module's
// descendants running after the module is killed. One implementation means one
// answer per platform.
//
// The shape is the same everywhere:
//
//	cmd := exec.Command(...)
//	procgroup.Prepare(cmd)           // before Start
//	err := cmd.Start()
//	g, err := procgroup.Attach(cmd)  // right after Start, before Wait
//	...
//	g.Interrupt()                    // only when g.Graceful()
//	g.Kill()                         // the whole tree
//	g.Close()                        // once, after the process has been waited
//
// Unix puts the child in its own process group and signals the group. Windows
// puts it in a Job Object created with kill-on-close: the child is created
// suspended, assigned to the job, and only then resumed, so nothing it starts
// can escape before the job holds it, and a service that dies without cleaning
// up leaves no survivors. Windows has no shared console to deliver a graceful
// stop through, so Graceful reports false there and a caller goes straight to
// Kill.
//
// Standard library only: the Windows calls go through kernel32.dll and
// advapi32.dll, loaded lazily.
package procgroup
