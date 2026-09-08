package tn

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// --- Self-restart on binary change ---

const (
	binaryWatchInterval  = 10 * time.Second
	binaryWatchMaxErrors = 3
)

// resolveOwnBinaryPath resolves the real on-disk path of the currently
// running executable, following symlinks (e.g. ~/bin/tn -> the actual build
// output), so a `go build -o ~/bin/tn` rebuild is detected regardless of how
// the daemon was launched.
func resolveOwnBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// binaryInfoChanged is a pure comparison: it reports whether b differs from
// a in a way that indicates the on-disk binary was rebuilt (different size
// or mtime). Either side may be nil (treated as "changed" unless both are).
func binaryInfoChanged(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a != nil || b != nil
	}
	return a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime())
}

// watchBinaryForSelfRestart starts a background watcher that exits the
// process when the on-disk binary changes, so launchd's KeepAlive brings the
// new build back up within ~binaryWatchInterval. A stat error only counts as
// "changed" after binaryWatchMaxErrors consecutive failures, so a transient
// rename window during `go build -o` doesn't trigger a spurious restart.
// Disabled entirely when TN_NO_SELFRESTART=1 (foreground/dev runs, tests,
// and smoke runs that start the real daemon should set this).
func (s *Server) watchBinaryForSelfRestart() {
	if os.Getenv("TN_NO_SELFRESTART") == "1" {
		return
	}

	path, err := resolveOwnBinaryPath()
	if err != nil {
		log.Printf("serve: could not resolve own binary path, self-restart watcher disabled: %v", err)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("serve: could not stat own binary, self-restart watcher disabled: %v", err)
		return
	}

	go func() {
		ticker := time.NewTicker(binaryWatchInterval)
		defer ticker.Stop()

		consecutiveErrors := 0
		for range ticker.C {
			cur, err := os.Stat(path)
			if err != nil {
				consecutiveErrors++
				if consecutiveErrors < binaryWatchMaxErrors {
					continue
				}
			} else {
				consecutiveErrors = 0
				if !binaryInfoChanged(info, cur) {
					continue
				}
			}
			log.Printf("serve: binary changed, exiting for launchd restart")
			s.exitForSelfRestart(os.Exit)
		}
	}()
}

// exitForSelfRestart acquires s.mu — the SAME mutex every state-mutating
// handler holds for the duration of its own read-modify-write — before
// calling exit, so the self-restart watcher can never interrupt a
// half-applied mutation (e.g. an assignment marker set in memory a moment
// before saveLocked persists it): the watcher blocks here until whichever
// handler currently holds the lock finishes its own atomic Lock/mutate/
// saveLocked/Unlock cycle, and only then exits. Deliberately the cheapest
// correct fix, not a WAL/journal: saveLocked's atomicWriteFile is already
// tmp+rename and fully synchronous, so state.json itself was never at risk
// of a torn write — the actual gap was purely in exit timing relative to
// in-memory mutation, which a shared mutex closes completely. Never
// unlocks: the process is exiting. exit is injectable (the real caller
// passes os.Exit) so a test can verify the mutex-wait behavior without
// terminating the test process.
func (s *Server) exitForSelfRestart(exit func(code int)) {
	s.mu.Lock()
	exit(0)
}
