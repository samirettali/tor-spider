package main

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

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
func (spider *Spider) Init() {
	spider.jobs = make(chan Job, spider.numWorkers*spider.parallelism*100)
	spider.results = make(chan PageInfo, 100)
	// defer storage.Client.Close()

	spider.startWebServer()
	spider.startJobsStorage()
	spider.pageStorage.Init()
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
		spider.jobs <- Job{URL}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Ok"))
		spider.Logger.Infof("Added %s to jobs queue", URL)
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

func (spider *Spider) getCollector(id string) (*colly.Collector, error) {
	c := colly.NewCollector(
		colly.MaxDepth(spider.depth),
		colly.Async(true),
		colly.IgnoreRobotsTxt(),
		colly.DisallowedDomains(
			spider.blacklist...,
		),
		colly.URLFilters(
			regexp.MustCompile("http://.+\\.onion.*"),
			regexp.MustCompile("https://.+\\.onion.*"),
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
	c.OnHTML("html", func(e *colly.HTMLElement) {
		result := PageInfo{
			URL:    e.Request.URL.String(),
			Body:   e.Text,
			Status: e.Response.StatusCode,
			Title:  e.ChildText("head.title"),
		}
		spider.pageStorage.SavePage(result)
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		spider.Logger.Debugf("Collector %s got %d for %s", id, r.StatusCode,
			r.Request.URL)
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		spider.Logger.Debugf("Collector %s error for %s: %s", id, r.Request.URL,
			err)
	})

	return c, nil
}

// Start starts the crawlers and logs messages
func (spider *Spider) Start() {
	go func() {
		id := 0
		sem := make(chan int, spider.numWorkers)
		for {
			sem <- 1
			go spider.crawl(strconv.Itoa(id), sem)
			id++
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		spider.Logger.Infof("There are %d jobs", len(spider.jobs))
	}
}

func (spider *Spider) crawl(id string, sem chan int) {
	defer func() {
		<-sem
	}()
	spider.Logger.Infof("Collector %s started", id)

	// Set up the collector
	c, err := spider.getCollector(id)
	if err != nil {
		spider.Logger.Error(err)
		return
	}

	// Get seed url and visit it
	for job := range spider.jobs {
		seed := job.URL
		spider.Logger.Debugf("Collector %s seeded with %s", id, seed)
		c.Visit(seed)
		// err := c.Visit(seed)
		// if err != nil {
		// 	spider.Logger.Error(err)
		// 	return
		// }
		c.Wait()
	}
}
