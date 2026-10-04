// Command drivekey gives an AI agent access to the user's own Google Drive and Sheets.
package main

import (
	"os"

	"github.com/jerryfane/drivekey/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
