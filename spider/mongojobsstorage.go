package spider

import (
	"context"
	"fmt"
	"strings"
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

	jobs    chan Job
	done    chan struct{}
	filling chan struct{}
	wg      *sync.WaitGroup
	min     int
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
	s.filling = make(chan struct{}, 1)
	s.min = min
	s.wg = &sync.WaitGroup{}
	return s
}

// Start checks one time at a second if there are enough jobs in the channel, and if there are not it fetches them from the database
func (s *MongoJobsStorage) Start() {
	s.wg.Add(1)
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if len(s.jobs) < s.min {
				go s.fillJobsChannel()
			}
		}

	}
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
	var b strings.Builder
	b.Grow(40)

	select {
	case <-s.done:
		fmt.Fprint(&b, "Stopped. ")
	default:
		fmt.Fprint(&b, "Running. ")
	}
	fmt.Fprintf(&b, "There are %d cached jobs ", len(s.jobs))
	count, err := s.countJobsInDb()

	if err != nil {
		fmt.Fprintf(&b, "and db is unreachable %v", err)
	} else {
		fmt.Fprintf(&b, "and %d jobs in the db", count)
	}

	return b.String()
}

// GetJob returns a job if it's cached in the jobs channel, otherwise, it caches
// jobs from the database if there is no other goroutine doing it. It timeouts
// after 3 seconds.
func (s *MongoJobsStorage) GetJob() (Job, error) {
	select {
	case job := <-s.jobs:
		return job, nil
	case <-time.After(3 * time.Second):
		return Job{}, &NoJobsError{"No jobs"}
	}
}

// SaveJob adds a job to the channel if it's not full, otherwise it flushes the
// channel to the db and then adds the job to the channel.
func (s *MongoJobsStorage) SaveJob(job Job) {
	select {
	case s.jobs <- job:
		return
	default:
		n := len(s.jobs) - s.min
		s.flush(n)
		s.jobs <- job
		select {
		case s.jobs <- job:
		case <-time.After(3 * time.Second):
			s.Logger.Errorf("Cannot save Job %v", job)
		}
	}
}

func (s *MongoJobsStorage) getCollectionClient() (*mongo.Collection, error) {
	var client *mongo.Client
	var err error

	if client, err = mongo.NewClient(options.Client().ApplyURI(s.URI)); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(30)*time.Second)
	defer cancel()
	if err = client.Connect(ctx); err != nil {
		return nil, err
	}

	db := client.Database(s.DatabaseName)
	return db.Collection(s.CollectionName), nil
}

func (s *MongoJobsStorage) deleteJobsFromDb(jobs []string) (int64, error) {
	var col *mongo.Collection
	var err error

	if col, err = s.getCollectionClient(); err != nil {
		return 0, err
	}
	defer col.Database().Client().Disconnect(context.Background())

	filter := bson.M{"url": bson.M{"$in": jobs}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(30)*time.Second)
	defer cancel()

	res, err := col.DeleteMany(ctx, filter)

	if err != nil {
		return 0, err
	}

	return res.DeletedCount, nil
}

func (s *MongoJobsStorage) fillJobsChannel() {
	s.wg.Add(1)
	defer s.wg.Done()

	select {
	case s.filling <- struct{}{}:
		defer func() {
			<-s.filling
		}()
		s.Logger.Info("Started filler")

		cur, err := s.getCursor()

		if err != nil {
			s.Logger.Error(err)
			return
		}

		defer cur.Close(context.Background())

		jobs := make([]string, 0)

	loop:
		for cur.Next(context.Background()) {
			var job Job
			err := cur.Decode(&job)
			if err != nil {
				s.Logger.Error(err)
			} else {
				select {
				case s.jobs <- job:
					jobs = append(jobs, job.URL)
				default:
					break loop
				}
			}
		}

		if err := cur.Err(); err != nil {
			log.Error(err)
		}

		deletedCount, err := s.deleteJobsFromDb(jobs)

		if err != nil {
			s.Logger.Error(err)
			return
		}

		s.Logger.Infof("Got %d jobs, deleted %d jobs", len(jobs), deletedCount)
		s.Logger.Info("Ended filler")
	}
}

func (s *MongoJobsStorage) getCursor() (*mongo.Cursor, error) {
	var col *mongo.Collection
	var err error
	if col, err = s.getCollectionClient(); err != nil {
		return nil, err
	}

	defer col.Database().Client().Disconnect(context.Background())

	findOptions := options.Find()
	findOptions.SetLimit(int64(s.min))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(30)*time.Second)
	defer cancel()

	pipeline := []bson.M{bson.M{"$sample": bson.M{"size": s.min}}}
	cur, err := col.Aggregate(ctx, pipeline)

	if err != nil {
		return nil, err
	}

	return cur, nil
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

	if len(jobs) == 0 {
		return 0, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(30)*time.Second)
	defer cancel()
	col, err := s.getCollectionClient()
	if err != nil {
		// TODO save jobs
		return 0, err
	}
	defer col.Database().Client().Disconnect(context.Background())
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

func (s *MongoJobsStorage) countJobsInDb() (int64, error) {
	col, err := s.getCollectionClient()
	if err != nil {
		return 0, err
	}

	defer col.Database().Client().Disconnect(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(30)*time.Second)
	defer cancel()
	count, err := col.CountDocuments(ctx, bson.D{})
	if err != nil {
		return 0, err
	}
	return count, nil
}
