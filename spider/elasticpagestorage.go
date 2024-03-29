package spider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/esapi"
	"github.com/elastic/go-elasticsearch/v7/esutil"
	log "github.com/sirupsen/logrus"
)

// ElasticPageStorage is an implementation of the PageStorage interface
type ElasticPageStorage struct {
	Logger *log.Logger

	uri   string
	index string
	pages chan PageInfo
	done  chan struct{}
	wg    sync.WaitGroup

	client *elasticsearch.Client
}

// ElasticPageConfig is a struct that holds ElasticPageStorage configuration.
type ElasticPageConfig struct {
	URI        string `split_words:"true"`
	Index      string `split_words:"true"`
	BufferSize int    `split_words:"true"`
}

// NewElasticPageStorage returns a new ElasticPageStorage
func NewElasticPageStorage(config *ElasticPageConfig) (*ElasticPageStorage, error) {
	e := &ElasticPageStorage{
		uri:   config.URI,
		index: config.Index,
	}

	e.pages = make(chan PageInfo, config.BufferSize)
	e.done = make(chan struct{})
	err := e.initClient()

	if err != nil {
		return nil, err
	}

	return e, nil
}

// Start does nothing in this case
func (e *ElasticPageStorage) Start() {
	e.wg.Add(1)
	defer e.wg.Done()
}

// SavePage page adds a page to the channel and if it can't, it flushes it
func (e *ElasticPageStorage) SavePage(page PageInfo) {
	// TODO can it hang?
	select {
	case e.pages <- page:
		return
	default:
		go e.flush()
		select {
		case e.pages <- page:
		case <-time.After(3 * time.Second):
			e.Logger.Error("Cannot flush pages")
		}
	}
}

func (e *ElasticPageStorage) getBulkIndexer() (esutil.BulkIndexer, error) {
	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Index:         e.index,
		Client:        e.client,
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
func (e *ElasticPageStorage) flush() {
	bi, err := e.getBulkIndexer()

	if err != nil {
		e.Logger.Error(err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(30)*time.Second)
	defer cancel()
	for {
		select {
		case page := <-e.pages:
			data, err := json.Marshal(page)

			if err != nil {
				e.Logger.Error(err)
				return
			}

			err = bi.Add(
				ctx,
				esutil.BulkIndexerItem{
					Action: "index",
					Body:   bytes.NewReader(data),
				},
			)

			if err != nil {
				e.Logger.Error(err)
				return
			}

		default:
			if err := bi.Close(context.Background()); err != nil {
				e.Logger.Error(err)
				return
			}

			biStats := bi.Stats()

			if biStats.NumFailed > 0 {
				e.Logger.Errorf("Failed to index %d pages", biStats.NumFailed)
			}

			return
		}
	}
}

// Stop flushes all the pages
func (e *ElasticPageStorage) Stop() error {
	close(e.done)
	e.wg.Wait()
	// _, err := e.flush()
	// return err
	e.flush()
	return nil
}

// Status return the status
func (e *ElasticPageStorage) Status() string {
	var b strings.Builder
	b.Grow(40)

	select {
	case <-e.done:
		fmt.Fprintf(&b, "Stopped. ")
	default:
		fmt.Fprintf(&b, "Running. ")
	}

	count, err := e.count()

	if err != nil {
		fmt.Fprintf(&b, "Elastic is unreachable %v", err)
	} else {
		fmt.Fprintf(&b, "There are %d pages in Elastic", count)
	}

	fmt.Fprintf(&b, ", there are %d pages waiting to be saved", len(e.pages))

	return b.String()
}

func (e *ElasticPageStorage) count() (int, error) {
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
		Index: []string{e.index},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(30)*time.Second)
	defer cancel()
	resp, err := countRequest.Do(ctx, e.client.Transport)
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

func (e *ElasticPageStorage) initClient() error {
	var err error
	retryBackoff := backoff.NewExponentialBackOff()
	client, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{
			e.uri,
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

	_, err = client.Info()
	if err != nil {
		return err
	}

	e.client = client
	return nil
}
