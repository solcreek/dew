//go:build linux

// mute-init is a deliberately unresponsive PID 1 for the smoke test's
// hang guard: it brings up nothing — no vsock transport, no dew-agent,
// no serial shell — reproducing the "guest never answers" condition
// that used to hang `dew run` until killed. It must keep running: PID 1
// exiting panics the kernel and would end the scenario early.
package main

func main() {
	select {}
}
