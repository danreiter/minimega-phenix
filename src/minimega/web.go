// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.
//Author: Brian Wright

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"minicli"
	log "minilog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/websocket"
)

const (
	defaultWebPort = 9001
	defaultWebRoot = "misc/web"
	friendlyError  = "oops, something went wrong"
)

type vmScreenshotParams struct {
	Host string
	Name string
	Port int
	ID   int
	Size int
}

var web struct {
	Running bool
	Server  *http.Server
	Port    int
	Root    string
}

var webCLIHandlers = []minicli.Handler{
	{ // web
		HelpShort: "start the minimega webserver",
		HelpLong: `
Launch the minimega webserver. Running web starts the HTTP server whose port
cannot be changed once started. The default port is 9001. To run the server on
a different port, run:

	web 10000

The webserver requires several resources found in misc/web in the repo. By
default, it looks in $PWD/misc/web for these resources. If you are running
minimega from a different location, you can specify a different path using:

	web root <path/to/web/dir>

You can also set the port when starting web with an alternative root directory:

	web root <path/to/web/dir> 10000

NOTE: If you start the webserver with an invalid root, you can safely re-run
"web root" to update it. You cannot, however, change the server's port.`,
		Patterns: []string{
			"web [port]",
			"web root <path> [port]",
		},
		Call: wrapSimpleCLI(cliWeb),
	},
}

func cliWeb(c *minicli.Command, resp *minicli.Response) error {
	port := defaultWebPort
	if c.StringArgs["port"] != "" {
		// Check if port is an integer
		p, err := strconv.Atoi(c.StringArgs["port"])
		if err != nil {
			return fmt.Errorf("'%v' is not a valid port", c.StringArgs["port"])
		}

		port = p
	}

	root := defaultWebRoot
	if c.StringArgs["path"] != "" {
		root = c.StringArgs["path"]
	}

	go webStart(port, root)

	return nil
}

func webStart(port int, root string) {
	web.Root = root

	mux := http.NewServeMux()
	for _, v := range []string{"css", "fonts", "js", "libs", "novnc", "images", "xterm.js"} {
		path := fmt.Sprintf("/%s/", v)
		dir := http.Dir(filepath.Join(root, v))
		mux.Handle(path, http.StripPrefix(path, http.FileServer(dir)))
	}

	mux.HandleFunc("/", webIndex)
	mux.HandleFunc("/vms", webTemplated)
	mux.HandleFunc("/hosts", webTemplated)
	mux.HandleFunc("/graph", webTemplated)
	mux.HandleFunc("/tilevnc", webTemplated)
	mux.HandleFunc("/hosts.json", webHostsJSON)
	mux.HandleFunc("/vms.json", webVMsJSON)
	mux.HandleFunc("/vlans.json", webVLANsJSON)
	mux.HandleFunc("/connect/", webConnect)
	mux.HandleFunc("/screenshot/", webScreenshot)
	mux.Handle("/tunnel/", websocket.Handler(tunnelHandler))

	if web.Server == nil {
		web.Server = &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: mux,
		}

		err := web.Server.ListenAndServe()
		if err != nil {
			log.Error("web: %v", err)
			web.Server = nil
		} else {
			web.Port = port
			web.Running = true
		}
	} else {
		log.Info("web: changing web root to: %s", root)
		if port != web.Port && port != defaultWebPort {
			log.Error("web: changing web's port is not supported")
		}
		// just update the mux
		web.Server.Handler = mux
	}
}

// webScreenshot serves routes like /screenshot/<host>/<id>.png. Optional size
// query parameter dictates the size of the screenshot.
func webScreenshot(w http.ResponseWriter, r *http.Request) {
	fields := strings.Split(r.URL.Path, "/")
	if len(fields) != 4 {
		http.NotFound(w, r)
		return
	}
	fields = fields[2:]

	size := r.URL.Query().Get("size")
	host := fields[0]
	id := strings.TrimSuffix(fields[1], ".png")
	do_encode := r.URL.Query().Get("base64") != ""

	cmdStr := fmt.Sprintf("vm screenshot %s file /dev/null %s", id, size)
	if host != hostname {
		cmdStr = fmt.Sprintf("mesh send %s %s", host, cmdStr)
	}

	cmd := minicli.MustCompile(cmdStr)
	cmd.SetRecord(false)

	var screenshot []byte

	for resps := range RunCommands(cmd) {
		for _, resp := range resps {
			if resp.Error != "" {
				if strings.HasPrefix(resp.Error, "vm not running:") {
					continue
				} else if resp.Error == "cannot take screenshot of container" {
					continue
				} else if strings.HasPrefix(resp.Error, "cannot take screenshot of container") {
					continue
				}

				// Unknown error
				log.Errorln(resp.Error)
				http.Error(w, friendlyError, http.StatusInternalServerError)
				return
			}

			if resp.Data == nil {
				http.NotFound(w, r)
			}

			if screenshot == nil {
				screenshot = resp.Data.([]byte)
			} else {
				log.Error("received more than one response for vm screenshot")
			}
		}
	}

	if screenshot != nil {
		if do_encode {
			base64string := "data:image/png;base64," + base64.StdEncoding.EncodeToString(screenshot)
			w.Write([]byte(base64string))
		} else {
			w.Write(screenshot)
		}
	} else {
		http.NotFound(w, r)
	}
}

// Redirect / to /vms
func webIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	http.Redirect(w, r, "/vms", 302)
}

