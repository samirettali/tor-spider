package spider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gocolly/colly"
	"github.com/gocolly/colly/extensions"
	"github.com/gocolly/colly/storage"
	tld "github.com/jpillora/go-tld"
	log "github.com/sirupsen/logrus"
)

// Job is a struct that represents a job
type Job struct {
	URL string
}

// PageInfo is a struct used to save the informations about a visited page
type PageInfo struct {
	URL    string
	Body   string
	Title  string
	Status int
}

// SpiderConfig is a struct that holds the spider configuration.
type SpiderConfig struct {
	NumWorkers     int             `split_words:"true"`
	Parallelism    int             `split_words:"true"`
	Depth          int             `split_words:"true"`
	Blacklist      []string        `split_words:"true"`
	ProxyURL       string          `split_words:"true"`
	VisitedStorage storage.Storage `split_words:"true"`
	PageStorage    PageStorage     `split_words:"true"`
	JobsStorage    JobsStorage     `split_words:"true"`
}

// PageStorage is an interface which handles tha storage of the visited pages
type PageStorage interface {
	SavePage(PageInfo)
}

// JobsStorage is an interface which handles tha storage of jobs
type JobsStorage interface {
	SaveJob(Job)
	GetJob() (Job, error)
	GetJobsChannel() chan Job
}

// Spider is a struct that represents a Spider
type Spider struct {
	Logger      *log.Logger
	jobsStorage JobsStorage
	pageStorage PageStorage
	blacklist   []string

	proxyURL       *url.URL
	wg             *sync.WaitGroup
	sem            chan struct{}
	onionSem       chan struct{}
	seedSem        chan struct{}
	done           chan struct{}
	onionCollector *colly.Collector
	seedCollector  *colly.Collector
}

// NewSpider returns a pointer to a spider
func NewSpider(config *SpiderConfig) (*Spider, error) {
	s := &Spider{
		blacklist:   config.Blacklist,
		sem:         make(chan struct{}, config.NumWorkers),
		onionSem:    make(chan struct{}, config.NumWorkers),
		seedSem:     make(chan struct{}, config.NumWorkers),
		done:        make(chan struct{}),
		wg:          &sync.WaitGroup{},
		pageStorage: config.PageStorage,
		jobsStorage: config.JobsStorage,
	}

	if err := s.initTorCollector(config); err != nil {
		return nil, err
	}

	if err := s.initSeedCollector(config); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Spider) initTorCollector(config *SpiderConfig) error {
	disallowed := make([]*regexp.Regexp, len(config.Blacklist))

	for index, b := range config.Blacklist {
		disallowed[index] = regexp.MustCompile(b)
	}

	c := colly.NewCollector(
		colly.MaxDepth(config.Depth),
		colly.Async(true),
		colly.IgnoreRobotsTxt(),
		colly.DisallowedURLFilters(
			disallowed...,
		),
		colly.URLFilters(
			regexp.MustCompile(`http://[a-zA-Z2-7]{16}\.onion.*`),
			regexp.MustCompile(`http://[a-zA-Z2-7]{56}\.onion.*`),
			regexp.MustCompile(`https://[a-zA-Z2-7]{16}\.onion.*`),
			regexp.MustCompile(`https://[a-zA-Z2-7]{56}\.onion.*`),
		),
	)

	// c.MaxBodySize = 1000 * 1000

	extensions.RandomUserAgent(c)
	extensions.Referer(c)

	proxyURL, err := url.Parse(config.ProxyURL)

	if err != nil {
		return err
	}

	c.WithTransport(&http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true,
	})

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: config.Parallelism,
	})

	if err := c.SetStorage(config.VisitedStorage); err != nil {
		return err
	}

	s.onionCollector = c

	return nil
}

func (s *Spider) initSeedCollector(config *SpiderConfig) error {
	c := colly.NewCollector(
		colly.MaxDepth(config.Depth),
		colly.Async(true),
		colly.IgnoreRobotsTxt(),
		colly.AllowURLRevisit(),
	)

	// c.MaxBodySize = 1000 * 1000

	extensions.RandomUserAgent(c)
	extensions.Referer(c)

	proxyURL, err := url.Parse(config.ProxyURL)

	if err != nil {
		return err
	}

	c.WithTransport(&http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   60 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true,
	})

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: config.Parallelism,
	})

	s.seedCollector = c

	return nil
}

func (s *Spider) startWebServer() {
	s.wg.Add(1)
	addr := ":8080"
	mux := http.NewServeMux()
	server := http.Server{Addr: addr, Handler: mux}

	mux.HandleFunc("/seed", s.seedHandler)
	mux.HandleFunc("/periodic", s.periodicJobHandler)

	go func() {
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Logger.Error(err)
		}
	}()

	go func() {
		<-s.done
		tc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server.Shutdown(tc)
		s.Logger.Debug("HTTP server shutdown")
		s.wg.Done()
	}()

	s.Logger.Infof("Listening on %s", addr)
}

func (s *Spider) seedHandler(w http.ResponseWriter, r *http.Request) {
	URL := r.URL.Query().Get("url")

	if URL == "" {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Missing url"))
		return
	}

	err := s.startSeedCollector(URL)

	if err != nil {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(err.Error()))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Ok"))
}

