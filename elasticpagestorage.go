package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
)

// ElasticPageStorage is an implementation of the PageStorage interface
type ElasticPageStorage struct {
	client *elasticsearch.Client
}

// Init initializes the connection to elastic search
func (e *ElasticPageStorage) Init() error {
	retryBackoff := backoff.NewExponentialBackOff()
	elasticURI := os.Getenv("ELASTIC_URI")
	client, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{
			elasticURI,
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

	if err != nil {
		return err
	}

	e.client = client
	return nil
}

// Start starts the indexing process
func (e *ElasticPageStorage) Start(results <-chan PageInfo, msgChan chan<- string, errChan chan<- error) {
	chunkSize := int(float64(cap(results)) * .1)
	for {
		if len(results) >= chunkSize {
			// ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)

			bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
				Index:         "onions",
				Client:        e.client,
				NumWorkers:    8,
				FlushBytes:    int(1000000),
				FlushInterval: 30 * time.Second,
			})

			if err != nil {
				errChan <- err
			}

			var countSuccessful uint64
			for i := 0; i < chunkSize; i++ {
				data, err := json.Marshal(<-results)
				if err != nil {
					errChan <- err
				}
				err = bi.Add(
					context.Background(),
					esutil.BulkIndexerItem{
						Action: "index",
						Body:   bytes.NewReader(data),

						// OnSuccess is called for each successful operation
						OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem) {
							atomic.AddUint64(&countSuccessful, 1)
						},

						// OnFailure is called for each failed operation
						OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
							if err != nil {
								errChan <- err
							} else {
								errChan <- err
							}
						},
					},
				)
				if err != nil {
					errChan <- err
				}
			}

			if err := bi.Close(context.Background()); err != nil {
				errChan <- err
			}

			biStats := bi.Stats()

			if biStats.NumFailed > 0 {
				errMsg := fmt.Sprintf("Failed to index %d documents", biStats.NumFailed)
				errChan <- errors.New(errMsg)
			} else {
				msgChan <- fmt.Sprintf("Indexed %d documents", countSuccessful)
			}
		}
	}
}
