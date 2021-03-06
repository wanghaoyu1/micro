// Package runtime is the micro runtime
package runtime

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/micro/cli"
	"github.com/micro/go-micro"
	"github.com/micro/go-micro/config/cmd"
	"github.com/micro/go-micro/runtime"
	rs "github.com/micro/go-micro/runtime/service"
	"github.com/micro/micro/runtime/notifier"
)

const (
	// RunUsage message for the run command
	RunUsage = "Required usage: micro run service --name example --version latest --source go/package/import/path"
	// KillUsage message for the kill command
	KillUsage = "Require usage: micro kill service --name example (optional: --version latest)"
	// Getusage message for micro get command
	GetUsage = "Require usage: micro ps service --name example (optional: --version latest)"
)

func defaultEnv() []string {
	var env []string
	for _, evar := range os.Environ() {
		if strings.HasPrefix(evar, "MICRO_") {
			env = append(env, evar)
		}
	}

	return env
}

func runService(ctx *cli.Context, srvOpts ...micro.Option) {
	// Init plugins
	for _, p := range Plugins() {
		p.Init(ctx)
	}

	// we need some args to run
	if len(ctx.Args()) == 0 {
		fmt.Println(RunUsage)
		return
	}

	// get the args
	name := ctx.String("name")
	version := ctx.String("version")
	source := ctx.String("source")
	env := ctx.StringSlice("env")
	local := ctx.Bool("local")

	// "service" is a reserved keyword
	// but otherwise assume anything else is source
	if v := ctx.Args()[0]; v != "service" {
		source = v
	}

	var r runtime.Runtime
	var exec []string

	// must specify service name
	if len(name) == 0 {
		if len(source) > 0 {
			name = filepath.Base(source)
		} else {
			cwd, _ := os.Getwd()
			name = filepath.Base(cwd)
		}
	}

	// local usage specified
	switch local {
	case true:
		r = *cmd.DefaultCmd.Options().Runtime
		// NOTE: When in local mode, we consider source to be
		// the filesystem path to the source of the service
		exec = []string{"go", "run", "."}

		if len(source) > 0 {
			// dir doesn't exist so pull
			if err := os.Chdir(source); err != nil {
				exec[2] = source
			}
		}

		// specify the runtime notifier to update wiht local file changes
		if err := r.Init(runtime.WithNotifier(notifier.New(name, version, source))); err != nil {
			fmt.Printf("Could not start notifier: %v", err)
			return
		}
	default:
		// new service runtime
		r = rs.NewRuntime()
		// NOTE: we consider source in default mode
		// to be the canonical Go module import path
		// if source is empty, we bail as this can
		// lead to a potential K8s API object creation DDOS
		if len(source) == 0 {
			fmt.Println(RunUsage)
			return
		}
		exec = []string{"go", "run", source}
	}

	// start the local runtime
	if err := r.Start(); err != nil {
		fmt.Printf("Could not start: %v", err)
		return
	}

	service := &runtime.Service{
		Name:     name,
		Source:   source,
		Version:  version,
		Metadata: make(map[string]string),
	}

	// default environment
	environment := defaultEnv()
	// add environment variable passed in via cli
	for _, evar := range env {
		for _, e := range strings.Split(evar, ",") {
			if len(e) > 0 {
				environment = append(environment, strings.TrimSpace(e))
			}
		}
	}

	// runtime based on environment we run the service in
	// TODO: how will this work with runtime service
	opts := []runtime.CreateOption{
		runtime.WithCommand(exec...),
		runtime.WithOutput(os.Stdout),
		runtime.WithEnv(environment),
	}

	// run the service
	if err := r.Create(service, opts...); err != nil {
		fmt.Println(err)
		return
	}

	// if in local mode register signal handlers
	if local {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)

		// wait for shutdown
		<-shutdown

		// delete service from runtime
		if err := r.Delete(service); err != nil {
			fmt.Println(err)
			return
		}

		if err := r.Stop(); err != nil {
			fmt.Println(err)
			return
		}
	}
}

func killService(ctx *cli.Context, srvOpts ...micro.Option) {
	// we expect `micro run service`
	if len(ctx.Args()) == 0 || ctx.Args()[0] != "service" {
		fmt.Println(KillUsage)
		return
	}

	// get the args
	name := ctx.String("name")
	version := ctx.String("version")
	local := ctx.Bool("local")

	if len(name) == 0 {
		fmt.Println(KillUsage)
		return
	}

	var r runtime.Runtime
	switch local {
	case true:
		r = *cmd.DefaultCmd.Options().Runtime
	default:
		r = rs.NewRuntime()
	}

	service := &runtime.Service{
		Name:    name,
		Version: version,
	}

	if err := r.Delete(service); err != nil {
		fmt.Println(err)
		return
	}
}

func getService(ctx *cli.Context, srvOpts ...micro.Option) {
	// get the args
	name := ctx.String("name")
	version := ctx.String("version")
	local := ctx.Bool("local")
	runType := ctx.Bool("runtime")

	var r runtime.Runtime
	switch local {
	case true:
		r = *cmd.DefaultCmd.Options().Runtime
	default:
		r = rs.NewRuntime()
	}

	var list bool

	if len(ctx.Args()) == 0 || ctx.Args()[0] != "service" {
		list = true
	}

	var services []*runtime.Service
	var err error

	// return a list of services
	switch list {
	case true:
		// return the runtiem services
		if runType {
			services, err = r.Read(runtime.ReadType("runtime"))
		} else {
			// list all running services
			services, err = r.List()
		}
	// return one service
	default:
		// check if service name was passed in
		if len(name) == 0 {
			fmt.Println(GetUsage)
			return
		}

		// get service with name and version
		opts := []runtime.ReadOption{
			runtime.ReadService(name),
			runtime.ReadVersion(version),
		}

		// return the runtime services
		if runType {
			opts = append(opts, runtime.ReadType("runtime"))
		}

		// read the service
		services, err = r.Read(opts...)
	}

	// check the error
	if err != nil {
		fmt.Println(err)
		return
	}

	// make sure we return UNKNOWN when empty string is supplied
	parse := func(m string) string {
		if len(m) == 0 {
			return "n/a"
		}
		return m
	}

	// don't do anything if there's no services
	if len(services) == 0 {
		return
	}

	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	writer := tabwriter.NewWriter(os.Stdout, 0, 8, 1, '\t', tabwriter.AlignRight)
	fmt.Fprintln(writer, "NAME\tVERSION\tSOURCE\tSTATUS\tBUILD\tMETADATA")
	for _, service := range services {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			service.Name,
			parse(service.Version),
			parse(service.Source),
			parse(service.Metadata["status"]),
			parse(service.Metadata["build"]),
			fmt.Sprintf("owner=%s,group=%s", parse(service.Metadata["owner"]), parse(service.Metadata["group"])))
	}
	writer.Flush()
}