func (s *Spider) periodicJobHandler(w http.ResponseWriter, r *http.Request) {
	URL := r.URL.Query().Get("url")

	if URL == "" {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Missing url"))
		return
	}

	_, err := url.ParseRequestURI(URL)

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Invalid url"))
		return
	}

	interval := r.URL.Query().Get("interval")

	if interval == "" {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Missing interval"))
		return
	}

	integerInterval, err := strconv.Atoi(interval)

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Invalid interval"))
		return
	}

	durationInterval := time.Duration(integerInterval) * time.Second
	go s.startPeriodicCollector(URL, durationInterval)

	response := fmt.Sprintf("Added periodic job for %s with interval of %d seconds", URL, integerInterval)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(response))
}

func (s *Spider) getOnionCollector() *colly.Collector {
	c := s.onionCollector.Clone()

	// Get all the links
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		foundURL := e.Request.AbsoluteURL(e.Attr("href"))
		if foundURL != "" && e.Request.Depth == c.MaxDepth && isOnion(foundURL) {
			s.jobsStorage.SaveJob(Job{foundURL})
		} else {
			e.Request.Visit(foundURL)
		}
	})

	// Save result
	c.OnResponse(func(r *colly.Response) {
		title := ""
		body := string(r.Body)
		bodyReader := strings.NewReader(body)
		dom, err := goquery.NewDocumentFromReader(bodyReader)

		if err != nil {
			s.Logger.Error(err)
		} else {
			title = dom.Find("title").Contents().Text()
		}

		result := PageInfo{
			URL:    r.Request.URL.String(),
			Body:   body,
			Status: r.StatusCode,
			Title:  title,
		}

		s.pageStorage.SavePage(result)
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		s.Logger.Debugf("Got %d for %s", r.StatusCode,
			r.Request.URL)
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		s.Logger.Debugf("OnionCollector: %s while visiting %s", err, r.Request.URL)
	})

	return c
}

func (s *Spider) getSeedCollector() *colly.Collector {
	c := s.seedCollector.Clone()

	// Get all the links
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		foundURL := e.Request.AbsoluteURL(e.Attr("href"))
		if isOnion(foundURL) {
			s.jobsStorage.SaveJob(Job{foundURL})
		}
		e.Request.Visit(foundURL)
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		s.Logger.Debugf("SeedCollector got %d for %s", r.StatusCode,
			r.Request.URL)
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		s.Logger.Debugf("SeedCollector: %s while visiting %s", err, r.Request.URL)
	})

	return c
}

// Start starts the crawlers and logs messages
func (s *Spider) Start() {
	s.wg.Add(1)
	s.startWebServer()
	// TODO add wg here as it's a goroutine
	for {
		select {
		case s.sem <- struct{}{}:
			go s.startCollector()
		case <-s.done:
			s.wg.Done()
			return
		}
	}
}

// Stop signals to all goroutines to stop and waits for them to stop
func (s *Spider) Stop() error {
	close(s.done)
	s.wg.Wait()
	close(s.sem)
	return nil
}

// Status returns how many collector are running
func (s *Spider) Status() string {
	var b strings.Builder
	b.Grow(40)

	select {
	case <-s.done:
		fmt.Fprint(&b, "Stopped. ")
	default:
		fmt.Fprint(&b, "Running. ")
	}

	fmt.Fprintf(&b, "%d onion collectors running and %d seed collectors running", len(s.onionSem), len(s.seedSem))

	return b.String()
}

func isOnion(url string) bool {
	u, err := tld.Parse(url)
	return err == nil && u.TLD == "onion"
}

func (s *Spider) startCollector() {
	s.wg.Add(1)

	defer func() {
		s.wg.Done()
		<-s.sem
	}()

	c := s.getOnionCollector()

	for {
		select {
		case <-s.done:
			return
		case job := <-s.jobsStorage.GetJobsChannel():
			s.onionSem <- struct{}{}
			err := c.Visit(job.URL)
			if err != nil {
				s.Logger.Debugf("Onion collector %d error: %s", c.ID, err.Error())
			} else {
				s.Logger.Debugf("Onion collector %d started on %s", c.ID, job.URL)
			}
			c.Wait()
			if err == nil {
				s.Logger.Debugf("Onion collector %d ended on %s", c.ID, job.URL)
			}
			<-s.onionSem
		}
	}
}

func (s *Spider) startSeedCollector(url string) error {
	select {
	case s.seedSem <- struct{}{}:
		s.wg.Add(1)
		go func() {
			defer func() {
				s.wg.Done()
				<-s.seedSem
			}()
			c := s.getSeedCollector()

			if err := c.Visit(url); err != nil {
				s.Logger.Debugf("Seed collector: %s", err.Error())
			}

			s.Logger.Infof("Seed collector started on %s", url)
			c.Wait()
			s.Logger.Infof("Seed collector on %s ended", url)
		}()
	default:
		return errors.New("Maximum number of seed collectors running, try later")
	}
	return nil
}

func (s *Spider) startPeriodicCollector(url string, interval time.Duration) {
	s.wg.Add(1)
	defer s.wg.Done()

	ticker := time.NewTicker(interval)
	c := s.getSeedCollector()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			c.Visit(url)
			s.Logger.Infof("Periodic collector on %s started", url)
			c.Wait()
			s.Logger.Infof("Periodic collector on %s ended", url)
		}
	}
}
