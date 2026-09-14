package e2e_test

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestPasswordPromptCancellationRestoresTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY signal test requires Unix")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY test requires Python 3")
	}
	bin := buildCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-u", "-c", `
import os, pty, select, signal, sys, tempfile, termios, time

for action in ('ctrl-c', 'sigint', 'sigterm', 'enter'):
    with tempfile.TemporaryDirectory() as home:
        # Obtain the original terminal settings before the child changes them.
        master, slave = pty.openpty()
        original = termios.tcgetattr(slave)
        pid = os.fork()
        if pid == 0:
            os.close(master)
            os.setsid()
            import fcntl
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
            for fd in (0, 1, 2):
                os.dup2(slave, fd)
            os.close(slave)
            os.chdir(home)
            env = {k: v for k, v in os.environ.items() if not k.startswith('FLINT_')}
            env.update(HOME=home, XDG_CONFIG_HOME=home)
            os.execve(sys.argv[1], [sys.argv[1], 'auth', 'import'], env)
        os.close(slave)
        try:
            output = b''
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline:
                if select.select([master], [], [], .05)[0]:
                    output += os.read(master, 4096)
                if b'Flint API key:' in output and not termios.tcgetattr(master)[3] & termios.ECHO:
                    break
            else:
                raise AssertionError('password prompt did not disable echo')
            os.write(master, b'synthetic-password')
            if action == 'ctrl-c':
                os.write(master, b'\x03')
            elif action == 'enter':
                os.write(master, b'\r')
            else:
                os.kill(pid, signal.SIGINT if action == 'sigint' else signal.SIGTERM)
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline:
                done, status = os.waitpid(pid, os.WNOHANG)
                if done:
                    pid = None
                    expected = 3 if action == 'enter' else 5
                    assert os.waitstatus_to_exitcode(status) == expected, (action, status)
                    break
                time.sleep(.02)
            else:
                raise AssertionError(action + ' did not stop the prompt')
            assert termios.tcgetattr(master) == original, action + ' did not restore terminal settings'
            while select.select([master], [], [], .05)[0]:
                try:
                    chunk = os.read(master, 4096)
                except OSError:
                    break
                if not chunk:
                    break
                output += chunk
            assert b'synthetic-password' not in output, 'password was echoed'
            print(action + ': exited promptly, terminal restored, password hidden')
        finally:
            if pid:
                os.kill(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
            os.close(master)
`, bin)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("PTY test: %v\n%s", err, output)
	}
}
