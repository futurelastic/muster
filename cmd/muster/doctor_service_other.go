//go:build !windows

package main

// checkServiceTask has no row to add outside Windows: there is no Task
// Scheduler, and the service manager there is the operator's own.
func checkServiceTask(getenv func(string) string) (doctorRow, bool) {
	return doctorRow{}, false
}
