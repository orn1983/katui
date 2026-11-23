package katui

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/twmb/franz-go/pkg/kadm"
	"golang.org/x/term"
)

const version = "0.1.0"

var errInterrupted = errors.New("keyboard interruption")

// Run executes the command with standard input and output and returns its exit status.
func Run(ctx context.Context, args []string) int {
	err := run(ctx, args)
	if ctx.Err() != nil || errors.Is(err, errInterrupted) {
		return 130
	}
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(os.Stderr, "katui:", safe(err.Error(), false))
	return 1
}

func run(ctx context.Context, args []string) error {
	o, err := arguments(args, os.Stderr)
	if err != nil {
		return err
	}
	if o.version {
		fmt.Println("katui", version)
		return nil
	}
	if !o.once && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
		return errors.New("interactive mode requires a terminal; use --once for JSON output")
	}
	b := &broker{options: o}
	if o.backend == "api" {
		b.client, err = o.kafka(o.config)
		if err != nil {
			return err
		}
		defer b.client.Close()
		b.admin = kadm.NewClient(b.client)
		b.disk = b.admin
		if o.sizeConfig != "" {
			disk, err := o.kafka(o.sizeConfig)
			if err != nil {
				return err
			}
			defer disk.Close()
			b.disk = kadm.NewClient(disk)
		}
	} else if err = b.findCLI(); err != nil {
		return err
	}
	if o.once {
		req, cancel := context.WithTimeout(ctx, o.timeout)
		defer cancel()
		value, err := b.once(req)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	return newUI(ctx, b).run()
}
