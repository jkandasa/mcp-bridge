// Package child manages the lifecycle of external MCP server child processes.
//
// Process flow:
//  1. Start() forks the binary via exec.Command, wiring stdin/stdout pipes.
//  2. watchLoop owns the single cmd.Wait() call for each process instance.
//     It signals exitCh when Wait returns so that kill() can synchronise
//     without calling Wait itself (calling Wait on the same Cmd from two
//     goroutines is undefined behaviour).
//  3. Stop() sends SIGTERM, waits on exitCh, and escalates to SIGKILL if the
//     process does not exit within stopTimeout.
//  4. Crashed processes are restarted with exponential back-off (500ms→30s).
package child

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.uber.org/zap"

	"mcp-bridge/internal/logger"
)

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 30 * time.Second
	stopTimeout    = 5 * time.Second
)

// Process manages one external MCP server binary as a subprocess.
type Process struct {
	name    string
	command string
	args    []string
	env     []string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	// exitCh is closed by watchLoop after cmd.Wait() returns for the current
	// process instance. kill() waits on it instead of calling Wait itself.
	exitCh  chan struct{}
	running bool
	stopped bool

	restartCh chan struct{}

	// OnRestart is called (in a goroutine) after a successful restart.
	OnRestart func()
}

// NewProcess creates a Process but does not start it yet.
func NewProcess(name, command string, args, env []string) *Process {
	return &Process{
		name:      name,
		command:   command,
		args:      args,
		env:       env,
		restartCh: make(chan struct{}),
	}
}

// Start launches the child binary and begins the auto-restart watch loop.
func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.launch(); err != nil {
		return err
	}
	p.running = true
	go p.watchLoop(ctx)
	return nil
}

// Stop permanently stops the child process. Auto-restart is disabled.
// Calling Stop more than once is safe.
func (p *Process) Stop() {
	p.mu.Lock()
	p.stopped = true
	exitCh := p.exitCh
	p.mu.Unlock()

	p.signal(os.Interrupt)

	if exitCh != nil {
		select {
		case <-exitCh:
		case <-time.After(stopTimeout):
			p.mu.Lock()
			if p.cmd != nil && p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			p.mu.Unlock()
			if exitCh != nil {
				<-exitCh
			}
		}
	}
}

// Pipes returns the current stdin writer and stdout reader for the child.
func (p *Process) Pipes() (stdin io.WriteCloser, stdout io.ReadCloser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdin, p.stdout
}

// RestartCh returns a channel that is closed when the process has restarted.
func (p *Process) RestartCh() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restartCh
}

// IsRunning reports whether the child process is currently alive.
func (p *Process) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// launch starts the binary and wires up the pipes. Must be called with mu held.
func (p *Process) launch() error {
	cmd := exec.Command(p.command, p.args...)
	cmd.Env = append(os.Environ(), p.env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("child %q: stdin pipe: %w", p.name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("child %q: stdout pipe: %w", p.name, err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("child %q: start: %w", p.name, err)
	}

	p.cmd = cmd
	p.stdin = stdin
	p.stdout = stdout
	p.exitCh = make(chan struct{})
	return nil
}

// signal sends sig to the current process (if alive). Safe to call with mu
// not held; acquires it internally.
func (p *Process) signal(sig os.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

// watchLoop is the single owner of cmd.Wait() for each process instance.
// It closes exitCh when Wait returns, then restarts the process if needed.
func (p *Process) watchLoop(ctx context.Context) {
	log := logger.L().With(zap.String("child", p.name))
	backoff := initialBackoff

	for {
		// Capture the current cmd and exitCh under the lock.
		p.mu.Lock()
		cmd := p.cmd
		exitCh := p.exitCh
		p.mu.Unlock()

		if cmd == nil {
			return
		}

		// Sole caller of Wait for this cmd instance.
		_ = cmd.Wait()
		close(exitCh) // unblocks Stop() if it is waiting

		p.mu.Lock()
		p.running = false
		if p.stopped {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		log.Warn("process exited; restarting", zap.Duration("backoff", backoff))

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}

		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return
		}
		if err := p.launch(); err != nil {
			log.Error("restart failed", zap.Error(err))
			p.mu.Unlock()
			continue
		}
		p.running = true
		old := p.restartCh
		p.restartCh = make(chan struct{})
		close(old)
		p.mu.Unlock()

		backoff = initialBackoff
		log.Info("restarted")

		if p.OnRestart != nil {
			go p.OnRestart()
		}
	}
}
