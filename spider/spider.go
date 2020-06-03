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

// PageStorage is an interface which handles tha storage of the visited pages
type PageStorage interface {
	SavePage(PageInfo)
}

// JobsStorage is an interface which handles tha storage of the visited pages
type JobsStorage interface {
	SaveJob(Job)
	GetJob() (Job, error)
}

// Spider is a struct that represents a Spider
type Spider struct {
	NumWorkers  int
	Parallelism int
	Depth       int
	Blacklist   []string
	ProxyURL    *url.URL
	Logger      *log.Logger

	Storage storage.Storage
	JS      JobsStorage
	PS      PageStorage

	wg                    *sync.WaitGroup
	sem                   chan struct{}
	runningCollectors     chan struct{}
	runningSeedCollectors chan struct{}
	done                  chan struct{}
	torCollector          *colly.Collector
}

// Init initialized all the struct values
func (spider *Spider) Init() error {
	spider.sem = make(chan struct{}, spider.NumWorkers)
	spider.runningCollectors = make(chan struct{}, spider.NumWorkers)
	spider.runningSeedCollectors = make(chan struct{}, spider.NumWorkers)
	spider.done = make(chan struct{})
	spider.wg = &sync.WaitGroup{}

	err := spider.initCollector()

	if err != nil {
		return err
	}

	return nil
}

func (spider *Spider) initCollector() error {
	disallowed := make([]*regexp.Regexp, len(spider.Blacklist))

	for index, b := range spider.Blacklist {
		disallowed[index] = regexp.MustCompile(b)
	}

	c := colly.NewCollector(
		colly.MaxDepth(spider.Depth),
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

	c.MaxBodySize = 1000 * 1000

	extensions.RandomUserAgent(c)
	extensions.Referer(c)

	c.WithTransport(&http.Transport{
		Proxy: http.ProxyURL(spider.ProxyURL),
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
			// KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true,
	})

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: spider.Parallelism,
	})

	if err := c.SetStorage(spider.Storage); err != nil {
		return err
	}

	spider.torCollector = c

	return nil
}

func (spider *Spider) startWebServer() {
	addr := ":8080"
	m := http.NewServeMux()
	s := http.Server{Addr: addr, Handler: m}

	m.HandleFunc("/seed", spider.seedHandler)
	m.HandleFunc("/periodic", spider.periodicJobHandler)

	spider.wg.Add(1)

	go s.ListenAndServe()

	go func() {
		<-spider.done
		s.Shutdown(context.Background())
		spider.wg.Done()
	}()

	log.Infof("Listening on %s", addr)
}

func (spider *Spider) seedHandler(w http.ResponseWriter, r *http.Request) {
	URL := r.URL.Query().Get("url")

	if URL == "" {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Missing url"))
		return
	}

	err := spider.startSeedCollector(URL)

	if err != nil {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(err.Error()))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Ok"))
}

func (spider *Spider) periodicJobHandler(w http.ResponseWriter, r *http.Request) {
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
	go spider.startPeriodicCollector(URL, durationInterval)

	response := fmt.Sprintf("Added periodic job for %s with interval of %d seconds", URL, integerInterval)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(response))
}

func (spider *Spider) getCollector() (*colly.Collector, error) {
	c := spider.torCollector.Clone()

	// Get all the links
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		foundURL := e.Request.AbsoluteURL(e.Attr("href"))
		if foundURL != "" && e.Request.Depth == spider.Depth && isOnion(foundURL) {
			spider.JS.SaveJob(Job{foundURL})
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
			spider.Logger.Error(err)
		} else {
			title = dom.Find("title").Contents().Text()
		}

		result := PageInfo{
			URL:    r.Request.URL.String(),
			Body:   body,
			Status: r.StatusCode,
			Title:  title,
		}

		spider.PS.SavePage(result)
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		spider.Logger.Debugf("Got %d for %s", r.StatusCode,
			r.Request.URL)
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		spider.Logger.Debugf("Error while visiting %s: %v", r.Request.URL, err)
	})

	return c, nil
}

