package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/eaopen/cloudfile-local-agent/internal/appfinder"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/runner"
	"github.com/eaopen/cloudfile-local-agent/internal/session"
	"github.com/eaopen/cloudfile-local-agent/internal/update"
)

const version = "0.4.0"

func main() {
	nativeHost := flag.Bool("native-host", false, "serve Chrome Native Messaging")
	runSession := flag.String("run-session", "", "run one CloudFile session file")
	runSessionStdin := flag.Bool("run-session-stdin", false, "run one CloudFile session descriptor from stdin")
	allowOrigin := flag.String("allow-origin", "", "trust one CloudFile server origin")
	setUpdateSource := flag.String("set-update-source", "", "record the static update.json URL")
	showConfig := flag.Bool("show-config", false, "print local configuration")
	validateConfig := flag.Bool("validate-config", false, "validate local configuration")
	checkUpdate := flag.Bool("check-update", false, "check for and report an available update")
	applyUpdate := flag.Bool("update", false, "check, download and install the latest update")
	flag.Parse()

	if *allowOrigin != "" {
		if _, err := config.AddOrigin(*allowOrigin); err != nil {
			fail(err)
		}
		return
	}
	if *setUpdateSource != "" {
		if _, err := config.SetUpdateSource(*setUpdateSource); err != nil {
			fail(err)
		}
		return
	}
	if *showConfig || *validateConfig {
		agentConfig, err := config.Load()
		if err != nil {
			fail(err)
		}
		if *showConfig {
			data, err := json.MarshalIndent(agentConfig, "", "  ")
			if err != nil {
				fail(err)
			}
			fmt.Println(string(data))
		}
		return
	}
	if *runSession != "" {
		if err := runner.Run(*runSession); err != nil {
			fail(err)
		}
		return
	}
	if *runSessionStdin {
		if err := runSessionFromStdin(); err != nil {
			fail(err)
		}
		return
	}
	if *checkUpdate || *applyUpdate {
		if err := runUpdate(*applyUpdate); err != nil {
			fail(err)
		}
		return
	}
	if *nativeHost || flag.NFlag() == 0 {
		if err := nativehost.Serve(os.Stdin, os.Stdout, handleNativeMessage); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "CloudFile Local Agent %s\n", version)
	fmt.Fprintln(os.Stderr, "Use --allow-origin https://cloudfile.example before opening sessions.")
}

func handleNativeMessage(request nativehost.Request) nativehost.Response {
	switch request.Type {
	case "status":
		response := nativehost.Response{
			OK:           true,
			Version:      version,
			Applications: appfinder.Names(),
		}
		if agentConfig, err := config.Load(); err == nil {
			if root, err := agentConfig.Root(); err == nil {
				response.WorkspaceRoot = root
				response.CanOpenWorkspace = true
			}
			response.UpdateSourceSet = agentConfig.UpdateSource != ""
			if agentConfig.UpdateSource != "" {
				if manifest, err := update.Fetch(agentConfig.UpdateSource); err == nil {
					if comparison, err := update.Compare(manifest.Version, version); err == nil && comparison > 0 {
						response.UpdateAvailable = true
						response.LatestVersion = manifest.Version
					}
				}
			}
		}
		return response
	case "open_workspace":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid open_workspace request"}
		}
		agentConfig, err := config.Load()
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		root, err := agentConfig.Root()
		if err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		if err := os.MkdirAll(root, 0700); err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		if err := openDirectory(root); err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		return nativehost.Response{OK: true, WorkspaceRoot: root}
	case "open_session_file":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid session file path"}
		}
		executable, err := os.Executable()
		if err != nil {
			return nativehost.Response{Error: "cannot start local agent"}
		}
		command := exec.Command(executable, "--run-session", request.Path)
		command.Stdout = nil
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		return nativehost.Response{OK: true}
	case "open_session":
		if !request.Valid() {
			return nativehost.Response{Error: "invalid session descriptor"}
		}
		descriptor := session.Descriptor{
			Protocol:  request.Protocol,
			Server:    request.Server,
			Ticket:    request.Ticket,
			ExpiresAt: request.ExpiresAt,
		}
		data, err := json.Marshal(descriptor)
		if err != nil {
			return nativehost.Response{Error: "cannot encode session descriptor"}
		}
		executable, err := os.Executable()
		if err != nil {
			return nativehost.Response{Error: "cannot start local agent"}
		}
		command := exec.Command(executable, "--run-session-stdin")
		command.Stdin = strings.NewReader(string(data))
		command.Stdout = nil
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			return nativehost.Response{Error: err.Error()}
		}
		return nativehost.Response{OK: true}
	default:
		return nativehost.Response{Error: "unsupported native message"}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "CloudFile Local Agent:", err)
	os.Exit(1)
}

func openDirectory(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("explorer.exe", path)
	case "darwin":
		command = exec.Command("open", path)
	default:
		command = exec.Command("xdg-open", path)
	}
	return command.Start()
}

func runSessionFromStdin() error {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	descriptor, err := session.Parse(data)
	if err != nil {
		return err
	}
	return runner.RunDescriptor(descriptor)
}

// runUpdate drives the --check-update / --update commands. With apply=false it
// only reports whether an update is available; with apply=true it downloads,
// verifies and installs it.
func runUpdate(apply bool) error {
	agentConfig, err := config.Load()
	if err != nil {
		return err
	}
	if agentConfig.UpdateSource == "" {
		return fmt.Errorf("update source is not configured (set update_source in config.json)")
	}
	manifest, err := update.Fetch(agentConfig.UpdateSource)
	if err != nil {
		return err
	}
	comparison, err := update.Compare(manifest.Version, version)
	if err != nil {
		return fmt.Errorf("update manifest has invalid version %q: %w", manifest.Version, err)
	}
	if comparison <= 0 {
		fmt.Printf("CloudFile Local Agent %s is up to date (latest %s)\n", version, manifest.Version)
		return nil
	}
	if !apply {
		fmt.Printf("update available: %s -> %s\n", version, manifest.Version)
		if manifest.Notes != "" {
			fmt.Println(manifest.Notes)
		}
		return nil
	}
	newBinary, err := update.Apply(agentConfig.UpdateSource, manifest)
	if err != nil {
		return err
	}
	fmt.Printf("updated to %s (%s)\n", manifest.Version, newBinary)
	fmt.Println("restart Chrome (or reload the extension) to pick up the new agent.")
	return nil
}
