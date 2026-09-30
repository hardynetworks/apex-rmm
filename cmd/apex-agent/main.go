// Command apex-agent is the Apex RMM endpoint agent for Windows, macOS and Linux.
//
//	apex-agent install --server https://rmm.example.com --token <enroll-token>
//	apex-agent uninstall
//	apex-agent run          (foreground, for debugging)
//	apex-agent service      (invoked by the OS service manager)
//	apex-agent desktop ...  (remote-desktop helper, spawned by the agent)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hardynetworks/apex-rmm/internal/agent"
	"github.com/hardynetworks/apex-rmm/internal/agent/desktop"
	"github.com/hardynetworks/apex-rmm/internal/proto"
)

func usage() {
	fmt.Fprintf(os.Stderr, `Apex RMM agent %s

Usage:
  apex-agent install --server URL --token TOKEN [--force]
  apex-agent uninstall
  apex-agent run
  apex-agent version
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
		server := fs.String("server", "", "Apex RMM server URL, e.g. https://remote.example.com")
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
