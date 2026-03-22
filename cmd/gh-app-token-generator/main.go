package main

import (
	"fmt"
	"os"

	"github.com/oikarinen/agent-gh-token-generator/internal/authtoken"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "Usage: ", os.Args[0], " <app_id> <installation_id>")
		os.Exit(1)
	}

	appID := os.Args[1]
	installationID := os.Args[2]

	accessToken, err := authtoken.GetToken(appID, installationID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: ", err)
		os.Exit(1)
	}

	fmt.Print(accessToken)
}
