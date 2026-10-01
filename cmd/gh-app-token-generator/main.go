package main

import (
	"fmt"
	"io"
	"os"

	"github.com/oikarinen/agent-gh-token-generator/internal/authtoken"
)

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr, authtoken.GetToken))
}

// run executes the command and returns its exit code. The token source is
// passed in so tests can exercise argument handling and output.
func run(args []string, stdout, stderr io.Writer, getToken func(appID, installationID string) (string, error)) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "Usage: ", args[0], " <app_id> <installation_id>")
		return 1
	}

	appID := args[1]
	installationID := args[2]

	accessToken, err := getToken(appID, installationID)
	if err != nil {
		fmt.Fprintln(stderr, "Error: ", err)
		return 1
	}

	fmt.Fprint(stdout, accessToken)
	return 0
}
