package main

import (
	"context"
	"fmt"
	"time"
)

func monitorSystemResources() {
	for {
		// Code for monitoring system resources goes here
		fmt.Println("Monitoring system resources...")
		time.Sleep(time.Second * 5) // Sleep for 5 seconds between monitoring intervals
	}
}

func main() {
	_, cancel := start()
	defer cancel()
	neverEnd := make(chan struct{})
	<-neverEnd
}

func start() (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = context.WithCancel(context.Background())
	go monitorSystemResources()
	return
}
