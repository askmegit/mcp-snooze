//go:build windows

package proxy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
)

func startProcess(command []string, stderr io.Writer) (*childProcess, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	return &childProcess{cmd: cmd, stdin: stdin, stdout: stdout, done: make(chan struct{})}, nil
}

func terminateProcess(proc *childProcess) {
	if proc == nil {
		return
	}
	select {
	case <-proc.done:
		return
	default:
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(proc.cmd.Process.Pid)).Run()
	<-proc.done
}

func notifySignals(ch chan<- os.Signal) func() {
	signal.Notify(ch, os.Interrupt)
	return func() { signal.Stop(ch) }
}
