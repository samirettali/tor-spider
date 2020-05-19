package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
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

// Spider is
type Spider struct {
	numWorkers  int
	parallelism int
	depth       int
	blacklist   []string
	jobs        chan Job
	results     chan PageInfo
	errChan     chan error
	msgChan     chan string
	debugChan   chan string

	storage     storage.Storage
	jobsStorage JobsStorage
	pageStorage PageStorage
}

// JobsStorage is an interface which handles the storage of the jobs when it's
// channel is empty or full.
type JobsStorage interface {
	Init() error
	SaveJob(Job) error
	GetJob() (Job, error)
}

// type PageStorage interface {
// 	Init() error
// }

// Init initialized all the struct values
func (spider *Spider) Init(numWorkers int, parallelism int, depth int, results chan PageInfo) {

	spider.jobs = make(chan Job, numWorkers*parallelism*100)
	spider.depth = depth
	spider.results = results
	spider.numWorkers = numWorkers
	spider.parallelism = parallelism
	// defer storage.Client.Close()

	spider.startWebServer()
	spider.startJobsStorage()

	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
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
		spider.msgChan <- "Added " + URL + " to jobs queue"
	})
	go http.ListenAndServe(addr, nil)
	log.Info("Listening on " + addr)
}

func (spider *Spider) startJobsStorage() {
	err := spider.jobsStorage.Init()
	if err != nil {
		spider.errChan <- err
		return
	}

	delay := 50 * time.Millisecond

	go func() {
		for {
			if len(spider.jobs) < spider.numWorkers {
				job, err := spider.jobsStorage.GetJob()
				if err != nil {
					if _, ok := err.(*NoJobsError); ok {
						spider.debugChan <- "No jobs in Storage"
					} else {
						spider.errChan <- err
					}
				} else {
					spider.jobs <- job
					spider.debugChan <- fmt.Sprintf("Got Job %v", job)
				}
			} else {
				time.Sleep(delay)
			}
		}
	}()

	go func() {
		upperBound := int(float64(cap(spider.jobs)) * .5)
		for {
			if len(spider.jobs) > upperBound {
				job := <-spider.jobs
				err := spider.jobsStorage.SaveJob(job)
				if err != nil {
					spider.errChan <- err
				} else {
					spider.debugChan <- fmt.Sprintf("Saved Job %v", job)
				}
			} else {
				time.Sleep(delay)
			}
		}
	}()
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

	if os.Getenv("PROXY_URI") == "" {
		return nil, errors.New("You must set PROXY_URI env variable")
	}

	proxyURL, err := url.Parse(os.Getenv("PROXY_URI"))
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
		spider.results <- result
	})

	// Debug responses
	c.OnResponse(func(r *colly.Response) {
		msg := fmt.Sprintf("Collector %s got %d for %s", id, r.StatusCode,
			r.Request.URL)
		spider.debugChan <- msg
	})

	// Debug errors
	c.OnError(func(r *colly.Response, err error) {
		msg := fmt.Sprintf("Collector %s error for %s: %s", id, r.Request.URL,
			err)
		spider.debugChan <- msg
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
	for {
		select {
		case err := <-spider.errChan:
			log.Error(err)
		case msg := <-spider.msgChan:
			log.Info(msg)
		case msg := <-spider.debugChan:
			log.Debug(msg)
		case <-ticker.C:
			msg := fmt.Sprintf("There are %d jobs and %d results", len(spider.jobs),
				len(spider.results))
			log.Info(msg)
		}
	}
}

func (spider *Spider) crawl(id string, sem chan int) {
	defer func() {
		<-sem
	}()
	spider.msgChan <- fmt.Sprintf("Collector %s started", id)

	// Set up the collector
	c, err := spider.getCollector(id)
	if err != nil {
		spider.errChan <- err
		return
	}

	// Get seed url and visit it
	for job := range spider.jobs {
		seed := job.URL
		spider.debugChan <- fmt.Sprintf("Collector %s seeded with %s", id,
			seed)
		c.Visit(seed)
		c.Wait()
	}

}
