package main

import (
	"flag"
	"fmt"
	"os"

	"phenix/api/config"
	"phenix/api/experiment"
	"phenix/store"
	"phenix/util"
	"phenix/util/envflag"

	"gopkg.in/yaml.v3"
)

var (
	f_help      bool
	f_storePath string
)

func init() {
	flag.BoolVar(&f_help, "help", false, "show this help message")
	flag.StringVar(&f_storePath, "store", "phenix.bdb", "path to Bolt store file")
}

func main() {
	envflag.Parse("PHENIX")
	flag.Parse()

	if f_help {
		usage()
		return
	}

	if flag.NArg() < 1 {
		usage()
		os.Exit(1)
	}

	if err := store.Init(store.Path(f_storePath)); err != nil {
		fmt.Println("error initializing config store:", err)
		os.Exit(1)
	}

	switch flag.Arg(0) {
	case "list":
		configs, err := config.ListConfigs(flag.Arg(1))

		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Println()

		if len(configs) == 0 {
			fmt.Println("no configs currently exist")
		} else {
			util.PrintTableOfConfigs(os.Stdout, configs)
		}

		fmt.Println()
	case "get":
		c, err := config.GetConfig(flag.Arg(1))
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		m, err := yaml.Marshal(c)
		if err != nil {
			fmt.Println(fmt.Errorf("marshaling config to YAML: %w", err))
			os.Exit(1)
		}

		fmt.Println(string(m))
	case "create":
		if flag.NArg() == 1 {
			fmt.Println("no config files provided")
			os.Exit(1)
		}

		for _, f := range flag.Args()[1:] {
			c, err := config.CreateConfig(f)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

			fmt.Printf("%s/%s config created\n", c.Kind, c.Metadata.Name)
		}
	case "edit":
		c, err := config.EditConfig(flag.Arg(1))
		if err != nil {
			if config.IsConfigNotModified(err) {
				fmt.Println("no changes made to config")
				os.Exit(0)
			}

			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Printf("%s/%s config updated\n", c.Kind, c.Metadata.Name)
	case "delete":
		if err := config.DeleteConfig(flag.Arg(1)); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Printf("%s deleted\n", flag.Arg(1))
	case "experiment":
		switch flag.Arg(1) {
		case "start":
			if err := experiment.Start(flag.Arg(2)); err != nil {
				panic(err)
			}
		case "stop":
			if err := experiment.Stop(flag.Arg(2)); err != nil {
				panic(err)
			}
		default:
			panic("unknown experiment command")
		}
	default:
		panic("unknown command")
	}

	os.Exit(0)
}

func usage() {
	fmt.Fprintln(flag.CommandLine.Output(), "minimega phenix")

	fmt.Fprintln(flag.CommandLine.Output(), "")

	fmt.Fprintln(flag.CommandLine.Output(), "Global Options:")
	flag.PrintDefaults()

	fmt.Fprintln(flag.CommandLine.Output(), "")

	fmt.Fprintln(flag.CommandLine.Output(), "Subcommands:")
	fmt.Fprintln(flag.CommandLine.Output(), "  list [all,topology,scenario,experiment] - get a list of configs")
	fmt.Fprintln(flag.CommandLine.Output(), "  get <kind/name>                         - get an existing config")
	fmt.Fprintln(flag.CommandLine.Output(), "  create <path/to/config>                 - create a new config")
	fmt.Fprintln(flag.CommandLine.Output(), "  edit <kind/name>                        - edit an existing config")
	fmt.Fprintln(flag.CommandLine.Output(), "  delete <kind/name>                      - delete a config")
	fmt.Fprintln(flag.CommandLine.Output(), "  experiment <start,stop> <name>          - start an existing experiment")
	// fmt.Fprintln(flag.CommandLine.Output(), "  help <cmd> - print help message for subcommand")

	fmt.Fprintln(flag.CommandLine.Output(), "")
}