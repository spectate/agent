package auth

import (
	"bufio"
	"fmt"
	"github.com/getsentry/sentry-go"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/spectate/agent/internal/http"
	"github.com/spectate/agent/pkg/config"
	"github.com/spf13/viper"
	"os"
	"strings"
)

func Authorize(token string) error {
	client := http.NewClient()

	existingToken := viper.Get("host.token")
	if existingToken != "" {
		requestOverwriteConfirmation()
	}

	hostInfo, err := host.Info()
	if err != nil {
		sentry.CaptureException(err)
		return err
	}

	response, err := client.Authorize(http.Authorize{
		Token:    token,
		Hostname: hostInfo.Hostname,
	})
	if err != nil {
		return err
	}

	result := response.Result().(*http.AuthorizeSuccess)

	viper.Set("host.token", result.Token)
	config.Update()

	return nil
}

func requestOverwriteConfirmation() {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("An existing token was found, overwrite? (y/n): ")
		response, err := reader.ReadString('\n')

		if err != nil {
			fmt.Println("Invalid input, please try again.")
			continue
		}

		// Trim spaces and make the response lower-case for comparison
		response = strings.ToLower(strings.TrimSpace(response))

		if response == "y" || response == "yes" {
			fmt.Println("Confirmed.")
			break
		} else if response == "n" || response == "no" {
			fmt.Println("Aborting.")
			os.Exit(1)
		} else {
			fmt.Println("Invalid response, please type 'y' or 'n'")
		}
	}
}
