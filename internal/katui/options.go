package katui

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type options struct {
	brokers     []string
	cluster     string
	profileName string
	view        string
	group       string
	topic       string
	partition   int

	config     string
	security   *securityConfig
	sizeConfig string
	inspector  string

	interval                        time.Duration
	timeout                         time.Duration
	sizeInterval                    time.Duration
	structuredInput, renderedOutput int
	payloadStorage                  int64
	payloadStorageSet               bool

	once        bool
	noCache     bool
	noCacheFlag bool
	version     bool
}

func arguments(args []string, stderr io.Writer) (options, error) {
	var o options
	var brokers string
	var interval, timeout, size float64
	var inputMiB, outputMiB int
	var storageMiB int64
	flags := flag.NewFlagSet("katui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	for _, name := range []string{"b", "bootstrap-server"} {
		flags.StringVar(&brokers, name, "", "comma-separated brokers")
	}
	for _, name := range []string{"g", "group"} {
		flags.StringVar(&o.group, name, "", "consumer group")
	}
	for _, name := range []string{"t", "topic"} {
		flags.StringVar(&o.topic, name, "", "topic name or group scope")
	}
	for _, name := range []string{"p", "partition"} {
		flags.IntVar(&o.partition, name, -1, "partition (requires group or topic)")
	}
	for _, name := range []string{"i", "interval"} {
		flags.Float64Var(&interval, name, 5, "monitoring interval in seconds")
	}
	flags.Float64Var(&timeout, "timeout", 20, "request timeout in seconds")
	flags.Float64Var(&size, "size-interval", 30, "disk sampling interval in seconds")
	flags.IntVar(&inputMiB, "structured-input-mib", defaultStructuredInput>>20, "initial fullscreen structured input budget in MiB")
	flags.IntVar(&outputMiB, "rendered-output-mib", defaultRenderedOutput>>20, "initial fullscreen rendered output budget in MiB")
	flags.Int64Var(&storageMiB, "payload-storage-mib", payloadQuota>>20, "temporary storage for complete payloads in MiB (0 disables spilling)")
	flags.StringVar(&o.profileName, "cluster", "", "choose a named saved cluster")
	flags.StringVar(&o.view, "view", "", "initial view: topics or groups")
	flags.StringVar(&o.config, "client-config", "", "JSON broker connection settings")
	flags.StringVar(&o.sizeConfig, "size-client-config", "", "JSON disk query connection settings")
	flags.StringVar(&o.inspector, "inspector-config", defaultPath("XDG_CONFIG_HOME", ".config", "inspectors.json"), "inspector preferences")
	flags.BoolVar(&o.noCacheFlag, "no-cache", false, "disable monitoring cache")
	flags.BoolVar(&o.once, "once", false, "print one JSON snapshot")
	flags.BoolVar(&o.version, "version", false, "print version")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	if flags.NArg() != 0 {
		return o, errors.New("unexpected positional arguments")
	}
	if o.version {
		return o, nil
	}
	if inputMiB < 1 || inputMiB > math.MaxInt>>20 || outputMiB < 1 || outputMiB > math.MaxInt>>20 {
		return o, fmt.Errorf("initial payload budgets must be 1–%d MiB", math.MaxInt>>20)
	}
	if storageMiB < 0 || storageMiB > 1<<20 {
		return o, errors.New("payload storage must be 0–1048576 MiB")
	}
	o.structuredInput, o.renderedOutput = inputMiB<<20, outputMiB<<20
	o.payloadStorage, o.payloadStorageSet = storageMiB<<20, true
	brokersSpecified := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "b" || f.Name == "bootstrap-server" {
			brokersSpecified = true
		}
		if (f.Name == "p" || f.Name == "partition") && o.partition < 0 {
			o.partition = -2
		}
	})
	if o.partition < -1 || o.partition > math.MaxInt32 || (o.partition >= 0 && o.topic == "" && o.group == "") {
		return o, errors.New("partition must be nonnegative and requires a topic or group")
	}
	if o.view != "" && o.view != "topics" && o.view != "groups" {
		return o, errors.New("--view must be topics or groups")
	}
	o.setCachePolicy()
	if o.view == "" {
		o.view = "topics"
		if o.group != "" {
			o.view = "groups"
		}
	}
	if brokersSpecified {
		if o.profileName != "" {
			return o, errors.New("--cluster and -b cannot be used together")
		}
		if err := o.setBrokers(brokers); err != nil {
			return o, err
		}
	}
	for _, item := range []struct {
		name  string
		value float64
		dest  *time.Duration
	}{
		{"interval", interval, &o.interval}, {"timeout", timeout, &o.timeout}, {"size-interval", size, &o.sizeInterval},
	} {
		ns := item.value * float64(time.Second)
		if math.IsNaN(ns) || ns < 1 || ns >= float64(math.MaxInt64) {
			return o, fmt.Errorf("--%s must be a positive finite number of seconds", item.name)
		}
		*item.dest = time.Duration(ns)
	}
	return o, nil
}

func (o *options) setBrokers(text string) error {
	brokers := strings.Split(text, ",")
	for i, address := range brokers {
		address = strings.TrimSpace(address)
		if address == "" {
			return errors.New("enter a Kafka broker address")
		}
		if strings.ContainsAny(address, " /\t\r\n") {
			return fmt.Errorf("invalid broker address %q; use host:port", address)
		}
		brokers[i] = address
	}
	cluster := append([]string(nil), brokers...)
	sort.Strings(cluster)
	o.brokers = brokers
	o.cluster = strings.Join(cluster, ",")
	return nil
}

func defaultPath(variable, fallback, name string) string {
	dir := os.Getenv(variable)
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, fallback)
	}
	return filepath.Join(dir, "katui", name)
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}
