package sap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"

	apisap "git.cluster.aimingupward.com/steve/agent-project-controller/pkg/sap"
	"github.com/creack/pty"
)

// StartPTY runs argv on a local pseudoterminal. The local prototype uses this
// so a client can exercise SAP without a cluster. It is not a sandbox boundary.
func StartPTY(ctx context.Context, argv []string) (apisap.Session, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("command required")
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		cancel()
		return nil, err
	}
	return &ptySession{file: file, cmd: cmd, cancel: cancel}, nil
}

type ptySession struct {
	file   *os.File
	cmd    *exec.Cmd
	cancel context.CancelFunc
	once   sync.Once
	code   int
	known  bool
}

func (s *ptySession) Read(p []byte) (int, error)  { return s.file.Read(p) }
func (s *ptySession) Write(p []byte) (int, error) { return s.file.Write(p) }
func (s *ptySession) Resize(cols, rows uint16) error {
	return pty.Setsize(s.file, &pty.Winsize{Rows: rows, Cols: cols})
}
func (s *ptySession) Close() error {
	s.finish()
	return nil
}
func (s *ptySession) ExitCode() (int, bool) {
	s.finish()
	return s.code, s.known
}
func (s *ptySession) finish() {
	s.once.Do(func() {
		s.cancel()
		_ = s.file.Close()
		_ = s.cmd.Wait()
		if s.cmd.ProcessState != nil {
			s.code = s.cmd.ProcessState.ExitCode()
			s.known = true
		}
	})
}
