package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	inputServerClass   = "com.mobilenext.mcp.InputServer"
	inputServerStartup = 15 * time.Second
)

// inputInjector sends one command (see InputServer.java for the protocol).
type inputInjector interface {
	call(timeout time.Duration, args ...string) error
}

// inputServer keeps a single InputServer running under app_process and talks
// to it over stdin/stdout. There is no socket, so nothing else on the device
// can reach it. It is started on first use and restarted if it dies or hangs.
type inputServer struct {
	jar string

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	lines    chan string
	stopping chan struct{}
	exited   chan struct{}
}

func newInputServer(jar string) *inputServer {
	return &inputServer{jar: jar}
}

func (s *inputServer) start() error {
	if _, err := os.Stat(s.jar); err != nil {
		return fmt.Errorf("input server: %w", err)
	}

	cmd := exec.Command("app_process", "/system/bin", inputServerClass)
	cmd.Env = append(os.Environ(), "CLASSPATH="+s.jar)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting input server: %w", err)
	}

	lines := make(chan string)
	stopping := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-stopping:
			}
		}
		_ = cmd.Wait()
		close(exited)
	}()

	s.cmd, s.stdin, s.lines, s.stopping, s.exited = cmd, stdin, lines, stopping, exited

	reply, err := s.read(inputServerStartup)
	if err == nil && reply != "ready" {
		err = fmt.Errorf("input server failed to start: %s", strings.TrimPrefix(reply, "error "))
	}
	if err != nil {
		s.stop()
		return err
	}
	log.Printf("input server started (pid %d)", cmd.Process.Pid)
	return nil
}

func (s *inputServer) stop() {
	if s.cmd == nil {
		return
	}
	close(s.stopping)
	_ = s.stdin.Close()
	_ = s.cmd.Process.Kill()
	<-s.exited
	s.cmd = nil
}

func (s *inputServer) read(timeout time.Duration) (string, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line := <-s.lines:
		return line, nil
	case <-s.exited:
		return "", errors.New("input server exited")
	case <-timer.C:
		return "", fmt.Errorf("input server did not answer within %s", timeout)
	}
}

func (s *inputServer) call(timeout time.Duration, args ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil {
		select {
		case <-s.exited:
			s.cmd = nil
		default:
		}
	}
	if s.cmd == nil {
		if err := s.start(); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(s.stdin, strings.Join(args, " ")+"\n"); err != nil {
		s.stop()
		return fmt.Errorf("input server: %w", err)
	}
	reply, err := s.read(timeout)
	if err != nil {
		// a hung or dead server would desync replies, start fresh next time
		s.stop()
		return err
	}
	if reply == "ok" {
		return nil
	}
	return fmt.Errorf("%s: %s", args[0], strings.TrimPrefix(reply, "error "))
}
