// dew for Linux — same CLI name, cross-platform commands.
// No Apple VZ (no dew up/start/run/exec/session on Linux).
package main

import (
	"fmt"
	"os"

	"github.com/solcreek/dew/internal/serve"
)

const version = "0.4.1"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "version":
		fmt.Printf("dew %s (linux)\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "dew: unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dew: %v\n", err)
		os.Exit(1)
	}
}

func cmdServe(args []string) error {
	port := "9080"
	dataDir := "/var/dew"
	tokenFile := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			i++
			if i < len(args) {
				port = args[i]
			}
		case "--data-dir":
			i++
			if i < len(args) {
				dataDir = args[i]
			}
		case "--token-file":
			i++
			if i < len(args) {
				tokenFile = args[i]
			}
		}
	}

	serve.Version = version
	return serve.Run(port, dataDir, tokenFile)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `dew — run any app, anywhere (linux)

Usage:
  dew serve [flags]              Run deploy receiver

Flags:
  --port <port>                  Listen port (default: 9080)
  --data-dir <dir>               Data directory (default: /var/dew)
  --token-file <path>            Token file path

Other:
  dew version                    Print version
  dew help                       Show this help
`)
}
