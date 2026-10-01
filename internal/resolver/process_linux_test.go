//go:build linux

package resolver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCancellationKillsExtractorProcessGroup(t *testing.T) {
	t.Setenv("GO_WANT_RESOLVER_GROUP_HELPER", "1")
	marker := filepath.Join(t.TempDir(), "processes")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := runCommand(ctx, os.Args[0], []string{"-test.run=^TestResolverGroupHelper$", "--", "parent", marker})
		done <- err
	}()
	var leader, child int
	waitFor(t, func() bool {
		body, err := os.ReadFile(marker)
		if err != nil {
			return false
		}
		n, _ := fmt.Sscanf(string(body), "%d %d", &leader, &child)
		return n == 2
	})
	group, err := syscall.Getpgid(child)
	if err != nil || group != leader {
		t.Fatalf("descendant was not isolated with its extractor: pgid=%d parent=%d error=%v", group, leader, err)
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled extractor succeeded")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancelled process group did not finish within WaitDelay")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("cancellation did not promptly close inherited pipes")
	}
	waitFor(t, func() bool { return !linuxProcessRunning(child) })
}

// Orphan zombies can briefly remain until the container's init reaps them.
// They are not executing and no longer retain descriptors or runtime memory.
func linuxProcessRunning(pid int) bool {
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 || end+2 >= len(body) {
		return true
	}
	state := body[end+2]
	return state != 'Z' && state != 'X'
}

func TestResolverGroupHelper(t *testing.T) {
	if os.Getenv("GO_WANT_RESOLVER_GROUP_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-2]
	marker := os.Args[len(os.Args)-1]
	if mode == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestResolverGroupHelper$", "--", "child", marker)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(marker, []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0600); err != nil {
			os.Exit(3)
		}
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}
