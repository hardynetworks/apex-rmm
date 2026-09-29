// Command hardy-agent is the Hardy RMM endpoint agent for Windows, macOS and Linux.
//
//	hardy-agent install --server https://rmm.example.com --token <enroll-token>
//	hardy-agent uninstall
//	hardy-agent run          (foreground, for debugging)
//	hardy-agent service      (invoked by the OS service manager)
//	hardy-agent desktop ...  (remote-desktop helper, spawned by the agent)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hardynetworks/hardy-rmm/internal/agent"
	"github.com/hardynetworks/hardy-rmm/internal/agent/desktop"
	"github.com/hardynetworks/hardy-rmm/internal/proto"
)

func usage() {
	fmt.Fprintf(os.Stderr, `Hardy RMM agent %s

Usage:
  hardy-agent install --server URL --token TOKEN [--force]
  hardy-agent uninstall
  hardy-agent run
  hardy-agent version
`, proto.Version)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		server := fs.String("server", "", "Hardy RMM server URL, e.g. https://remote.example.com")
		token := fs.String("token", "", "enrollment token")
		force := fs.Bool("force", false, "re-enroll even if already enrolled")
		_ = fs.Parse(args)
		if err := agent.Install(*server, *token, *force); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
	case "uninstall":
		if err := agent.Uninstall(); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
	case "service":
		agent.SetupLogging("agent")
		if err := agent.RunService(); err != nil {
			log.Fatal(err)
		}
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := agent.RunForeground(ctx); err != nil {
			log.Fatal(err)
		}
	case "desktop":
		fs := flag.NewFlagSet("desktop", flag.ExitOnError)
		url := fs.String("url", "", "")
		session := fs.String("session", "", "")
		token := fs.String("token", "", "")
		_ = fs.Parse(args)
		agent.SetupLogging("desktop")
		if err := desktop.Run(*url, *session, *token); err != nil {
			log.Printf("desktop session ended: %v", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println(proto.Version)
	default:
		usage()
	}
}
