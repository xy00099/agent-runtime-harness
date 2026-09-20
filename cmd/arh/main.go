// Command arh is the Agent Runtime Harness binary: daemon + CLI + MCP.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/xy00099/agent-runtime-harness/internal/cli"
)

func main() {
	ctx := context.Background()
	os.Exit(cli.Run(ctx, os.Args[1:]))
}

var _ = fmt.Sprintf
