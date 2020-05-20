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

	results := make(chan PageInfo, 1000)
	msgChan := make(chan string, 10)
	errChan := make(chan error, 10)
	debugChan := make(chan string, 10)

	if *debug {
		log.SetLevel(log.DebugLevel)
	} else if *verbose {
		log.SetLevel(log.InfoLevel)
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
		log.Error("You must set ELASTIC_URI env variable")
	}
	elasticIndex, ok := os.LookupEnv("ELASTIC_INDEX")
	if !ok {
		log.Error("You must set ELASTIC_INDEX env variable")
	}
	pageStorage := &ElasticPageStorage{
		URI:        elasticURI,
		Index:      elasticIndex,
		BufferSize: 100,
	}

	jobsStorage := &MongoJobsStorage{
		DatabaseName:   os.Getenv("MONGO_DB"),
		CollectionName: os.Getenv("MONGO_COL"),
	}

	// Workers starter
	spider := &Spider{
		storage:     visitedStorage,
		jobsStorage: jobsStorage,
		pageStorage: pageStorage,
		msgChan:     msgChan,
	}
	spider.Init(*numWorkers, *parallelism, *depth, results)
	spider.msgChan = msgChan
	spider.errChan = errChan
	spider.debugChan = debugChan
	if *blacklistFile != "" {
		blacklist, err := readLines(*blacklistFile)
		if err != nil {
			log.Fatal("Error while reading " + *blacklistFile)
		}
		spider.blacklist = blacklist
	}

	spider.Start()
}
