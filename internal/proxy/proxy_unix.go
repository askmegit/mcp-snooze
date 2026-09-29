//go:build !windows

package proxy

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func startProcess(command []string, stderr io.Writer) (*childProcess, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-proc.done:
		return
	case <-timer.C:
	}
	_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
	<-proc.done
}

func notifySignals(ch chan<- os.Signal) func() {
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	return func() { signal.Stop(ch) }
}
