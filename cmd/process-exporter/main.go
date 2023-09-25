package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/ncabatoff/fakescraper"
	common "github.com/ncabatoff/process-exporter"
	"github.com/ncabatoff/process-exporter/collector"
	"github.com/ncabatoff/process-exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	promVersion "github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"
)

// Version is set at build time use ldflags.
var version string

func printManual() {
	fmt.Print(`Usage:
  process-exporter [options] -config.path filename.yml

or 

  process-exporter [options] -procnames name1,...,nameN [-namemapping k1,v1,...,kN,vN]

The recommended option is to use a config file, but for convenience and
backwards compatibility the -procnames/-namemapping options exist as an
alternative.
 
The -children option (default:true) makes it so that any process that otherwise
isn't part of its own group becomes part of the first group found (if any) when
walking the process tree upwards.  In other words, resource usage of
subprocesses is added to their parent's usage unless the subprocess identifies
as a different group name.

Command-line process selection (procnames/namemapping):

  Every process not in the procnames list is ignored.  Otherwise, all processes
  found are reported on as a group based on the process name they share. 
  Here 'process name' refers to the value found in the second field of
  /proc/<pid>/stat, which is truncated at 15 chars.

  The -namemapping option allows assigning a group name based on a combination of
  the process name and command line.  For example, using 

    -namemapping "python2,([^/]+)\.py,java,-jar\s+([^/]+).jar" 

  will make it so that each different python2 and java -jar invocation will be
  tracked with distinct metrics.  Processes whose remapped name is absent from
  the procnames list will be ignored.  Here's an example that I run on my home
  machine (Ubuntu Xenian):

    process-exporter -namemapping "upstart,(--user)" \
      -procnames chromium-browse,bash,prometheus,prombench,gvim,upstart:-user

  Since it appears that upstart --user is the parent process of my X11 session,
  this will make all apps I start count against it, unless they're one of the
  others named explicitly with -procnames.

Config file process selection (filename.yml):

  See README.md.
` + "\n")

}

type (
	prefixRegex struct {
		prefix string
		regex  *regexp.Regexp
	}

	nameMapperRegex struct {
		mapping map[string]*prefixRegex
	}
)

func (nmr *nameMapperRegex) String() string {
	return fmt.Sprintf("%+v", nmr.mapping)
}

// Create a nameMapperRegex based on a string given as the -namemapper argument.
func parseNameMapper(s string) (*nameMapperRegex, error) {
	mapper := make(map[string]*prefixRegex)
	if s == "" {
		return &nameMapperRegex{mapper}, nil
	}

	toks := strings.Split(s, ",")
	if len(toks)%2 == 1 {
		return nil, fmt.Errorf("bad namemapper: odd number of tokens")
	}

	for i, tok := range toks {
		if tok == "" {
			return nil, fmt.Errorf("bad namemapper: token %d is empty", i)
		}
		if i%2 == 1 {
			name, regexstr := toks[i-1], tok
			matchName := name
			prefix := name + ":"

			if r, err := regexp.Compile(regexstr); err != nil {
				return nil, fmt.Errorf("error compiling regexp '%s': %v", regexstr, err)
			} else {
				mapper[matchName] = &prefixRegex{prefix: prefix, regex: r}
			}
		}
	}

	return &nameMapperRegex{mapper}, nil
}

func (nmr *nameMapperRegex) MatchAndName(nacl common.ProcAttributes) (bool, string) {
	if pregex, ok := nmr.mapping[nacl.Name]; ok {
		if pregex == nil {
			return true, nacl.Name
		}
		matches := pregex.regex.FindStringSubmatch(strings.Join(nacl.Cmdline, " "))
		if len(matches) > 1 {
			for _, matchstr := range matches[1:] {
				if matchstr != "" {
					return true, pregex.prefix + matchstr
				}
			}
		}
	}

	return false, ""
}

func init() {
	prometheus.MustRegister(promVersion.NewCollector("process_exporter"))
}

