package main

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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

// Spider is a struct that represents a Spider
type Spider struct {
	numWorkers  int
	parallelism int
	depth       int
	blacklist   []string
	jobs        chan Job
	results     chan PageInfo
	proxyURI    string
	collector   *colly.Collector

	storage     storage.Storage
	jobsStorage JobsStorage
	pageStorage PageStorage

	Logger *log.Logger
}

// JobsStorage is an interface which handles the storage of the jobs when it's
// channel is empty or full.
type JobsStorage interface {
	Init() error
	SaveJob(Job) error
	GetJob() (Job, error)
}

// PageStorage is an interface which handles tha storage of the visited pages
type PageStorage interface {
	Init() error
	SavePage(PageInfo) error
}

// Init initialized all the struct values
func (spider *Spider) Init() error {
	spider.jobs = make(chan Job, spider.numWorkers*spider.parallelism*100)
	spider.results = make(chan PageInfo, 100)
	c, err := spider.getCollector()
	if err != nil {
		return err
	}
	spider.collector = c
	// defer storage.Client.Close()

	spider.startWebServer()

	if err := spider.startJobsStorage(); err != nil {
		return err
	}

	if err := spider.pageStorage.Init(); err != nil {
		return err
	}

	return nil
}

func (spider *Spider) startWebServer() {
	// Web listener
	addr := ":8888"
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		URL := r.URL.Query().Get("url")
		if URL == "" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Missing url"))
			return
		}
		_, err := url.Parse(URL)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Invalid url"))
			return
		}
		c, err := spider.getInputCollector()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		c.Visit(URL)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Ok"))
		spider.Logger.Infof("Started InputCollector on %s", URL)
	})
	go http.ListenAndServe(addr, nil)
	log.Info("Listening on " + addr)
}

func (spider *Spider) startJobsStorage() error {
	err := spider.jobsStorage.Init()
	if err != nil {
		return err
	}

	delay := 50 * time.Millisecond

	go func() {
		lowerBound := int(float64(cap(spider.jobs)) * .15)
		for {
			if len(spider.jobs) < lowerBound {
				job, err := spider.jobsStorage.GetJob()
				if err != nil {
					if _, ok := err.(*NoJobsError); ok {
						spider.Logger.Debug("No jobs in storage")
					} else {
						spider.Logger.Error(err)
					}
				} else {
					spider.jobs <- job
					spider.Logger.Debugf("Got Job %v", job)
				}
			} else {
				time.Sleep(delay)
			}
		}
	}()

	go func() {
		upperBound := int(float64(cap(spider.jobs)) * .85)
		for {
			if len(spider.jobs) > upperBound {
				job := <-spider.jobs
				err := spider.jobsStorage.SaveJob(job)
				if err != nil {
					log.Error(err)
				}
			} else {
				time.Sleep(delay)
			}
		}
	}()
	return nil
}

func (spider *Spider) getCollector() (*colly.Collector, error) {
	disallowed := make([]*regexp.Regexp, len(spider.blacklist))
	for index, b := range spider.blacklist {
		disallowed[index] = regexp.MustCompile(b)
	}
	c := colly.NewCollector(
		colly.MaxDepth(spider.depth),
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

	proxyURL, err := url.Parse(spider.proxyURI)
	if err != nil {
		return nil, err
	}

	c.WithTransport(&http.Transport{
		Proxy: http.ProxyURL(proxyURL),
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
		Parallelism: spider.parallelism,
	})

	if err := c.SetStorage(spider.storage); err != nil {
		return nil, err
	}

	// Get all the links
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		foundURL := e.Request.AbsoluteURL(e.Attr("href"))
		if foundURL != "" && e.Request.Depth == spider.depth {
			spider.jobs <- Job{foundURL}
		} else {
			e.Request.Visit(foundURL)
		}
	})

	// Save result
	c.OnResponse(func(r *colly.Response) {
		body := string(r.Body)
		bodyReader := strings.NewReader(body)
		dom, err := goquery.NewDocumentFromReader(bodyReader)
		title := ""
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
		err = spider.pageStorage.SavePage(result)
		if err != nil {
			spider.Logger.Error(err)
		}
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

func (spider *Spider) getInputCollector() (*colly.Collector, error) {
	c := colly.NewCollector(
		colly.MaxDepth(3),
		colly.Async(true),
		colly.IgnoreRobotsTxt(),
	)

	c.MaxBodySize = 1000 * 1000

	extensions.RandomUserAgent(c)
	extensions.Referer(c)

	proxyURL, err := url.Parse(spider.proxyURI)
	if err != nil {
		return nil, err
	}

	c.WithTransport(&http.Transport{
		Proxy: http.ProxyURL(proxyURL),
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
		Parallelism: spider.parallelism,
	})

	// Get all the links
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		foundURL := e.Request.AbsoluteURL(e.Attr("href"))
		spider.jobs <- Job{foundURL}
		e.Request.Visit(foundURL)
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		spider.Logger.Debugf("InputCollector got %d for %s", r.StatusCode,
			r.Request.URL)
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		spider.Logger.Debugf("InputCollector error for %s: %s", r.Request.URL,
			err)
	})

	return c, nil
}

// Start starts the crawlers and logs messages
func (spider *Spider) Start() {
	sem := make(chan int, spider.numWorkers)
	go func() {
		for {
			sem <- 1
			job := <-spider.jobs
			go spider.crawl(job.URL, sem)
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		spider.Logger.Infof("There are %d jobs and %d collectors running", len(spider.jobs), len(sem))
	}
}

func (spider *Spider) crawl(seed string, sem chan int) {
	defer func() {
		<-sem
	}()

	c, err := spider.getCollector()
	if err != nil {
		spider.Logger.Error(err)
		return
	}

	err = c.Visit(seed)
	if err == nil {
		spider.Logger.Debugf("Collector started on %s", seed)
	}
	c.Wait()
}
