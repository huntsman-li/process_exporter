package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"regexp"
	"strings"

	"github.com/ncabatoff/fakescraper"
	common "process_exporter"
	"process_exporter/config"
	"process_exporter/proc"
	"github.com/prometheus/client_golang/prometheus"
	"strconv"
)

func printManual() {
	fmt.Print(`Usage:
  process-exporter [options] -config.path filename.yml

or 

  process-exporter [options] -procnames name1,...,nameN [-namemapping k1,v1,...,kN,vN]

The recommended option is to use a config file, but for convenience and
backwards compatability the -procnames/-namemapping options exist as an
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

var (
	numprocsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_num_procs",
		"number of processes in this group",
		[]string{"groupname", "command"},
		nil)

	cpuUserSecsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cpu_user_seconds_total",
		"Cpu user usage in seconds",
		[]string{"groupname", "command"},
		nil)

	cpuSystemSecsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cpu_system_seconds_total",
		"Cpu system usage in seconds",
		[]string{"groupname", "command"},
		nil)

	readBytesDesc = prometheus.NewDesc(
		"namedprocess_namegroup_read_bytes_total",
		"number of bytes read by this group",
		[]string{"groupname", "command"},
		nil)

	writeBytesDesc = prometheus.NewDesc(
		"namedprocess_namegroup_write_bytes_total",
		"number of bytes written by this group",
		[]string{"groupname", "command"},
		nil)

	majorPageFaultsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_major_page_faults_total",
		"Major page faults",
		[]string{"groupname", "command"},
		nil)

	minorPageFaultsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_minor_page_faults_total",
		"Minor page faults",
		[]string{"groupname", "command"},
		nil)

	membytesDesc = prometheus.NewDesc(
		"namedprocess_namegroup_memory_bytes",
		"number of bytes of memory in use",
		[]string{"groupname", "memtype", "command"},
		nil)

	openFDsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_open_filedesc",
		"number of open file descriptors for this group",
		[]string{"groupname", "command"},
		nil)

	worstFDRatioDesc = prometheus.NewDesc(
		"namedprocess_namegroup_worst_fd_ratio",
		"the worst (closest to 1) ratio between open fds and max fds among all procs in this group",
		[]string{"groupname", "command"},
		nil)

	startTimeDesc = prometheus.NewDesc(
		"namedprocess_namegroup_oldest_start_time_seconds",
		"start time in seconds since 1970/01/01 of oldest process in group",
		[]string{"groupname", "command"},
		nil)

	numThreadsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_num_threads",
		"Number of threads",
		[]string{"groupname", "command"},
		nil)

	scrapeErrorsDesc = prometheus.NewDesc(
		"namedprocess_scrape_errors",
		"general scrape errors: no proc metrics collected during a cycle",
		nil,
		nil)

	scrapeProcReadErrorsDesc = prometheus.NewDesc(
		"namedprocess_scrape_procread_errors",
		"incremented each time a proc's metrics collection fails",
		nil,
		nil)

	scrapePartialErrorsDesc = prometheus.NewDesc(
		"namedprocess_scrape_partial_errors",
		"incremented each time a tracked proc's metrics collection fails partially, e.g. unreadable I/O stats",
		nil,
		nil)

	threadCountDesc = prometheus.NewDesc(
		"namedprocess_namegroup_thread_count",
		"Number of threads in this group with same threadname",
		[]string{"groupname", "threadname", "command"},
		nil)

	threadCpuSecsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_thread_cpu_seconds_total",
		"Cpu user/system usage in seconds",
		[]string{"groupname", "threadname", "cpumode", "command"},
		nil)

	threadIoBytesDesc = prometheus.NewDesc(
		"namedprocess_namegroup_thread_io_bytes_total",
		"number of bytes read/written by these threads",
		[]string{"groupname", "threadname", "iomode", "command"},
		nil)

	threadMajorPageFaultsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_thread_major_page_faults_total",
		"Major page faults for these threads",
		[]string{"groupname", "threadname", "command"},
		nil)

	threadMinorPageFaultsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_thread_minor_page_faults_total",
		"Minor page faults for these threads",
		[]string{"groupname", "threadname", "command"},
		nil)
	ProcessStatus = prometheus.NewDesc(
		"namedprocess_namegroup_process_status",
		"process status",
		[]string{"groupname", "status", "command"},
		nil)
	UnknownProcessDesc = prometheus.NewDesc(
		"namedprocess_namegroup_unkown_process",
		"Minor page faults for these threads",
		[]string{"groupname", "pid", "starttime", "command", "procs", "numthreads", "openfds", "readbytes", "writebytes"},
		nil)
)

type (
	prefixRegex struct {
		prefix string
		regex  *regexp.Regexp
	}

	nameMapperRegex struct {
		mapping map[string]*prefixRegex
	}
)

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

func (nmr *nameMapperRegex) MatchAndName(nacl common.NameAndCmdline) (bool, string) {
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

var (
	procnames []string
)

func main() {
	var (
		listenAddress = flag.String("web.listen-address", ":9256",
			"Address on which to expose metrics and web interface.")
		metricsPath = flag.String("web.telemetry-path", "/metrics",
			"Path under which to expose metrics.")
		onceToStdout = flag.Bool("once-to-stdout", false,
			"Don't bind, instead just print the metrics once to stdout and exit")
		procNames = flag.String("procnames", "",
			"comma-seperated list of process names to monitor")
		procfsPath = flag.String("procfs", "/proc",
			"path to read proc data from")
		nameMapping = flag.String("namemapping", "",
			"comma-seperated list, alternating process name and capturing regex to apply to cmdline")
		children = flag.Bool("children", true,
			"if a proc is tracked, track with it any children that aren't part of their own group")
		threads = flag.Bool("threads", false,
			"report on per-threadname metrics as well")
		man = flag.Bool("man", false,
			"print manual")
		configPath = flag.String("config.path", "",
			"path to YAML config file")
	)
	flag.Parse()

	if *man {
		printManual()
		return
	}

	var matchnamer common.MatchNamer

	if *configPath != "" {
		if *nameMapping != "" || *procNames != "" {
			log.Fatalf("-config.path cannot be used with -namemapping or -procnames")
		}

		cfg, err := config.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("error reading config file %q: %v", *configPath, err)
		}
		log.Printf("Reading metrics from %s based on %q", *procfsPath, *configPath)
		matchnamer = cfg.MatchNamers
		procnames = cfg.Names
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

		procnames = names

		log.Printf("Reading metrics from %s for procnames: %v", *procfsPath, names)
		matchnamer = namemapper
	}
	pc, err := NewProcessCollector(*procfsPath, *children, *threads, matchnamer)
	if err != nil {
		log.Fatalf("Error initializing: %v", err)
	}

	prometheus.MustRegister(pc)

	if *onceToStdout {
		// We throw away the first result because that first collection primes the pump, and
		// otherwise we won't see our counter metrics.  This is specific to the implementation
		// of NamedProcessCollector.Collect().
		fscraper := fakescraper.NewFakeScraper()
		fscraper.Scrape()
		fmt.Print(fscraper.Scrape())
		return
	}

	http.Handle(*metricsPath, prometheus.Handler())

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Named Process Exporter</title></head>
			<body>
			<h1>Named Process Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatalf("Unable to setup HTTP server: %v", err)
	}
}

type (
	scrapeRequest struct {
		results chan<- prometheus.Metric
		done    chan struct{}
	}

	NamedProcessCollector struct {
		scrapeChan chan scrapeRequest
		*proc.Grouper
		threads              bool
		source               proc.Source
		scrapeErrors         int
		scrapeProcReadErrors int
		scrapePartialErrors  int
	}
)

func NewProcessCollector(
	procfsPath string,
	children bool,
	threads bool,
	n common.MatchNamer,
) (*NamedProcessCollector, error) {
	fs, err := proc.NewFS(procfsPath)
	if err != nil {
		return nil, err
	}
	p := &NamedProcessCollector{
		scrapeChan: make(chan scrapeRequest),
		Grouper:    proc.NewGrouper(n, children, threads),
		source:     fs,
		threads:    threads,
	}

	colErrs, _, err := p.Update(p.source.AllProcs())
	if err != nil {
		return nil, err
	}
	p.scrapePartialErrors += colErrs.Partial
	p.scrapeProcReadErrors += colErrs.Read

	go p.start()

	return p, nil
}

// Describe implements prometheus.Collector.
func (p *NamedProcessCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cpuUserSecsDesc
	ch <- cpuSystemSecsDesc
	ch <- numprocsDesc
	ch <- readBytesDesc
	ch <- writeBytesDesc
	ch <- membytesDesc
	ch <- openFDsDesc
	ch <- worstFDRatioDesc
	ch <- startTimeDesc
	ch <- majorPageFaultsDesc
	ch <- minorPageFaultsDesc
	ch <- numThreadsDesc
	ch <- scrapeErrorsDesc
	ch <- scrapeProcReadErrorsDesc
	ch <- scrapePartialErrorsDesc
	ch <- threadCountDesc
	ch <- threadCpuSecsDesc
	ch <- threadIoBytesDesc
	ch <- threadMajorPageFaultsDesc
	ch <- threadMinorPageFaultsDesc
	ch <- ProcessStatus
	ch <- UnknownProcessDesc
}

// Collect implements prometheus.Collector.
func (p *NamedProcessCollector) Collect(ch chan<- prometheus.Metric) {
	req := scrapeRequest{results: ch, done: make(chan struct{})}
	p.scrapeChan <- req
	<-req.done
}

func (p *NamedProcessCollector) start() {
	for req := range p.scrapeChan {
		ch := req.results
		p.scrape(ch)
		req.done <- struct{}{}
	}
}

func (p *NamedProcessCollector) scrape(ch chan<- prometheus.Metric) {
	permErrs, groups, err := p.Update(p.source.AllProcs())
	p.scrapePartialErrors += permErrs.Partial
	if err != nil {
		p.scrapeErrors++
		log.Printf("error reading procs: %v", err)
	} else {
		for gname, gcounts := range groups {
			if gcounts.Allow {
				p.scrapeAllow(gname, gcounts, ch)
				for _, r := range procnames {
					if r != gname {
						ch <- prometheus.MustNewConstMetric(ProcessStatus,
							prometheus.GaugeValue, float64(gcounts.Procs), r, "down", "")

					}
				}
			} else {
				p.scrapeDeny(gname, gcounts, ch)
			}

		}
	}
	ch <- prometheus.MustNewConstMetric(scrapeErrorsDesc,
		prometheus.CounterValue, float64(p.scrapeErrors))
	ch <- prometheus.MustNewConstMetric(scrapeProcReadErrorsDesc,
		prometheus.CounterValue, float64(p.scrapeProcReadErrors))
	ch <- prometheus.MustNewConstMetric(scrapePartialErrorsDesc,
		prometheus.CounterValue, float64(p.scrapePartialErrors))
}

func (p *NamedProcessCollector) scrapeAllow(gname string, gcounts proc.Group, ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(ProcessStatus,
		prometheus.GaugeValue, float64(gcounts.Procs), gname, "up", strings.Join(gcounts.ProcessBash," "))

	ch <- prometheus.MustNewConstMetric(numprocsDesc,
		prometheus.GaugeValue, float64(gcounts.Procs), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(membytesDesc,
		prometheus.GaugeValue, float64(gcounts.Memory.ResidentBytes), gname, "resident", strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(membytesDesc,
		prometheus.GaugeValue, float64(gcounts.Memory.VirtualBytes), gname, "virtual", strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(startTimeDesc,
		prometheus.GaugeValue, float64(gcounts.OldestStartTime.Unix()), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(openFDsDesc,
		prometheus.GaugeValue, float64(gcounts.OpenFDs), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(worstFDRatioDesc,
		prometheus.GaugeValue, float64(gcounts.WorstFDratio), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(cpuUserSecsDesc,
		prometheus.CounterValue, gcounts.CPUUserTime, gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(cpuSystemSecsDesc,
		prometheus.CounterValue, gcounts.CPUSystemTime, gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(readBytesDesc,
		prometheus.CounterValue, float64(gcounts.ReadBytes), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(writeBytesDesc,
		prometheus.CounterValue, float64(gcounts.WriteBytes), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(majorPageFaultsDesc,
		prometheus.CounterValue, float64(gcounts.MajorPageFaults), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(minorPageFaultsDesc,
		prometheus.CounterValue, float64(gcounts.MinorPageFaults), gname, strings.Join(gcounts.ProcessBash," "))
	ch <- prometheus.MustNewConstMetric(numThreadsDesc,
		prometheus.GaugeValue, float64(gcounts.NumThreads), gname, strings.Join(gcounts.ProcessBash," "))
	if p.threads {
		for _, thr := range gcounts.Threads {
			ch <- prometheus.MustNewConstMetric(threadCountDesc,
				prometheus.GaugeValue, float64(thr.NumThreads),
				gname, thr.Name)
			ch <- prometheus.MustNewConstMetric(threadCpuSecsDesc,
				prometheus.CounterValue, float64(thr.CPUUserTime),
				gname, thr.Name, "user")
			ch <- prometheus.MustNewConstMetric(threadCpuSecsDesc,
				prometheus.CounterValue, float64(thr.CPUSystemTime),
				gname, thr.Name, "system")
			ch <- prometheus.MustNewConstMetric(threadIoBytesDesc,
				prometheus.CounterValue, float64(thr.ReadBytes),
				gname, thr.Name, "read")
			ch <- prometheus.MustNewConstMetric(threadIoBytesDesc,
				prometheus.CounterValue, float64(thr.WriteBytes),
				gname, thr.Name, "write")
			ch <- prometheus.MustNewConstMetric(threadMajorPageFaultsDesc,
				prometheus.CounterValue, float64(thr.MajorPageFaults),
				gname, thr.Name)
			ch <- prometheus.MustNewConstMetric(threadMinorPageFaultsDesc,
				prometheus.CounterValue, float64(thr.MinorPageFaults),
				gname, thr.Name)
		}
	}
}

func (p *NamedProcessCollector) scrapeDeny(gname string, gcounts proc.Group, ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(UnknownProcessDesc,
		prometheus.GaugeValue, 0, gname, strconv.Itoa(gcounts.ProcessId),
			gcounts.OldestStartTime.String(), strings.Join(gcounts.ProcessBash," "), strconv.Itoa(gcounts.Procs),
				strconv.Itoa(int(gcounts.NumThreads)), strconv.Itoa(int(gcounts.OpenFDs)), strconv.Itoa(int(gcounts.ReadBytes)),
					strconv.Itoa(int(gcounts.WriteBytes)))
}