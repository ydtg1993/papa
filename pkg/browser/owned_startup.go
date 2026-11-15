package browser

// Keep process ownership here instead of Launcher.Launch: Rod v0.116.2 has
// context-free guard PID and HTTP endpoint waits. Flags still come from Rod.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/ysmood/leakless"
	"github.com/ysmood/leakless/pkg/shared"
)

type browserLauncher interface {
	Kill()
	Cleanup()
	Get(flags.Flag) string
}

type ownedLauncher struct {
	config   *launcher.Launcher
	cmd      *exec.Cmd
	done     chan struct{}
	guard    net.Conn
	childPID int
	guarded  bool
	kill     sync.Once
	// Only directories created by this launch are ours to delete. A configured
	// user-data-dir can contain durable credentials and is not an ownership grant.
	profileDir string
	cleanupErr error // published when Cleanup returns
}

func (p *ownedLauncher) Get(f flags.Flag) string { return p.config.Get(f) }
func (p *ownedLauncher) Kill() {
	p.kill.Do(func() {
		if p.guard != nil {
			_ = p.guard.Close()
		}
		if p.childPID > 0 {
			if child, err := os.FindProcess(p.childPID); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), browserCleanupTimeout)
				stopDiagnosticProcess(ctx, child)
				cancel()
			}
		} else if p.guarded && p.done != nil {
			// A cancelled handshake may precede the PID reply. Give the
			// packaged guard time to observe its closed socket and kill the
			// child before terminating the guard itself.
			// Do not kill a guard before it can clean its unknown child (on
			// Unix that child owns a different process group). Cleanup keeps
			// the physical slot occupied until the guard has actually exited.
			return
		}
		if p.cmd != nil && p.cmd.Process != nil {
			ctx, cancel := context.WithTimeout(context.Background(), browserCleanupTimeout)
			defer cancel()
			stopDiagnosticProcess(ctx, p.cmd.Process)
		}
	})
}
func (p *ownedLauncher) Cleanup() {
	if p.done != nil {
		<-p.done
	}
	ctx, cancel := context.WithTimeout(context.Background(), browserCleanupTimeout)
	defer cancel()
	if p.profileDir != "" {
		p.cleanupErr = removeDiagnosticProfile(ctx, p.profileDir)
	}
}

func (p *ownedLauncher) CleanupError() error { return p.cleanupErr }

// Preserve the packaged leakless guard and its parent-death socket protocol,
// but own both ends of setup so cancellation can interrupt Accept/Decode.
func ownedGuardBinary(ctx context.Context) (bin string, err error) {
	for {
		lock, e := net.Listen("tcp", "127.0.0.1:2978")
		if e == nil {
			defer lock.Close()
			defer func() {
				if v := recover(); v != nil {
					err = fmt.Errorf("extract leakless guard: %v", v)
				}
			}()
			return leakless.GetLeaklessBin(), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func acceptOwnedGuard(ctx context.Context, listener net.Listener, done <-chan struct{}, uid string, childPID *int) (net.Conn, error) {
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-done:
			_ = listener.Close()
		case <-watchDone:
		}
	}()
	conn, err := listener.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("accept leakless guard: %w", err)
	}
	stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopConn()
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	if err := json.NewEncoder(conn).Encode(shared.Message{UID: uid}); err != nil {
		return nil, err
	}
	var reply shared.Message
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("receive leakless PID: %w", err)
	}
	if reply.Error != "" || reply.PID <= 0 {
		return nil, fmt.Errorf("leakless startup failed: %s", reply.Error)
	}
	*childPID = reply.PID
	// Detach cancellation before transferring the live watchdog socket.
	// If the callback already started, ctx.Err below rejects the transfer.
	stopConn()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}

func launchOwned(ctx context.Context, l *launcher.Launcher, profileDir string) (owned *ownedLauncher, endpoint string, err error) {
	bin, err := diagnosticBinary(ctx, l.Get(flags.Bin))
	if err != nil {
		return nil, "", err
	}
	if pref := l.Get(flags.Preferences); pref != "" {
		profile := l.Get(flags.ProfileDir)
		if profile == "" {
			profile = "Default"
		}
		path := filepath.Join(l.Get(flags.UserDataDir), profile, "Preferences")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(path, []byte(pref), 0600); err != nil {
			return nil, "", err
		}
	}
	args := l.FormatArgs()
	var listener net.Listener
	uid := ""
	if l.Has(flags.Leakless) && leakless.Support() {
		guardBin, e := ownedGuardBinary(ctx)
		if e != nil {
			return nil, "", e
		}
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", err
		}
		defer listener.Close()
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, "", err
		}
		uid = hex.EncodeToString(token[:])
		args = append([]string{uid, listener.Addr().String(), bin}, args...)
		bin = guardBin
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	cmd := exec.Command(bin, args...)
	if runtime.GOOS != "windows" {
		if xvfb, ok := l.GetFlags(flags.XVFB); ok {
			cmd = exec.Command("xvfb-run", append(append([]string{}, xvfb...), cmd.Args...)...)
		}
	}
	configureDiagnosticProcess(cmd)
	cmd.Env, _ = l.GetFlags(flags.Env)
	cmd.Dir = l.Get(flags.WorkingDir)
	output := &diagnosticOutput{url: make(chan string, 1)}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	owned = &ownedLauncher{config: l, cmd: cmd, done: make(chan struct{}), guarded: listener != nil, profileDir: profileDir}
	go func() { _ = cmd.Wait(); close(owned.done) }()
	// Return ownership even on failure: the caller keeps its creation slot
	// until process cleanup completes; no background launch escapes the pool.
	if listener != nil {
		owned.guard, err = acceptOwnedGuard(ctx, listener, owned.done, uid, &owned.childPID)
		if err != nil {
			return owned, "", err
		}
	}
	select {
	case <-ctx.Done():
		return owned, "", ctx.Err()
	case <-owned.done:
		return owned, "", fmt.Errorf("browser exited before announcing control endpoint")
	case endpoint = <-output.url:
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "ws" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil {
		return owned, "", fmt.Errorf("browser announced invalid control endpoint")
	}
	// Chromium already announced the exact WebSocket endpoint. Connecting to
	// it directly avoids Rod's redundant context-free /json/version request.
	return owned, endpoint, nil
}
