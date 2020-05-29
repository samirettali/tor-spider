package spider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/elastic/go-elasticsearch/v8/esapi"

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

	pages chan PageInfo
	done  chan struct{}
	wg    sync.WaitGroup
}

// NewElasticPageStorage returns a new ElasticPageStorage
func NewElasticPageStorage(uri string, index string, bufferSize int) *ElasticPageStorage {
	e := &ElasticPageStorage{
		URI:        uri,
		Index:      index,
		BufferSize: bufferSize,
	}
	e.pages = make(chan PageInfo, e.BufferSize)
	e.done = make(chan struct{})
	return e
}

// Start does nothing in this case
func (e *ElasticPageStorage) Start() {
	e.wg.Add(1)
	defer e.wg.Done()
	return
}

// SavePage page adds a page to the channel and if it can't, it flushes it
func (e *ElasticPageStorage) SavePage(page PageInfo) {
	// TODO can it hang?
	select {
	case e.pages <- page:
		return
	default:
		_, err := e.flush()
		if err != nil {
			e.Logger.Error(err)
		}
		e.pages <- page
	}
}

func (e *ElasticPageStorage) getBulkIndexer() (esutil.BulkIndexer, error) {
	client, err := e.getClient()

	if err != nil {
		return nil, err
	}

	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Index:         e.Index,
		Client:        client,
		NumWorkers:    8,
		FlushBytes:    int(1000000),
		FlushInterval: 30 * time.Second,
	})

	if err != nil {
		return nil, err
	}

	return bi, nil
}

// Flush flushes the page queue
func (e *ElasticPageStorage) flush() (uint64, error) {
	bi, err := e.getBulkIndexer()

	if err != nil {
		return 0, err
	}

	for {
		select {
		case page := <-e.pages:
			data, err := json.Marshal(page)

			if err != nil {
				return 0, err
			}

			err = bi.Add(
				context.Background(),
				esutil.BulkIndexerItem{
					Action: "index",
					Body:   bytes.NewReader(data),
				},
			)

			if err != nil {
				return 0, err
			}

		default:
			if err := bi.Close(context.Background()); err != nil {
				return 0, err
			}

			biStats := bi.Stats()

			if biStats.NumFailed > 0 {
				msg := fmt.Sprintf("Failed to index %d documents", biStats.NumFailed)
				return 0, &SavePageError{msg}
			}
			return biStats.NumAdded, nil
		}
	}
}

// Stop flushes all the pages
func (e *ElasticPageStorage) Stop() error {
	close(e.done)
	e.wg.Wait()
	_, err := e.flush()
	return err
}

// Status return the status
func (e *ElasticPageStorage) Status() string {
	s := ""
	select {
	case <-e.done:
		s += fmt.Sprintf("Stopped. ")
	default:
		s += "Running. "
		count, err := e.count()
		if err != nil {
			s += fmt.Sprintf("Elastic is unreachable %v", err)
		} else {
			s += fmt.Sprintf("There are %d pages in Elastic", count)
		}
	}
	s += fmt.Sprintf(", there are %d pages waiting to be saved", len(e.pages))
	return s
}

func (e *ElasticPageStorage) count() (int, error) {
	client, err := e.getClient()
	if err != nil {
		return 0, err
	}
	type countResponse struct {
		Count  int `json:"count"`
		Shards struct {
			Total     int `json:"total"`
			Succesful int `json:"successful"`
			Skipped   int `json:"skipped"`
			Failed    int `json:"failed"`
		} `json:"_shards"`
	}
	countRequest := &esapi.CountRequest{
		Index: []string{e.Index},
	}
	resp, err := countRequest.Do(context.Background(), client.Transport)
	if err != nil {
		return 0, err
	}
	result := countResponse{}
	dec := json.NewDecoder(resp.Body)

	if err := dec.Decode(&result); err != nil {
		return 0, err
	}
	return result.Count, nil
}

func (e *ElasticPageStorage) getClient() (*elasticsearch.Client, error) {
	var err error
	retryBackoff := backoff.NewExponentialBackOff()
	client, err := elasticsearch.NewClient(elasticsearch.Config{
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

	if err != nil {
		return nil, err
	}

	_, err = client.Info()
	if err != nil {
		return nil, err
	}
	return client, nil
}
