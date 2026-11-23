package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/orn1983/katui/internal/katui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := katui.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
