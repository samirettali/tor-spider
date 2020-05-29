package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gocolly/redisstorage"
	"github.com/samirettali/tor-spider/serviceregistry"
	"github.com/samirettali/tor-spider/spider"
	log "github.com/sirupsen/logrus"
)

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func main() {
	blacklistFile := flag.String("b", "", "blacklist file")
	depth := flag.Int("d", 1, "depth of each collector")
	verbose := flag.Bool("v", false, "verbose")
	debug := flag.Bool("x", false, "debug")
	numWorkers := flag.Int("w", 32, "number of workers")
	parallelism := flag.Int("p", 4, "parallelism of workers")
	flag.Parse()

	logger := log.New()
	// logger.SetReportCaller(true)
	logger.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	if *debug {
		logger.SetLevel(log.DebugLevel)
	} else if *verbose {
		logger.SetLevel(log.InfoLevel)
	}

	// Check env variables
	redisURI, ok := os.LookupEnv("REDIS_URI")
	if !ok {
		log.Fatal("You must set REDIS_URI env variable")
	}
	elasticURI, ok := os.LookupEnv("ELASTIC_URI")
	if !ok {
		logger.Error("You must set ELASTIC_URI env variable")
	}
	elasticIndex, ok := os.LookupEnv("ELASTIC_INDEX")
	if !ok {
		logger.Error("You must set ELASTIC_INDEX env variable")
	}
	mongoURI, ok := os.LookupEnv("MONGO_URI")
	if !ok {
		logger.Error("You must define MONGO_URI env variable")
	}
	mongoDB, ok := os.LookupEnv("MONGO_DB")
	if !ok {
		logger.Error("You must set MONGO_DB env variable")
	}
	mongoCol, ok := os.LookupEnv("MONGO_COL")
	if !ok {
		logger.Error("You must set MONGO_COL env variable")
	}

	proxyURI, ok := os.LookupEnv("PROXY_URI")
	if !ok {
		logger.Error("You must set PROXY_URI env variable")
	}
	proxyURL, err := url.Parse(proxyURI)
	if err != nil {
		log.Panic(err)
	}

	visitedStorage := &redisstorage.Storage{
		Address:  redisURI,
		Password: "",
		DB:       0,
		Prefix:   "0",
	}

	registry := serviceregistry.NewServiceRegistry()

	elasticPageStorage := spider.NewElasticPageStorage(elasticURI, elasticIndex, 100)
	elasticPageStorage.Logger = logger

	mongoJobsStorage := spider.NewMongoJobsStorage(mongoURI, mongoDB, mongoCol, 10000, *numWorkers)
	mongoJobsStorage.Logger = logger

	registry.RegisterService(elasticPageStorage)
	registry.RegisterService(mongoJobsStorage)

	// TODO use generic interface as it's meant to be done. For now this will
	// do.
	var js *spider.MongoJobsStorage
	if err2 := registry.FetchService(&js); err2 != nil {
		log.Panic(err2)
	}

	var ps *spider.ElasticPageStorage
	if err3 := registry.FetchService(&ps); err3 != nil {
		log.Panic(err3)
	}

	spider := &spider.Spider{
		Storage:     visitedStorage,
		JS:          js,
		PS:          ps,
		ProxyURL:    proxyURL,
		NumWorkers:  *numWorkers,
		Parallelism: *parallelism,
		Depth:       *depth,
		Logger:      logger,
	}

	if err := spider.Init(); err != nil {
		log.Fatalf("Spider ended with %v", err)
	}

	if *blacklistFile != "" {
		blacklist, err := readLines(*blacklistFile)
		if err != nil {
			log.Fatal("Error while reading " + *blacklistFile)
		}
		spider.Blacklist = blacklist
	}

	registry.RegisterService(spider)
	registry.StartAll()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT)
	// signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})

	ticker := time.NewTicker(time.Second * 3)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		<-stop
		registry.StopAll()
		close(done)
		wg.Done()
	}()

	for {
		select {
		case <-ticker.C:
			statuses := registry.Statuses()
			for kind, status := range statuses {
				logger.Infof("%s %s", kind, status)
			}
			fmt.Println()
		case <-done:
			statuses := registry.Statuses()
			fmt.Println("All services stopped")
			for kind, status := range statuses {
				logger.Infof("%s %s", kind, status)
			}
			wg.Wait()
			return
		}
	}
}
