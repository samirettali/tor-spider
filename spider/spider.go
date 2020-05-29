package spider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gocolly/colly"
	"github.com/gocolly/colly/extensions"
	"github.com/gocolly/colly/storage"
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
	// SaveJob(Job)
	// GetJob() (Job, error)
	GetJobsChannel() chan Job
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

	wg           *sync.WaitGroup
	sem          chan struct{}
	done         chan struct{}
	torCollector *colly.Collector
	jobs         chan Job
}

// Init initialized all the struct values
func (spider *Spider) Init() error {
	spider.sem = make(chan struct{}, spider.NumWorkers)
	spider.done = make(chan struct{})
	spider.jobs = spider.JS.GetJobsChannel()
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
	// Web listener
	m := http.NewServeMux()
	addr := ":8080"
	s := http.Server{Addr: addr, Handler: m}
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		URL := r.URL.Query().Get("url")
		if URL == "" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Missing url"))
			return
		}
		c, err := spider.getSeedCollector()
		if err != nil {
			spider.Logger.Error(err)
			return
		}
		spider.wg.Add(1)
		go func() {
			defer spider.wg.Done()
			c.Visit(URL)
			spider.Logger.Infof("Seed collector started on %s", URL)
			c.Wait()
			spider.Logger.Infof("Seed collector on %s ended", URL)
		}()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Ok"))
	})
	spider.wg.Add(2)
	go s.ListenAndServe()
	go func() {
		<-spider.done
		s.Shutdown(context.Background())
		spider.wg.Done()
		spider.Logger.Infof("Shutdown web server")
		spider.wg.Done()
	}()
	log.Infof("Listening on %s", addr)
}

func (spider *Spider) getCollector() (*colly.Collector, error) {
	c := spider.torCollector.Clone()

	// Get all the links
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		foundURL := e.Request.AbsoluteURL(e.Attr("href"))
		// e.Request.Visit(foundURL)
		if foundURL != "" && e.Request.Depth == spider.Depth {
			spider.jobs <- Job{foundURL}
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
		spider.jobs <- Job{foundURL}
		// spider.JS.SaveJob(Job{foundURL})
		e.Request.Visit(foundURL)
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		spider.Logger.Debugf("SeedCollector got %d for %s", r.StatusCode,
			r.Request.URL)
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		spider.Logger.Errorf("SeedCollector: %s", err)
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
			spider.wg.Add(1)
			spider.startCollector()
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
	return fmt.Sprintf("%dx%d collectors running",
		len(spider.sem), spider.Parallelism)
}

func (spider *Spider) startCollector() {
	go func() {
		defer func() {
			spider.wg.Done()
			<-spider.sem
		}()
		c, err := spider.getCollector()

		if err != nil {
			spider.Logger.Error(err)
			return
		}

		for {
			select {
			case job := <-spider.jobs:

				c.Visit(job.URL)
				err = c.Visit(job.URL)
				if err != nil {
					spider.Logger.Debug(err)
					// continue
					// return
				}

				spider.Logger.Debugf("Collector %d started on %s", c.ID, job.URL)
				c.Wait()
				spider.Logger.Debugf("Collector %d ended on %s", c.ID, job.URL)
			case <-spider.done:
				return
			}
		}
	}()
}
