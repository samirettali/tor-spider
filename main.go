package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gocolly/redisstorage"
	"github.com/kelseyhightower/envconfig"
	"github.com/samirettali/tor-spider/serviceregistry"
	"github.com/samirettali/tor-spider/spider"
	log "github.com/sirupsen/logrus"
)

type config struct {
	RedisURI      string `envconfig:"REDIS_URI"`
	ElasticURI    string `envconfig:"ELASTIC_URI"`
	ElasticIndex  string `envconfig:"ELASTIC_INDEX"`
	ProxyURI      string `envconfig:"PROXY_URI"`
	MongoURI      string `envconfig:"MONGO_URI"`
	MongoDB       string `envconfig:"MONGO_DB"`
	MongoCol      string `envconfig:"MONGO_COL"`
	LogLevel      string `envconfig:"LOG_LEVEL" default:"error"`
	BlacklistFile string `envconfig:"BLACKLIST_FILE" required:"false"`
	Depth         int    `envconfig:"DEPTH" default:"2"`
	Workers       int    `envconfig:"WORKERS" default:"32"`
	Parallelism   int    `envconfig:"PARALLELISM" default:"4"`
}

func loadConfig() (*config, error) {
	var cfg config
	err := envconfig.Process("", &cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
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
	logger := log.New()
	logger.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	config, err := loadConfig()
	if err != nil {
		logger.Fatal(err)
	}

	switch {
	case config.LogLevel == "error":
		logger.SetLevel(log.ErrorLevel)
	case config.LogLevel == "info":
		logger.SetLevel(log.InfoLevel)
	case config.LogLevel == "debug":
		logger.SetLevel(log.DebugLevel)
	}

	proxyURL, err := url.Parse(config.ProxyURI)
	if err != nil {
		log.Panic(err)
	}

	visitedStorage := &redisstorage.Storage{
		Address:  config.RedisURI,
		Password: "",
		DB:       0,
		Prefix:   "0",
	}

	registry := serviceregistry.NewServiceRegistry()

	elasticPageStorage := spider.NewElasticPageStorage(config.ElasticURI, config.ElasticIndex, 100)
	elasticPageStorage.Logger = logger

	mongoJobsStorage := spider.NewMongoJobsStorage(config.MongoURI, config.MongoDB, config.MongoCol, 10000, config.Workers)
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
		NumWorkers:  config.Workers,
		Parallelism: config.Parallelism,
		Depth:       config.Depth,
		Logger:      logger,
	}

	if err := spider.Init(); err != nil {
		log.Fatalf("Spider ended with %v", err)
	}

	if config.BlacklistFile != "" {
		blacklist, err := readLines(config.BlacklistFile)
		if err != nil {
			log.Fatal("Error while reading " + config.BlacklistFile)
		}
		spider.Blacklist = blacklist
	}

	registry.RegisterService(spider)
	registry.StartAll()

	stop := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Second * 10)

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