// Templated HTML responses
func webTemplated(w http.ResponseWriter, r *http.Request) {
	lp := filepath.Join(web.Root, "templates", "_layout.tmpl")
	fp := filepath.Join(web.Root, "templates", r.URL.Path+".tmpl")

	info, err := os.Stat(fp)
	if err != nil {
		// 404 if template doesn't exist
		http.NotFound(w, r)
		return
	}

	if info.IsDir() {
		// 404 if directory
		http.NotFound(w, r)
		return
	}

	tmpl, err := template.ParseFiles(lp, fp)
	if err != nil {
		log.Error(err.Error())
		http.Error(w, http.StatusText(500), 500)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "layout", nil); err != nil {
		log.Error(err.Error())
		http.Error(w, http.StatusText(500), 500)
	}
}

func webConnect(w http.ResponseWriter, r *http.Request) {
	// URL should be of the form `/connect/<name>`
	fields := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(fields) != 2 {
		return
	}
	name := fields[1]

	vms := GlobalVMs()
	vm := vms.findVM(name, true)
	if vm == nil {
		http.NotFound(w, r)
		return
	}

	// set no-cache headers
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate") // HTTP 1.1.
	w.Header().Set("Pragma", "no-cache")                                   // HTTP 1.0.
	w.Header().Set("Expires", "0")                                         // Proxies.

	switch vm.GetType() {
	case KVM:
		http.ServeFile(w, r, filepath.Join(web.Root, "vnc_auto.html"))
	case CONTAINER:
		http.ServeFile(w, r, filepath.Join(web.Root, "terminal.html"))
	default:
		http.NotFound(w, r)
	}
}

// JSON responses below

func webHostsJSON(w http.ResponseWriter, r *http.Request) {
	hosts := [][]interface{}{}

	cmd := minicli.MustCompile("host")
	cmd.SetRecord(false)

	for resps := range runCommandGlobally(cmd) {
		for _, resp := range resps {
			if resp.Error != "" {
				log.Errorln(resp.Error)
				continue
			}

			for _, row := range resp.Tabular {
				res := []interface{}{}
				for _, v := range row {
					res = append(res, v)
				}
				hosts = append(hosts, res)
			}
		}
	}

	js, err := json.Marshal(hosts)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(js)
}

func webVMsJSON(w http.ResponseWriter, r *http.Request) {
	// we want a map of "hostname + id" to vm info so that it can be sorted
	infovms := make(map[string]map[string]interface{}, 0)

	vms := GlobalVMs()

	for _, vm := range vms {
		stateMask := VM_QUIT | VM_ERROR

		if vm.GetState()&stateMask != 0 {
			continue
		}

		config := getConfig(vm)

		vmMap := map[string]interface{}{
			"namespace": config.Namespace,
			"host":      vm.GetHost(),
			"id":        vm.GetID(),
			"name":      vm.GetName(),
			"state":     vm.GetState().String(),
			"type":      vm.GetType().String(),
			"activecc":  vm.IsCCActive(),
			"vcpus":     config.Vcpus,
			"memory":    config.Memory,
			"snapshot":  config.Snapshot,
			"uiud":      config.UUID,
		}

		if vm, ok := vm.(*KvmVM); ok {
			vmMap["vnc_port"] = vm.VNCPort
			vmMap["kvm_initrdpath"] = vm.KVMConfig.InitrdPath
			vmMap["kvm_kernelpath"] = vm.KVMConfig.KernelPath
			if vm.KVMConfig.DiskPaths == nil {
				vmMap["kvm_diskpaths"] = make([]int, 0)
			} else {
				vmMap["kvm_diskpaths"] = vm.KVMConfig.DiskPaths
			}
		}

		if vm, ok := vm.(*ContainerVM); ok {
			vmMap["console_port"] = vm.ConsolePort
			vmMap["container_fspath"] = vm.ContainerConfig.FSPath
			vmMap["container_preinit"] = vm.ContainerConfig.Preinit
			if vm.ContainerConfig.Init == nil {
				vmMap["container_init"] = make([]int, 0)
			} else {
				vmMap["container_init"] = vm.ContainerConfig.Init
			}
		}

		if config.Networks == nil {
			vmMap["network"] = make([]int, 0)
		} else {
			vmMap["network"] = config.Networks
		}

		if vm.GetTags() == nil {
			vmMap["tags"] = make(map[string]string, 0)
		} else {
			vmMap["tags"] = vm.GetTags()
		}

		// The " " is invalid as a hostname, so we use it as a separator.
		infovms[vm.GetHost()+" "+strconv.Itoa(vm.GetID())] = vmMap
	}

	// We need to pass it as an array for the JSON generation (so the weird keys don't show up)
	infoslice := make([]map[string]interface{}, len(infovms))

	// Make a slice of all keys in infovms, then sort it
	keys := []string{}
	for k, _ := range infovms {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Make a sorted slice of values from the sorted slice of keys
	for i, k := range keys {
		infoslice[i] = infovms[k]
	}

	// Now the order of items in the JSON doesn't randomly change between calls (since the values are sorted)
	js, err := json.Marshal(infoslice)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(js)
}

func webVLANsJSON(w http.ResponseWriter, r *http.Request) {
	vlans := [][]interface{}{}

	cmd := minicli.MustCompile("vlans")
	cmd.SetRecord(false)

	for resps := range runCommandGlobally(cmd) {
		for _, resp := range resps {
			if resp.Error != "" {
				log.Errorln(resp.Error)
				continue
			}

			for _, row := range resp.Tabular {
				res := []interface{}{}
				for _, v := range row {
					res = append(res, v)
				}
				vlans = append(vlans, res)
			}
		}
	}

	js, err := json.Marshal(vlans)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(js)
}
