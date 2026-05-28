// dew-serve is the standalone deploy receiver for Linux VPS.
// Cross-platform (no Apple VZ dependency).
//
// Usage:
//
//	dew-serve --port 9080 --data-dir /var/dew --token-file /var/dew/token
package main

import (
	"fmt"
	"os"

	"github.com/solcreek/dew/internal/serve"
)

func main() {
	port := "9080"
	dataDir := "/var/dew"
	tokenFile := ""

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--port":
			i++
			if i < len(os.Args) {
				port = os.Args[i]
			}
		case "--data-dir":
			i++
			if i < len(os.Args) {
				dataDir = os.Args[i]
			}
		case "--token-file":
			i++
			if i < len(os.Args) {
				tokenFile = os.Args[i]
			}
		case "--version":
			fmt.Printf("dew-serve %s\n", serve.Version)
			return
		case "--help", "-h":
			fmt.Println("dew-serve — deploy receiver for Dew")
			fmt.Println()
			fmt.Println("Usage:")
			fmt.Println("  dew-serve [flags]")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  --port <port>           Listen port (default: 9080)")
			fmt.Println("  --data-dir <dir>        Data directory (default: /var/dew)")
			fmt.Println("  --token-file <path>     Token file path")
			fmt.Println("  --version               Print version")
			return
		}
	}

	if err := serve.Run(port, dataDir, tokenFile); err != nil {
		fmt.Fprintf(os.Stderr, "dew-serve: %v\n", err)
		os.Exit(1)
	}
}