func main() {
	var (
		webConfig         = kingpinflag.AddFlags(kingpin.CommandLine, ":9256")
		metricsPath       = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		onceToStdoutDelay = kingpin.Flag("once-to-stdout-delay", "Don't bind, just wait this much time, print the metrics once to stdout, and exit").Default("0").Duration()
		procNames         = kingpin.Flag("procnames", "comma-separated list of process names to monitor").Default("").String()
		procfsPath        = kingpin.Flag("procfs", "path to read proc data from").Default("/proc").String()
		nameMapping       = kingpin.Flag("namemapping", "comma-separated list, alternating process name and capturing regex to apply to cmdline").Default("").String()
		children          = kingpin.Flag("children", "if a proc is tracked, track with it any children that aren't part of their own group").Default("true").Bool()
		threads           = kingpin.Flag("threads", "report on per-threadname metrics as well").Default("true").Bool()
		smaps             = kingpin.Flag("gather-smaps", "gather metrics from smaps file, which contains proportional resident memory size").Default("true").Bool()
		man               = kingpin.Flag("man", "path to YAML config file").Default("false").Bool()
		configPath        = kingpin.Flag("config.path", "path to YAML config file").Default("").String()
		recheck           = kingpin.Flag("recheck", "recheck process names on each scrape").Default("false").Bool()
		debug             = kingpin.Flag("debug", "log debugging information to stdout").Default("false").Bool()
		// showVersion       = kingpin.Flag("version", "print version information and exit").Default("false").Bool()
	)
	kingpin.Version(promVersion.Print(version))
	kingpin.CommandLine.HelpFlag.Short('h')
	kingpin.Parse()

	promlogConfig := &promlog.Config{}
	logger := promlog.New(promlogConfig)

	// if *showVersion {
	// 	fmt.Printf("%s\n", promVersion.Print("process-exporter"))
	// 	os.Exit(0)
	// }

	if *man {
		printManual()
		return
	}

	var matchnamer common.MatchNamer

	if *configPath != "" {
		if *nameMapping != "" || *procNames != "" {
			log.Fatalf("-config.path cannot be used with -namemapping or -procnames")
		}

		cfg, err := config.ReadFile(*configPath, *debug)
		if err != nil {
			log.Fatalf("error reading config file %q: %v", *configPath, err)
		}
		log.Printf("Reading metrics from %s based on %q", *procfsPath, *configPath)
		matchnamer = cfg.MatchNamers
		if *debug {
			log.Printf("using config matchnamer: %v", cfg.MatchNamers)
		}
	} else {
		namemapper, err := parseNameMapper(*nameMapping)
		if err != nil {
			log.Fatalf("Error parsing -namemapping argument '%s': %v", *nameMapping, err)
		}

		var names []string
		for _, s := range strings.Split(*procNames, ",") {
			if s != "" {
				if _, ok := namemapper.mapping[s]; !ok {
					namemapper.mapping[s] = nil
				}
				names = append(names, s)
			}
		}

		log.Printf("Reading metrics from %s for procnames: %v", *procfsPath, names)
		if *debug {
			log.Printf("using cmdline matchnamer: %v", namemapper)
		}
		matchnamer = namemapper
	}

	pc, err := collector.NewProcessCollector(
		collector.ProcessCollectorOption{
			ProcFSPath:  *procfsPath,
			Children:    *children,
			Threads:     *threads,
			GatherSMaps: *smaps,
			Namer:       matchnamer,
			Recheck:     *recheck,
			Debug:       *debug,
		},
	)
	if err != nil {
		log.Fatalf("Error initializing: %v", err)
	}

	prometheus.MustRegister(pc)

	if *onceToStdoutDelay != 0 {
		// We throw away the first result because that first collection primes the pump, and
		// otherwise we won't see our counter metrics.  This is specific to the implementation
		// of NamedProcessCollector.Collect().
		fscraper := fakescraper.NewFakeScraper()
		fscraper.Scrape()
		time.Sleep(*onceToStdoutDelay)
		fmt.Print(fscraper.Scrape())
		return
	}

	http.Handle(*metricsPath, promhttp.Handler())

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Named Process Exporter</title></head>
			<body>
			<h1>Named Process Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})
	server := &http.Server{}
	if err := web.ListenAndServe(server, webConfig, logger); err != nil {
		log.Fatalf("Failed to start the server: %v", err)
		os.Exit(1)
	}
}
