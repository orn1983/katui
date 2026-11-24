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

	backend       string
	config        string
	security      *securityConfig
	sizeConfig    string
	inspector     string
	cli           string
	commandConfig string

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
	flags.IntVar(&inputMiB, "structured-input-mib", maxStructuredInput>>20, "default structured payload input limit in MiB")
	flags.IntVar(&outputMiB, "rendered-output-mib", maxRenderedPayload>>20, "default payload display limit in MiB")
	flags.Int64Var(&storageMiB, "payload-storage-mib", payloadQuota>>20, "temporary storage for complete payloads in MiB (0 disables spilling)")
	flags.StringVar(&o.profileName, "cluster", "", "choose a named saved cluster")
	flags.StringVar(&o.view, "view", "", "topics or groups")
	flags.StringVar(&o.backend, "backend", "api", "api or cli")
	flags.StringVar(&o.config, "client-config", "", "JSON broker connection settings")
	flags.StringVar(&o.sizeConfig, "size-client-config", "", "JSON disk query connection settings")
	flags.StringVar(&o.inspector, "inspector-config", defaultPath("XDG_CONFIG_HOME", ".config", "inspectors.json"), "inspector preferences")
	flags.StringVar(&o.cli, "kafka-cli", "", "path to kafka-consumer-groups.sh")
	flags.StringVar(&o.commandConfig, "command-config", "", "Kafka CLI connection properties")
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
	if inputMiB < 1 || inputMiB > maxInspectionBytes>>20 || outputMiB < 1 || outputMiB > maxInspectionBytes>>20 {
		return o, fmt.Errorf("payload input and display limits must be 1–%d MiB", maxInspectionBytes>>20)
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
	if o.backend != "api" && o.backend != "cli" {
		return o, errors.New("--backend must be api or cli")
	}
	if o.view != "" && o.view != "topics" && o.view != "groups" {
		return o, errors.New("--view must be topics or groups")
	}
	if o.backend == "api" && (o.cli != "" || o.commandConfig != "") {
		return o, errors.New("--kafka-cli and --command-config require CLI backend")
	}
	if o.backend == "cli" && (o.config != "" || o.sizeConfig != "" || o.view == "topics" || (o.topic != "" && o.group == "")) {
		return o, errors.New("CLI backend supports group monitoring and --command-config")
	}
	o.setCachePolicy()
	if o.view == "" {
		o.view = "topics"
		if o.group != "" || o.backend == "cli" {
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
