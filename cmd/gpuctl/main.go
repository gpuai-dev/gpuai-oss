// Command gpuctl is the GPU.ai Open Source server: the public /v1 API plus the
// background workers that poll supplier availability, drive provisioning,
// monitor instance health and reconcile drift.
//
// Usage:
//
//	gpuctl [serve] [--log-level=info] [--log-format=json]
//	gpuctl migrate
//	gpuctl apikey create --name NAME [--scopes JSON]
//	gpuctl version
package main

import (
	"fmt"
	"os"
)

// version is injected at build time via -ldflags "-X main.version=<tag>".
var version string

const usage = `usage: gpuctl <command> [flags]

commands:
  serve            run the API server and background workers (default)
  migrate          apply pending database migrations and exit
  apikey create    create an API key for the default organization and print it once
  version          print the build version
`

func main() {
	sub := "serve"
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "serve":
		runServe(args)
	case "migrate":
		runMigrate(args)
	case "apikey":
		runAPIKey(args)
	case "version":
		v := version
		if v == "" {
			v = "dev"
		}
		fmt.Println(v)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", sub, usage)
		os.Exit(2)
	}
}
