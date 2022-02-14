package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"encoding/base64"
	"encoding/json"

	_ "net/http/pprof"

	"github.com/ginuerzh/gost"
	"github.com/ginuerzh/gost/utils"
	"github.com/go-log/log"
)

var (
	configureFile string
	baseCfg       = &baseConfig{}
	pprofAddr     string
	pprofEnabled  = os.Getenv("PROFILING") != ""
)

func init() {
	gost.SetLogger(&gost.LogLogger{})

	var (
		printVersion bool
		fastOpen     bool
	)
	localHost := os.Getenv("SS_LOCAL_HOST")
	localPort := os.Getenv("SS_LOCAL_PORT")
	pluginOptions := os.Getenv("SS_PLUGIN_OPTIONS")

	splitted := strings.Split(pluginOptions, " ")
	var encoded string = ""
	for _, subString := range splitted {
		if strings.HasPrefix(subString, "CFGBLOB=") {
			encoded = subString[len("CFGBLOB="):]
			break
		}
	}
	if encoded != "" {
		jsonBytes, err := base64.StdEncoding.WithPadding('_').DecodeString(encoded)
		if err != nil {
			fmt.Fprintln(os.Stderr, "base64 decode error:", err)
			os.Exit(2)
		}
		type cfgblob struct {
			CmdArgs	[][]string
			DataDir	string
			Files	map[string]string
		}
		var cfg cfgblob
		err = json.Unmarshal([]byte(jsonBytes), &cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "json unmarshal error:", err)
			os.Exit(2)
		}
		for _, oneOrTwoArgs := range cfg.CmdArgs {
			for _, arg := range oneOrTwoArgs {
				arg = strings.ReplaceAll(arg, "#SS_LOCAL_HOST", localHost)
				arg = strings.ReplaceAll(arg, "#SS_LOCAL_PORT", localPort)
				os.Args = append(os.Args, arg)
			}
		}
		err = os.Chdir(cfg.DataDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "chdir error:", err)
			os.Exit(2)
		}
		os.Mkdir("gost_files", 0700)
		err = os.Chdir("gost_files")
		if err != nil {
			fmt.Fprintln(os.Stderr, "chdir error:", err)
			os.Exit(2)
		}
		existing, err := os.ReadDir(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, "readdir error:", err)
			os.Exit(2)
		}
		for _, dirEntry := range existing {
			err = os.Remove(dirEntry.Name())
			if err != nil {
				fmt.Fprintln(os.Stderr, "cannot remove existing file, error:", err)
			}
		}
		for fileName, fileData := range cfg.Files {
			err = os.WriteFile(fileName, []byte(fileData), 0600)
			if err != nil {
				fmt.Fprintln(os.Stderr, "writefile error:", err)
				os.Exit(2)
			}
		}
	} else {
		pluginOptions = strings.ReplaceAll(pluginOptions, "#SS_HOST", os.Getenv("SS_REMOTE_HOST"))
		pluginOptions = strings.ReplaceAll(pluginOptions, "#SS_PORT", os.Getenv("SS_REMOTE_PORT"))

		os.Args = append(os.Args, "-L")
		os.Args = append(os.Args, fmt.Sprintf("ss+tcp://none@[%s]:%s", localHost, localPort))
		os.Args = append(os.Args, strings.Split(pluginOptions, " ")...)
	}

	flag.Var(&baseCfg.route.ChainNodes, "F", "forward address, can make a forward chain")
	flag.Var(&baseCfg.route.ServeNodes, "L", "listen address, can listen on multiple ports (required)")
	flag.StringVar(&configureFile, "C", "", "configure file")
	flag.BoolVar(&baseCfg.Debug, "D", false, "enable debug log")
	flag.BoolVar(&utils.VpnMode, "V", false, "VPN Mode")
	flag.BoolVar(&fastOpen, "fast-open", false, "fast Open TCP")
	flag.BoolVar(&printVersion, "PV", false, "print version")
	if pprofEnabled {
		flag.StringVar(&pprofAddr, "P", ":6060", "profiling HTTP server address")
	}
	flag.Parse()

	if printVersion {
		fmt.Fprintf(os.Stdout, "gost %s (%s %s/%s)\n",
			gost.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	if localHost == "" || localPort == "" {
		fmt.Fprintln(os.Stderr, "Can only be used in the shadowsocks plugin.")
		os.Exit(1)
	}
	utils.Init()

	if configureFile != "" {
		_, err := parseBaseConfig(configureFile)
		if err != nil {
			log.Log(err)
			os.Exit(1)
		}
	}
	if flag.NFlag() == 0 {
		flag.PrintDefaults()
		os.Exit(0)
	}
}

func main() {
	if pprofEnabled {
		go func() {
			log.Log("profiling server on", pprofAddr)
			log.Log(http.ListenAndServe(pprofAddr, nil))
		}()
	}

	// NOTE: as of 2.6, you can use custom cert/key files to initialize the default certificate.
	tlsConfig, err := tlsConfig(defaultCertFile, defaultKeyFile, "")
	if err != nil {
		// generate random self-signed certificate.
		cert, err := gost.GenCertificate()
		if err != nil {
			log.Log(err)
			os.Exit(1)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
	} else {
		log.Log("load TLS certificate files OK")
	}

	gost.DefaultTLSConfig = tlsConfig

	if err := start(); err != nil {
		log.Log(err)
		os.Exit(1)
	}

	select {}
}

func start() error {
	gost.Debug = baseCfg.Debug

	var routers []router
	rts, err := baseCfg.route.GenRouters()
	if err != nil {
		return err
	}
	routers = append(routers, rts...)

	for _, route := range baseCfg.Routes {
		rts, err := route.GenRouters()
		if err != nil {
			return err
		}
		routers = append(routers, rts...)
	}

	if len(routers) == 0 {
		return errors.New("invalid config")
	}
	for i := range routers {
		go routers[i].Serve()
	}

	return nil
}
