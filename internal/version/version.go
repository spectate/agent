package version

import (
	"fmt"
	"time"
)

var (
	Version     = "0.0.0+dev"
	BuildDate   = "2023-10-25T12:01:25Z"
	Environment = "development"
)

func init() {
	parsedDate, err := time.Parse(time.RFC3339, BuildDate)
	if err != nil {
		fmt.Printf("Error parsing BuildDate: %v\n", err)
	} else {
		BuildDate = parsedDate.Format("Mon Jan 2 15:04:05 MST 2006")
	}
}
