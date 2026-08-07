package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/nativehost"
	"github.com/eaopen/cloudfile-local-agent/internal/runner"
)

const version = "0.2.0"

func main() {
	nativeHost := flag.Bool("native-host", false, "serve Chrome Native Messaging")
	runSession := flag.String("run-session", "", "run one CloudFile session file")
	allowOrigin := flag.String("allow-origin", "", "trust one CloudFile server origin")
	flag.Parse()

	if *allowOrigin != "" {
		if _, err := config.AddOrigin(*allowOrigin); err != nil {
			fail(err)
		}
		return
	}
	if *runSession != "" {
		if err := runner.Run(*runSession); err != nil {
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
		return nativehost.Response{OK: true, Version: version}
	case "open_session_file":
		if request.Path == "" {
			return nativehost.Response{Error: "session file path is required"}
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
	default:
		return nativehost.Response{Error: "unsupported native message"}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "CloudFile Local Agent:", err)
	os.Exit(1)
}
