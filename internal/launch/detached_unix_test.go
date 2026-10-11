//go:build !windows

package launch

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestDetachedWorkerSurvivesLauncher(t *testing.T) {
	path := t.TempDir() + "/result.json"
	t.Setenv("CLOUDFILE_LAUNCH_TEST_RESULT", path)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLauncherHelper$")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("launcher failed: %s %v", output, err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var result struct {
			PID, Group int
			Input      string
		}
		if json.Unmarshal(data, &result) != nil {
			continue
		}
		if result.PID != result.Group || result.Input != "private-session-ticket" {
			t.Fatalf("worker not detached: %#v", result)
		}
		return
	}
	t.Fatal("worker did not finish after launcher exit")
}
func TestLauncherHelper(t *testing.T) {
	if os.Getenv("CLOUDFILE_LAUNCH_TEST_RESULT") == "" {
		return
	}
	executable, _ := os.Executable()
	if WithInput(executable, []byte("private-session-ticket"), "-test.run=^TestWorkerHelper$") != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func TestWorkerHelper(t *testing.T) {
	path := os.Getenv("CLOUDFILE_LAUNCH_TEST_RESULT")
	if path == "" {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	time.Sleep(150 * time.Millisecond)
	group, _ := syscall.Getpgid(0)
	data, _ := json.Marshal(struct {
		PID, Group int
		Input      string
	}{os.Getpid(), group, string(input)})
	if os.WriteFile(path, data, 0600) != nil {
		os.Exit(4)
	}
	os.Exit(0)
}
