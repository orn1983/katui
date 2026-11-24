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
	if o.profileName != "" {
		profiles, err := loadConnectionProfiles(o.backend)
		if err != nil {
			return err
		}
		p, ok := profiles[o.profileName]
		if !ok {
			return fmt.Errorf("saved cluster %q was not found", o.profileName)
		}
		if err := b.selectConnection(p); err != nil && (o.once || !errors.Is(err, errProfilePassword)) {
			return err
		}
	}
	if len(b.brokers) != 0 {
		if err := b.open(); err != nil {
			return err
		}
	}
	if o.once {
		defer b.close()
		if len(b.brokers) == 0 {
			return errors.New("no Kafka connection configured; pass -b host:port or --cluster NAME")
		}
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
	for {
		u := newUI(ctx, b)
		err := u.run()
		b.close()
		if err != nil || u.nextBroker == nil {
			return err
		}
		b = u.nextBroker
	}
}

func (b *broker) open() error {
	if b.backend == "cli" {
		return b.findCLI()
	}
	client, err := b.kafka(b.config)
	if err != nil {
		return err
	}
	admin := kadm.NewClient(client)
	disk := admin
	if b.sizeConfig != "" {
		sizeClient, err := b.kafka(b.sizeConfig)
		if err != nil {
			client.Close()
			return err
		}
		disk = kadm.NewClient(sizeClient)
	}
	b.client, b.admin, b.disk = client, admin, disk
	return nil
}

func (b *broker) close() {
	if b.disk != nil && b.disk != b.admin {
		b.disk.Close()
	}
	if b.client != nil {
		b.client.Close()
	}
}
