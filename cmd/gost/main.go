package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"strings"
	"encoding/base64"
	"encoding/json"

	"github.com/go-gost/gost/utils"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/parsing"
	xlogger "github.com/go-gost/x/logger"
	xmetrics "github.com/go-gost/x/metrics"
	"github.com/go-gost/x/registry"
)

var (
	log logger.Logger

	cfgFile      string
	outputFormat string
	services     stringList
	nodes        stringList
	debug        bool
	apiAddr      string
	metricsAddr  string
)

func init() {
	var printVersion bool
	var fastOpen     bool

	localHost := os.Getenv("SS_LOCAL_HOST")
	localPort := os.Getenv("SS_LOCAL_PORT")
	remoteHost := os.Getenv("SS_REMOTE_HOST")
	remotePort := os.Getenv("SS_REMOTE_PORT")
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
				arg = strings.ReplaceAll(arg, "#SS_HOST", "#SS_REMOTE_HOST")
				arg = strings.ReplaceAll(arg, "#SS_PORT", "#SS_REMOTE_PORT")
				arg = strings.ReplaceAll(arg, "#SS_REMOTE_HOST", remoteHost)
				arg = strings.ReplaceAll(arg, "#SS_REMOTE_PORT", remotePort)
				arg = strings.ReplaceAll(arg, "#SS_LOCAL_HOST", localHost)
				arg = strings.ReplaceAll(arg, "#SS_LOCAL_PORT", localPort)
				os.Args = append(os.Args, arg)
			}
		}
		myDataDir := strings.ReplaceAll(cfg.DataDir, "com.github.shadowsocks.plugin.gost", "com.github.shadowsocks")
		os.MkdirAll(myDataDir, 0700)
		err = os.Chdir(myDataDir)
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
		pluginOptions = strings.ReplaceAll(pluginOptions, "#SS_HOST", remoteHost)
		pluginOptions = strings.ReplaceAll(pluginOptions, "#SS_PORT", remotePort)

		os.Args = append(os.Args, "-L")
		os.Args = append(os.Args, fmt.Sprintf("ss+tcp://none@[%s]:%s", localHost, localPort))
		os.Args = append(os.Args, strings.Split(pluginOptions, " ")...)
	}

	flag.Var(&services, "L", "service list")
	flag.Var(&nodes, "F", "chain node list")
	flag.StringVar(&cfgFile, "C", "", "configure file")
	flag.BoolVar(&utils.VpnMode, "V", false, "VPN Mode")
	flag.BoolVar(&fastOpen, "fast-open", false, "fast Open TCP")
	flag.BoolVar(&printVersion, "PV", false, "print version")
	flag.StringVar(&outputFormat, "O", "", "output format, one of yaml|json format")
	flag.BoolVar(&debug, "D", false, "debug mode")
	flag.StringVar(&apiAddr, "api", "", "api service address")
	flag.StringVar(&metricsAddr, "metrics", "", "metrics service address")
	flag.Parse()

	if printVersion {
		fmt.Fprintf(os.Stdout, "gost %s (%s %s/%s)\n",
			version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	if localHost == "" || localPort == "" {
		fmt.Fprintln(os.Stderr, "Can only be used in the shadowsocks plugin.")
		os.Exit(1)
	}
	utils.Init()

	log = xlogger.NewLogger()
	logger.SetDefault(log)
}

func main() {
	cfg := &config.Config{}
	var err error
	if len(services) > 0 || apiAddr != "" {
		cfg, err = buildConfigFromCmd(services, nodes)
		if err != nil {
			log.Fatal(err)
		}
		if debug && cfg != nil {
			if cfg.Log == nil {
				cfg.Log = &config.LogConfig{}
			}
			cfg.Log.Level = string(logger.DebugLevel)
		}
		if apiAddr != "" {
			cfg.API = &config.APIConfig{
				Addr: apiAddr,
			}
		}
		if metricsAddr != "" {
			cfg.Metrics = &config.MetricsConfig{
				Addr: metricsAddr,
			}
		}
	} else {
		if cfgFile != "" {
			err = cfg.ReadFile(cfgFile)
		} else {
			err = cfg.Load()
		}
		if err != nil {
			log.Fatal(err)
		}
	}

	log = logFromConfig(cfg.Log)

	logger.SetDefault(log)

	if outputFormat != "" {
		if err := cfg.Write(os.Stdout, outputFormat); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}

	if cfg.Profiling != nil {
		go func() {
			addr := cfg.Profiling.Addr
			if addr == "" {
				addr = ":6060"
			}
			log.Info("profiling server on ", addr)
			log.Fatal(http.ListenAndServe(addr, nil))
		}()
	}

	if cfg.API != nil {
		s, err := buildAPIService(cfg.API)
		if err != nil {
			log.Fatal(err)
		}
		defer s.Close()

		go func() {
			log.Info("api service on ", s.Addr())
			log.Fatal(s.Serve())
		}()
	}

	if cfg.Metrics != nil {
		xmetrics.Init(xmetrics.NewMetrics())
		if cfg.Metrics.Addr != "" {
			s, err := buildMetricsService(cfg.Metrics)
			if err != nil {
				log.Fatal(err)
			}
			go func() {
				defer s.Close()
				log.Info("metrics service on ", s.Addr())
				log.Fatal(s.Serve())
			}()
		}
	}

	parsing.BuildDefaultTLSConfig(cfg.TLS)

	services := buildService(cfg)
	for _, svc := range services {
		svc := svc
		go func() {
			svc.Serve()
			// svc.Close()
		}()
	}

	config.SetGlobal(cfg)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	for sig := range sigs {
		switch sig {
		case syscall.SIGHUP:
			return
		default:
			for name, srv := range registry.ServiceRegistry().GetAll() {
				srv.Close()
				log.Debugf("service %s shutdown", name)
			}
			return
		}
	}
}
