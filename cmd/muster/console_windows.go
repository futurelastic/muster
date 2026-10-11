package main

import (
	"syscall"
	"unsafe"
)

// detachConsole frees the console when this process is the only one attached to
// it.
//
// muster is a console program. Started by the scheduler at log-on it is given a
// console window of its own, which would sit on the desktop for as long as the
// service runs. When the process is alone on its console nothing else can be
// using that window, so it is released: the window closes and the service keeps
// running. Started from a shell (or by anything that shares its console) there
// is more than one process on the console and it is left alone — a person
// running `muster serve` in a terminal keeps their terminal.
//
// What gets freed is only the console. A redirected stdout or stderr is a file
// or a pipe, not the console, and is unaffected; FLEET_LOG_FILE is how a
// service in this state is read at all.
func detachConsole() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	getList := k32.NewProc("GetConsoleProcessList")
	free := k32.NewProc("FreeConsole")
	if getList.Find() != nil || free.Find() != nil {
		return
	}
	// With room for two ids, a result of 1 means this process is the only one;
	// a larger result means others are attached (and nothing useful is in the
	// buffer). Zero means the call failed, e.g. there is no console: nothing to
	// free either way.
	var ids [2]uint32
	n, _, _ := getList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	if n == 1 {
		_, _, _ = free.Call()
	}
}
