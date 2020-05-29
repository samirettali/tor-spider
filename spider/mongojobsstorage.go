package spider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TODO make it return two channels, one for input and one for output

// MongoJobsStorage is an implementation of the JobsStorage interface
type MongoJobsStorage struct {
	DatabaseName   string
	CollectionName string
	Logger         *log.Logger
	URI            string
	BufferSize     int

	jobs chan Job
	done chan struct{}
	wg   *sync.WaitGroup
	min  int
}

// NewMongoJobsStorage returns an instance
func NewMongoJobsStorage(URI string, dbName string, colName string, bufSize int, min int) *MongoJobsStorage {
	s := &MongoJobsStorage{
		URI:            URI,
		DatabaseName:   dbName,
		CollectionName: colName,
		jobs:           make(chan Job, bufSize),
	}
	s.done = make(chan struct{})
	s.min = min
	s.wg = &sync.WaitGroup{}
	return s
}

// Start starts the getter and the saver the collection
func (s *MongoJobsStorage) Start() {
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.getter()
	}()

	go func() {
		defer s.wg.Done()
		s.saver()
	}()
}

// Stop signals to the getter and the saver to stop
func (s *MongoJobsStorage) Stop() error {
	// TODO close jobs channel?
	close(s.done)
	s.wg.Wait()
	_, err := s.flush(cap(s.jobs))
	return err
}

// Status returns the status
func (s *MongoJobsStorage) Status() string {
	select {
	case <-s.done:
		return fmt.Sprintf("Stopped. There are %d cached jobs waiting to be saved", len(s.jobs))
	default:
		count, err := s.countJobsInDb()

		if err != nil {
			return fmt.Sprintf("Error %v", err)
		}

		return fmt.Sprintf("Running. There are %d jobs in the DB and %d cached jobs", count, len(s.jobs))
	}
}

func (s *MongoJobsStorage) countJobsInDb() (int64, error) {
	col, err := s.getCollectionClient()
	if err != nil {
		return 0, err
	}
	defer col.Database().Client().Disconnect(context.Background())
	count, err := col.CountDocuments(context.Background(), bson.D{})
	if err != nil {
		return 0, err
	}
	return count, nil

}

// GetJob returns a job
func (s *MongoJobsStorage) GetJob() (Job, error) {
	select {
	case job := <-s.jobs:
		return job, nil
	default:
		return Job{}, &NoJobsError{"No jobs"}
	}
}

// SaveJob saves a job
func (s *MongoJobsStorage) SaveJob(job Job) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case s.jobs <- job:
			return
		default:
			n := len(s.jobs) - s.min
			s.flush(n)
			s.jobs <- job
		}
	}()
}

func (s *MongoJobsStorage) getCollectionClient() (*mongo.Collection, error) {
	var client *mongo.Client
	var err error

	if client, err = mongo.NewClient(options.Client().ApplyURI(s.URI)); err != nil {
		return nil, err
	}

	if err = client.Connect(context.Background()); err != nil {
		return nil, err
	}

	db := client.Database(s.DatabaseName)
	return db.Collection(s.CollectionName), nil
}

func (s *MongoJobsStorage) getter() {
	for {
		select {
		case <-s.done:
			return
		default:
			if len(s.jobs) < s.min {
				n := s.fillCache()
				if n == 0 {
					time.Sleep(time.Second)
				}
			}
		}
	}
}

func (s *MongoJobsStorage) fillCache() int {
	var col *mongo.Collection
	var err error
	if col, err = s.getCollectionClient(); err != nil {
		s.Logger.Error(err)
		return 0
	}

	defer func() {
		if err := col.Database().Client().Disconnect(context.Background()); err != nil {
			s.Logger.Error(err)
		}
	}()

	findOptions := options.Find()
	findOptions.SetLimit(int64(s.min))
	cur, err := col.Find(context.Background(), bson.D{}, findOptions)

	if err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			s.Logger.Error(err)
		}
		return 0
	}

	count := 0
	for cur.Next(context.Background()) {
		var job Job
		err := cur.Decode(&job)
		if err != nil {
			s.Logger.Error(err)
		} else {
			count++

		}
		s.jobs <- job
		_, err = col.DeleteOne(context.Background(), job)
		if err != nil {
			s.Logger.Error(err)
		}
	}

	if err := cur.Err(); err != nil {
		log.Fatal(err)
	}
	cur.Close(context.Background())
	return count

}

// SaveJob adds a job to the jobs channel, upon checking if it's full
func (s *MongoJobsStorage) saver() {
	bound := int(float64(cap(s.jobs)) * .75)
	for {
		select {
		case <-s.done:
			return
		default:
			if len(s.jobs) > bound {
				_, err := s.flush(bound / 2)
				if err != nil {
					s.Logger.Error(err)
				}
			} else {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
}

// GetJobsChannel returns the channel on which jobs are sent and received from
// the storage
func (s *MongoJobsStorage) GetJobsChannel() chan Job {
	return s.jobs
}

func (s *MongoJobsStorage) flush(max int) (int, error) {
	jobs := make([]interface{}, 0)
loop:
	for i := 0; i < max; i++ {
		select {
		case job := <-s.jobs:
			jobs = append(jobs, job)
		default:
			break loop
		}
	}
	ctx := context.Background()
	col, err := s.getCollectionClient()
	if err != nil {
		// TODO save jobs
		return 0, err
	}
	result, err := col.InsertMany(ctx, jobs)
	if err != nil {
		return 0, err

	}
	inserted := len(result.InsertedIDs)
	if inserted != len(jobs) {
		s.Logger.Errorf("%d jobs were not saved", len(jobs)-inserted)
	}
	return inserted, nil
}
