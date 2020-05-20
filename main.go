package main

import (
	"bufio"
	"flag"
	"os"

	"github.com/gocolly/redisstorage"
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
	depth := flag.Int("d", 2, "depth of each collector")
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

	// Setting up storage
	redisURI := os.Getenv("REDIS_URI")
	if redisURI == "" {
		log.Fatal("You must set REDIS_URI env variable")
	}

	visitedStorage := &redisstorage.Storage{
		Address:  redisURI,
		Password: "",
		DB:       0,
		Prefix:   "0",
	}

	elasticURI, ok := os.LookupEnv("ELASTIC_URI")
	if !ok {
		logger.Error("You must set ELASTIC_URI env variable")
	}
	elasticIndex, ok := os.LookupEnv("ELASTIC_INDEX")
	if !ok {
		logger.Error("You must set ELASTIC_INDEX env variable")
	}
	pageStorage := &ElasticPageStorage{
		URI:        elasticURI,
		Index:      elasticIndex,
		BufferSize: 100,
		Logger:     logger,
	}

	jobsStorage := &MongoJobsStorage{
		DatabaseName:   os.Getenv("MONGO_DB"),
		CollectionName: os.Getenv("MONGO_COL"),
		Logger:         logger,
	}

	// Workers starter
	spider := &Spider{
		storage:     visitedStorage,
		jobsStorage: jobsStorage,
		pageStorage: pageStorage,
		numWorkers:  *numWorkers,
		parallelism: *parallelism,
		depth:       *depth,
		Logger:      logger,
	}
	spider.Init()
	if *blacklistFile != "" {
		blacklist, err := readLines(*blacklistFile)
		if err != nil {
			log.Fatal("Error while reading " + *blacklistFile)
		}
		spider.blacklist = blacklist
	}

	spider.Start()
}
