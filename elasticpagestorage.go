package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
	log "github.com/sirupsen/logrus"
)

// ElasticPageStorage is an implementation of the PageStorage interface
type ElasticPageStorage struct {
	URI        string
	Index      string
	BufferSize int
	Logger     *log.Logger

	pages  chan PageInfo
	client *elasticsearch.Client
}

// Init initializes the connection to elastic search
func (e *ElasticPageStorage) Init() error {
	e.pages = make(chan PageInfo, e.BufferSize)
	retryBackoff := backoff.NewExponentialBackOff()
	var err error
	e.client, err = elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{
			e.URI,
		},
		// Retry on 429 TooManyRequests statuses
		RetryOnStatus: []int{502, 503, 504, 429},

		// Configure the backoff function
		RetryBackoff: func(i int) time.Duration {
			if i == 1 {
				retryBackoff.Reset()
			}
			return retryBackoff.NextBackOff()
		},
		MaxRetries: 5,
	})
	return err
}

// SavePage adds a page to the channel pages
func (e *ElasticPageStorage) SavePage(page PageInfo) error {
	select {
	case e.pages <- page:
		return nil
	default:
		err := e.flush()
		if err != nil {
			return err
		}
		e.pages <- page
		return nil
	}
}

func (e *ElasticPageStorage) flush() error {
	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Index:         e.Index,
		Client:        e.client,
		NumWorkers:    8,
		FlushBytes:    int(1000000),
		FlushInterval: 30 * time.Second,
	})

	if err != nil {
		return err
	}

	for i := 0; i < e.BufferSize; i++ {
		data, err := json.Marshal(<-e.pages)
		if err != nil {
			return err
		}
		err = bi.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Action: "index",
				Body:   bytes.NewReader(data),
			},
		)
		if err != nil {
			return err
		}
	}

	if err := bi.Close(context.Background()); err != nil {
		return err
	}

	biStats := bi.Stats()

	if biStats.NumFailed > 0 {
		msg := fmt.Sprintf("Failed to index %d documents", biStats.NumFailed)
		return &SavePageError{msg}
	}
	e.Logger.Infof("Saved %d pages", biStats.NumAdded)
	return nil
}
