// Command supervisor runs a program and restarts it whenever it exits, so
// the chaos control can kill Cuboid with SIGKILL and show it resume exactly
// from its committed position.
//
//	supervisor -- cuboid serve
package main

import (
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		log.Fatal("usage: supervisor -- <command> [args]")
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	for restarts := 0; ; restarts++ {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		start := time.Now()
		if err := cmd.Start(); err != nil {
			log.Fatalf("start %s: %v", args[0], err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case s := <-sigs:
			cmd.Process.Signal(s)
			<-done
			return
		case err := <-done:
			log.Printf("supervisor: %s exited after %s (%v); restarting (restart %d)", args[0], time.Since(start).Round(time.Millisecond), err, restarts+1)
			if time.Since(start) < 2*time.Second {
				time.Sleep(time.Second)
			}
		}
	}
}