func (spider *Spider) getSeedCollector() (*colly.Collector, error) {
	c := colly.NewCollector(
		colly.MaxDepth(2),
		colly.Async(true),
		colly.IgnoreRobotsTxt(),
	)

	c.MaxBodySize = 1000 * 1000

	extensions.RandomUserAgent(c)
	extensions.Referer(c)

	c.WithTransport(&http.Transport{
		Proxy: http.ProxyURL(spider.ProxyURL),
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
			// KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true,
	})

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: spider.Parallelism,
	})

	// Get all the links
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		foundURL := e.Request.AbsoluteURL(e.Attr("href"))
		if isOnion(foundURL) {
			spider.JS.SaveJob(Job{foundURL})
		}
		e.Request.Visit(foundURL)
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		spider.Logger.Debugf("SeedCollector got %d for %s", r.StatusCode,
			r.Request.URL)
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		spider.Logger.Debugf("SeedCollector: %s", err)
	})

	return c, nil
}

// Start starts the crawlers and logs messages
func (spider *Spider) Start() {
	spider.startWebServer()
	// TODO add wg here as it's a goroutine
	for {
		select {
		case spider.sem <- struct{}{}:
			go spider.startCollector()
		case <-spider.done:
			spider.wg.Wait()
			return
		}
	}
}

// Stop signals to all goroutines to stop and waits for them to stop
func (spider *Spider) Stop() error {
	close(spider.done)
	spider.Logger.Infof("Spider is stopping")
	spider.wg.Wait()
	spider.Logger.Info("All goroutines ended, flushing data")
	return nil
}

// Status returns how many collector are running
func (spider *Spider) Status() string {
	var b strings.Builder
	b.Grow(40)

	select {
	case <-spider.done:
		fmt.Fprint(&b, "Stopped. ")
	default:
		fmt.Fprint(&b, "Running. ")
	}

	fmt.Fprintf(&b, "%dx%d collectors running and %dx%d seed collectors running", len(spider.runningCollectors), spider.Parallelism, len(spider.runningSeedCollectors),
		spider.Parallelism)

	return b.String()
}

func isOnion(url string) bool {
	u, err := tld.Parse(url)
	return err == nil && u.TLD == "onion"
}

func (spider *Spider) startCollector() {
	spider.wg.Add(1)

	defer func() {
		spider.wg.Done()
		<-spider.sem
	}()

	c, err := spider.getCollector()

	if err != nil {
		spider.Logger.Error(err)
		return
	}

	ticker := time.NewTicker(time.Second)

	for {
		select {
		case <-spider.done:
			return
		case <-ticker.C:
			job, err := spider.JS.GetJob()
			if err == nil {
				spider.runningCollectors <- struct{}{}
				err := c.Visit(job.URL)
				if err != nil {
					spider.Logger.Debugf("Collector %d error: %s", c.ID, err.Error())
				} else {
					spider.Logger.Debugf("Collector %d started on %s", c.ID, job.URL)
				}
				c.Wait()
				if err == nil {
					spider.Logger.Debugf("Collector %d ended on %s", c.ID, job.URL)
				}
				<-spider.runningCollectors
			}
		}
	}
}

func (spider *Spider) startSeedCollector(url string) error {
	select {
	case spider.runningSeedCollectors <- struct{}{}:
		go func() {
			spider.wg.Add(1)
			c, err := spider.getSeedCollector()
			if err != nil {
				spider.Logger.Error(err)
				return
			}
			c.Visit(url)
			spider.Logger.Infof("Seed collector started on %s", url)
			c.Wait()
			spider.Logger.Infof("Seed collector on %s ended", url)
			spider.wg.Done()
			<-spider.runningSeedCollectors
		}()
		return nil
	default:
		return errors.New("Maximum number of seed collectors running, try later")
	}
}

func (spider *Spider) startPeriodicCollector(url string, interval time.Duration) {
	spider.wg.Add(1)
	defer spider.wg.Done()

	ticker := time.NewTicker(interval)

	for {
		select {
		case <-spider.done:
			return
		case <-ticker.C:
			c, err := spider.getSeedCollector()
			if err != nil {
				spider.Logger.Error(err)
				break
			}
			c.Visit(url)
			spider.Logger.Infof("Periodic collector on %s started", url)
			c.Wait()
			spider.Logger.Infof("Periodic collector on %s ended", url)

		}
	}
}
