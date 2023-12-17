package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"sync"
	"syscall"
	"time"

	"github.com/gocolly/redisstorage"
	"github.com/kelseyhightower/envconfig"
	"github.com/samirettali/serviceregistry"
	"github.com/samirettali/tor-spider/spider"
	log "github.com/sirupsen/logrus"
)

type config struct {
	RedisURI          string `envconfig:"REDIS_URI" required:"true" split_words:"true"`
	BlacklistFile     string `envconfig:"BLACKLIST_FILE" required:"false" split_words:"true"`
	LogLevel          string `envconfig:"LOG_LEVEL" default:"error" split_words:"true"`
	LogStatusInterval int    `envconfig:"LOG_INTERVAL" default:"3" split_words:"true"`
}

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
	f, err := os.Create("./data/tor-spider.pprof")
	if err != nil {
		log.Fatal(err)
	}
	pprof.StartCPUProfile(f)
	defer pprof.StopCPUProfile()

	logger := log.New()
	logger.SetReportCaller(true)
	logger.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	var cfg config
	if err := envconfig.Process("", &cfg); err != nil {
		logger.Fatal(err)
	}

	logger.Infof("%+v", cfg)

	switch {
	case cfg.LogLevel == "error":
		logger.SetLevel(log.ErrorLevel)
	case cfg.LogLevel == "info":
		logger.SetLevel(log.InfoLevel)
	case cfg.LogLevel == "debug":
		logger.SetLevel(log.DebugLevel)
	default:
		logger.Fatalf("Invalid debug level: %s", cfg.LogLevel)
	}

	visitedStorage := &redisstorage.Storage{
		Address:  cfg.RedisURI,
		Password: "",
		DB:       0,
		Prefix:   "0",
	}

	// defer func() {
	// 	visitedStorage.Client.Close()
	// }()

	registry := serviceregistry.NewServiceRegistry(logger)

	// ElasticPageStorage configuration
	var elasticConfig spider.ElasticPageConfig
	err = envconfig.Process("ELASTIC_PAGE_STORAGE", &elasticConfig)

	if err != nil {
		logger.Fatal(err)
	}
	log.Infof("%+v\n", elasticConfig)

	elasticPageStorage, err := spider.NewElasticPageStorage(&elasticConfig)

	if err != nil {
		logger.Fatal(err)
	}

	elasticPageStorage.Logger = logger

	if err := registry.RegisterService(elasticPageStorage); err != nil {
		logger.Fatal(err)
	}

	// MongoJobsStorage configuration
	var mongoConfig spider.MongoJobsConfig
	err = envconfig.Process("MONGO_JOBS_STORAGE", &mongoConfig)

	if err != nil {
		logger.Fatal(err)
	}
	log.Infof("%+v\n", mongoConfig)

	mongoJobsStorage, err := spider.NewMongoJobsStorage(&mongoConfig)

	if err != nil {
		logger.Fatal(err)
	}

	mongoJobsStorage.Logger = logger

	if err := registry.RegisterService(mongoJobsStorage); err != nil {
		logger.Fatal(err)
	}

	// Spider configuration
	// TODO use generic interface as it's meant to be done. For now this will
	// do.
	var jobsStorage *spider.MongoJobsStorage
	if err := registry.FetchService(&jobsStorage); err != nil {
		logger.Fatal(err)
	}

	var pageStorage *spider.ElasticPageStorage
	if err := registry.FetchService(&pageStorage); err != nil {
		logger.Fatal(err)
	}

	var spiderConfig spider.SpiderConfig
	if err := envconfig.Process("SPIDER", &spiderConfig); err != nil {
		logger.Fatal(err)
	}
	log.Infof("%+v", spiderConfig)

	spiderConfig.JobsStorage = jobsStorage
	spiderConfig.PageStorage = pageStorage
	spiderConfig.VisitedStorage = visitedStorage

	if cfg.BlacklistFile != "" {
		blacklist, err := readLines(cfg.BlacklistFile)
		if err != nil {
			log.Fatal("Error while reading " + cfg.BlacklistFile)
		}
		spiderConfig.Blacklist = blacklist
	}

	spider, err := spider.NewSpider(&spiderConfig)

	if err != nil {
		logger.Fatal(err)
	}

	spider.Logger = logger

	if err := registry.RegisterService(spider); err != nil {
		logger.Fatal(err)
	}

	registry.StartAll()

	stop := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Second * time.Duration(cfg.LogStatusInterval))

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
